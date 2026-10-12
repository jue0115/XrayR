package controller

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	"github.com/XrayR-project/XrayR/common/limiter"
	"github.com/XrayR-project/XrayR/common/rule"
	"github.com/xtls/xray-core/features/stats"
)

func TestMemoryCompareUserLists(t *testing.T) {
	user := api.UserInfo{UID: 1, Email: "user@example.invalid", UUID: "old-uuid", SpeedLimit: 100, DeviceLimit: 3}
	other := api.UserInfo{UID: 2, Email: "other@example.invalid"}
	changed := user
	changed.SpeedLimit = 200
	for _, test := range []struct {
		name                      string
		old, next, deleted, added []api.UserInfo
	}{
		{"empty", nil, nil, nil, nil},
		{"reordered", []api.UserInfo{user, other}, []api.UserInfo{other, user}, nil, nil},
		{"removed", []api.UserInfo{user, other}, []api.UserInfo{user}, []api.UserInfo{other}, nil},
		{"added", []api.UserInfo{user}, []api.UserInfo{user, other}, nil, []api.UserInfo{other}},
		{"updated", []api.UserInfo{user}, []api.UserInfo{changed}, []api.UserInfo{user}, []api.UserInfo{changed}},
		{"duplicate", []api.UserInfo{user, user}, []api.UserInfo{user, other, other}, nil, []api.UserInfo{other}},
	} {
		t.Run(test.name, func(t *testing.T) {
			deleted, added := compareUserList(&test.old, &test.next)
			if !reflect.DeepEqual(deleted, test.deleted) || !reflect.DeepEqual(added, test.added) {
				t.Fatalf("unexpected diff: deleted=%v added=%v", deleted, added)
			}
		})
	}
}

func TestMemoryPruneUserWarnings(t *testing.T) {
	user := api.UserInfo{UID: 1, Email: "user@example.invalid", SpeedLimit: 100}
	removed := api.UserInfo{UID: 2, Email: "removed@example.invalid"}
	changed := user
	changed.SpeedLimit = 200
	c := &Controller{warnedUsers: map[api.UserInfo]int{user: 2, removed: 1}}
	users := []api.UserInfo{user}
	c.pruneUserWarnings(&users)
	if len(c.warnedUsers) != 1 || c.warnedUsers[user] != 2 {
		t.Fatal("did not retain unchanged warnings and remove deleted users")
	}
	users[0] = changed
	c.pruneUserWarnings(&users)
	if len(c.warnedUsers) != 0 {
		t.Fatal("old parameter variant retained")
	}
	c.warnedUsers[changed]++
	if c.warnedUsers[changed] != 1 {
		t.Fatal("new warning cannot be recorded")
	}
}

type memoryNodeAPI struct {
	api.API
	users *[]api.UserInfo
	err   error
}

func (*memoryNodeAPI) GetNodeInfo() (*api.NodeInfo, error) {
	return nil, errors.New(api.NodeNotModified)
}

func (a *memoryNodeAPI) GetUserList() (*[]api.UserInfo, error) { return a.users, a.err }

func TestMemoryUserRefreshPrunesWarnings(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			c, _, user, _, _ := newTrafficTestController(t)
			users := []api.UserInfo{user}
			c.userList = &users
			c.nodeInfo = &api.NodeInfo{Port: 12345}
			c.config.DisableGetRule = true
			c.warnedUsers = map[api.UserInfo]int{user: 2, {UID: 2, Email: "removed@example.invalid"}: 1}
			client := &memoryNodeAPI{users: &users}
			if fail {
				client.err = errors.New("simulated panel failure")
			}
			c.apiClient = client
			if err := c.nodeInfoMonitor(); err != nil {
				t.Fatal(err)
			}
			want := 1
			if fail {
				want = 2
			}
			if len(c.warnedUsers) != want || c.warnedUsers[user] != 2 {
				t.Fatal("refresh cleared current warnings or failed to clear stale ones")
			}
		})
	}
}

type memoryMonitorAPI struct{ *trafficTestAPI }

func (*memoryMonitorAPI) ReportNodeStatus(*api.NodeStatus) error { return nil }

type memoryCountingStats struct {
	stats.Manager
	reads int
}

func (s *memoryCountingStats) GetCounter(name string) stats.Counter {
	s.reads++
	return s.Manager.GetCounter(name)
}

func TestMemoryTrafficMonitorSingleSnapshot(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			c, client, user, _, down := newTrafficTestController(t)
			users := []api.UserInfo{user}
			c.userList = &users
			c.config.UpdatePeriodic = 60
			c.config.AutoSpeedLimitConfig = &AutoSpeedLimitConfig{Limit: 1, WarnTimes: 2, LimitSpeed: 1, LimitDuration: 1}
			c.warnedUsers = map[api.UserInfo]int{user: 2}
			c.limitedUsers = make(map[api.UserInfo]LimitInfo)
			l := limiter.New()
			defer l.Close()
			if err := l.AddInboundLimiter(c.Tag, 0, &users, nil); err != nil {
				t.Fatal(err)
			}
			c.dispatcher = &mydispatcher.DefaultDispatcher{Limiter: l, RuleManager: rule.New()}
			defer c.dispatcher.RuleManager.Close()
			c.apiClient = &memoryMonitorAPI{client}
			counting := &memoryCountingStats{Manager: c.stm}
			c.stm = counting
			down.Add(5 << 30)
			reports := 0
			client.report = func(traffic *[]api.UserTraffic) error {
				reports++
				if len(*traffic) != 1 || (*traffic)[0].Download != 5<<30 {
					t.Fatal("changed report bytes")
				}
				down.Add(50)
				if fail {
					return errors.New("simulated acknowledgement failure")
				}
				return nil
			}
			if err := c.userInfoMonitor(); err != nil {
				t.Fatal(err)
			}
			if reports != 1 || counting.reads != 2 {
				t.Fatalf("reports=%d counter reads=%d", reports, counting.reads)
			}
			want := int64(50)
			if fail {
				want += 5 << 30
			}
			if down.Value() != want {
				t.Fatalf("unacknowledged/live bytes changed: %d, want %d", down.Value(), want)
			}
			if len(c.warnedUsers) != 0 || len(c.limitedUsers) != 1 {
				t.Fatal("auto speed limit behavior changed")
			}
		})
	}
}

func BenchmarkMemoryCompareUserList(b *testing.B) {
	users := make([]api.UserInfo, 6400)
	for i := range users {
		users[i] = api.UserInfo{UID: i + 1, Email: fmt.Sprintf("user%d@example.invalid", i), UUID: "synthetic-uuid", DeviceLimit: 3}
	}
	b.ReportAllocs()
	for b.Loop() {
		compareUserList(&users, &users)
	}
}
