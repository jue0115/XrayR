package controller

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	"github.com/XrayR-project/XrayR/common/limiter"
	"github.com/XrayR-project/XrayR/common/rule"
	log "github.com/sirupsen/logrus"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/vless"
)

type lifecycleUsers struct {
	proxy.Inbound
	proxy.UserManager
	users      map[string]*protocol.MemoryUser
	failRemove map[string]int
	failAdd    map[string]int
	removed    []string
	added      []string
}

func (m *lifecycleUsers) GetUser(_ context.Context, email string) *protocol.MemoryUser {
	return m.users[email]
}

func (m *lifecycleUsers) RemoveUser(_ context.Context, email string) error {
	m.removed = append(m.removed, email)
	if m.failRemove[email] > 0 {
		m.failRemove[email]--
		return errors.New("simulated auth removal failure")
	}
	delete(m.users, email)
	return nil
}

func (m *lifecycleUsers) AddUser(_ context.Context, user *protocol.MemoryUser) error {
	m.added = append(m.added, user.Email)
	if m.failAdd[user.Email] > 0 {
		m.failAdd[user.Email]--
		return errors.New("simulated auth addition failure")
	}
	if m.users[user.Email] != nil {
		return errors.New("duplicate auth user")
	}
	m.users[user.Email] = user
	return nil
}

type lifecycleInbound struct {
	inbound.Handler
	users *lifecycleUsers
}

func (h *lifecycleInbound) GetInbound() proxy.Inbound { return h.users }

type lifecycleInbounds struct {
	inbound.Manager
	h inbound.Handler
}

func (m *lifecycleInbounds) GetHandler(context.Context, string) (inbound.Handler, error) {
	return m.h, nil
}

type lifecycleAPI struct {
	*trafficTestAPI
	lists   []*[]api.UserInfo
	call    int
	failAll bool
}

func (*lifecycleAPI) GetNodeInfo() (*api.NodeInfo, error) {
	return nil, errors.New(api.NodeNotModified)
}

func (a *lifecycleAPI) GetUserList() (*[]api.UserInfo, error) {
	call := a.call
	a.call++
	if call < len(a.lists) && a.lists[call] != nil {
		return a.lists[call], nil
	}
	return nil, errors.New(api.UserNotModified)
}

func (a *lifecycleAPI) failure() error {
	if a.failAll {
		return errors.New("simulated panel failure")
	}
	return nil
}

func (a *lifecycleAPI) ReportNodeStatus(*api.NodeStatus) error        { return a.failure() }
func (a *lifecycleAPI) ReportNodeOnlineUsers(*[]api.OnlineUser) error { return a.failure() }
func (a *lifecycleAPI) ReportIllegal(*[]api.DetectResult) error       { return a.failure() }

func newLifecycleController(t *testing.T, users []api.UserInfo) (*Controller, *lifecycleAPI, *lifecycleUsers) {
	t.Helper()
	c, trafficAPI, _, _, _ := newTrafficTestController(t)
	c.config.DisableGetRule = true
	c.config.AutoSpeedLimitConfig = &AutoSpeedLimitConfig{}
	c.nodeInfo = &api.NodeInfo{Port: 12345, NodeType: "Vless"}
	c.userList = &users
	l := limiter.New()
	t.Cleanup(func() { l.Close() })
	if err := l.AddInboundLimiter(c.Tag, 0, &users, nil); err != nil {
		t.Fatal(err)
	}
	c.dispatcher = &mydispatcher.DefaultDispatcher{Limiter: l, RuleManager: rule.New()}
	t.Cleanup(func() { c.dispatcher.RuleManager.Close() })
	client := &lifecycleAPI{trafficTestAPI: trafficAPI}
	c.apiClient = client
	manager := &lifecycleUsers{users: make(map[string]*protocol.MemoryUser), failRemove: make(map[string]int), failAdd: make(map[string]int)}
	for _, user := range c.buildVlessUser(&users) {
		memory, err := user.ToMemoryUser()
		if err != nil {
			t.Fatal(err)
		}
		manager.users[user.Email] = memory
	}
	c.ibm = &lifecycleInbounds{h: &lifecycleInbound{users: manager}}
	return c, client, manager
}

func lifecycleUser(uid int, email string) api.UserInfo {
	return api.UserInfo{UID: uid, Email: email, UUID: "90e4903e-66a4-45f7-abda-fb5433f4820f"}
}

func TestLifecycleRemovalFailureRetriesOn304(t *testing.T) {
	users := []api.UserInfo{lifecycleUser(1, "one@example.invalid"), lifecycleUser(2, "two@example.invalid"), lifecycleUser(3, "three@example.invalid")}
	c, client, manager := newLifecycleController(t, users)
	empty := []api.UserInfo{}
	client.lists = []*[]api.UserInfo{&empty}
	key := c.buildUserTag(&users[0])
	manager.failRemove[key] = 1
	reports := make(map[int]int)
	client.report = func(traffic *[]api.UserTraffic) error {
		for _, user := range *traffic {
			reports[user.UID]++
		}
		return nil
	}
	for i := range users {
		counter, err := c.stm.GetOrRegisterCounter("user>>>" + c.buildUserTag(&users[i]) + ">>>traffic>>>downlink")
		if err != nil {
			t.Fatal(err)
		}
		counter.Add(100)
	}
	c.nodeInfoMonitor()
	if len(manager.removed) != 3 || len(manager.users) != 1 || len(*c.userList) != 1 || c.pendingUserList == nil {
		t.Fatal("one failed removal skipped others or falsely committed the target")
	}
	if reports[1] != 0 || reports[2] != 1 || reports[3] != 1 {
		t.Fatal("traffic flushed before auth removal, or successful deletions not flushed", reports)
	}
	c.nodeInfoMonitor() // HTTP 304 must still retry the one failed operation.
	if len(manager.users) != 0 || len(*c.userList) != 0 || c.pendingUserList != nil || len(manager.removed) != 4 {
		t.Fatal("unfinished removal was lost on HTTP 304")
	}
	c.nodeInfoMonitor()
	if len(manager.removed) != 4 || reports[1] != 1 || reports[2] != 1 || reports[3] != 1 {
		t.Fatal("successful removals/reports were repeated", reports)
	}
	if _, _, _, down := c.getTraffic(key); down != nil {
		t.Fatal("deleted counter retained")
	}
	if done, err := c.removeUsers([]string{key}, c.Tag); err != nil || len(done) != 1 {
		t.Fatal("missing auth user is not an idempotent success")
	}
}

func TestLifecyclePartialAdditionAndLatestTarget(t *testing.T) {
	one, two := lifecycleUser(1, "one@example.invalid"), lifecycleUser(2, "two@example.invalid")
	for _, withdraw := range []bool{false, true} {
		c, client, manager := newLifecycleController(t, nil)
		requested := []api.UserInfo{one, two}
		client.lists = []*[]api.UserInfo{&requested}
		manager.failAdd[c.buildUserTag(&two)] = 1
		c.nodeInfoMonitor()
		if len(manager.users) != 1 || len(*c.userList) != 1 || c.pendingUserList == nil {
			t.Fatal("partial addition was not tracked")
		}
		if withdraw {
			empty := []api.UserInfo{}
			client.lists = append(client.lists, &empty)
		}
		c.nodeInfoMonitor()
		want := 2
		if withdraw {
			want = 0
		}
		if len(manager.users) != want || len(*c.userList) != want || c.pendingUserList != nil {
			t.Fatalf("latest target not applied: withdraw=%t auth=%d list=%d", withdraw, len(manager.users), len(*c.userList))
		}
		if !withdraw && len(manager.added) != 3 {
			t.Fatal("retry re-added already committed auth users")
		}
	}
}

func TestLifecycleParameterRetryPreservesCountersAndFlow(t *testing.T) {
	old := lifecycleUser(1, "one@example.invalid")
	old.SpeedLimit = 100
	c, client, manager := newLifecycleController(t, []api.UserInfo{old})
	updated := old
	updated.SpeedLimit = 200
	desired := []api.UserInfo{updated}
	client.lists = []*[]api.UserInfo{&desired}
	key := c.buildUserTag(&old)
	counter, _ := c.stm.GetOrRegisterCounter("user>>>" + key + ">>>traffic>>>downlink")
	counter.Add(5 << 30)
	manager.failAdd[key] = 1
	c.nodeInfoMonitor()
	if c.pendingUserList == nil || len(*c.userList) != 1 || (*c.userList)[0] != old {
		t.Fatal("failed parameter replacement lost its previous identity")
	}
	c.nodeInfoMonitor()
	if c.pendingUserList != nil || (*c.userList)[0] != updated {
		t.Fatal("parameter update was not retried")
	}
	if _, down, _, current := c.getTraffic(key); current != counter || down != 5<<30 {
		t.Fatal("parameter update detached/reset traffic counters")
	}
	c.nodeInfo.VlessFlow = "xtls-rprx-vision"
	if _, err := c.addUsers(c.buildVlessUser(&desired), c.Tag); err != nil {
		t.Fatal(err)
	}
	if manager.users[key].Account.(*vless.MemoryAccount).Flow != "xtls-rprx-vision" {
		t.Fatal("account equality ignored changed VLESS flow")
	}
	if _, err := c.addUsers([]*protocol.User{nil}, c.Tag); err == nil {
		t.Fatal("invalid/skipped builder account was accepted")
	}
}

func TestLifecycleLimiterUpdateRetriesAfterAuthSuccess(t *testing.T) {
	user := lifecycleUser(1, "one@example.invalid")
	user.SpeedLimit = 200
	c, client, manager := newLifecycleController(t, nil)
	desired := []api.UserInfo{user}
	client.lists = []*[]api.UserInfo{&desired}
	c.dispatcher.Limiter.DeleteInboundLimiter(c.Tag)
	c.nodeInfoMonitor()
	if len(manager.users) != 1 || c.pendingUserList == nil {
		t.Fatal("limiter failure lost the newly applied auth user")
	}
	empty := []api.UserInfo{}
	c.dispatcher.Limiter.AddInboundLimiter(c.Tag, 0, &empty, nil)
	c.limitedUsers = map[api.UserInfo]LimitInfo{user: {end: time.Now().Unix() + 60, currentSpeedLimit: 1, originSpeedLimit: 200}}
	c.nodeInfoMonitor()
	info, _ := c.dispatcher.Limiter.InboundInfo.Load(c.Tag)
	stored, exists := info.(*limiter.InboundInfo).UserInfo.Load(c.buildUserTag(&user))
	if !exists || stored.(limiter.UserInfo).SpeedLimit != 125000 || c.pendingUserList != nil || len(manager.added) != 1 {
		t.Fatal("304 did not repair limiter state or erased the active automatic limit")
	}
}

func TestLifecycleFailedParameterReplacementRevertedOrWithdrawn(t *testing.T) {
	old := lifecycleUser(1, "one@example.invalid")
	old.SpeedLimit = 100
	for _, withdraw := range []bool{false, true} {
		c, client, manager := newLifecycleController(t, []api.UserInfo{old})
		updated := old
		updated.SpeedLimit = 200
		desired := []api.UserInfo{updated}
		latest := []api.UserInfo{old}
		if withdraw {
			latest = nil
		}
		client.lists = []*[]api.UserInfo{&desired, &latest}
		key := c.buildUserTag(&old)
		counter, _ := c.stm.GetOrRegisterCounter("user>>>" + key + ">>>traffic>>>downlink")
		counter.Add(100)
		manager.failAdd[key] = 1
		c.nodeInfoMonitor()
		c.nodeInfoMonitor()
		want := 1
		if withdraw {
			want = 0
		}
		if len(manager.users) != want || len(*c.userList) != want || c.pendingUserList != nil {
			t.Fatalf("reverted/withdrawn replacement not reconciled: withdraw=%t", withdraw)
		}
		_, down, _, current := c.getTraffic(key)
		if withdraw {
			if current != nil {
				t.Fatal("withdrawn replacement orphaned a counter")
			}
		} else if current != counter || down != 100 {
			t.Fatal("reverted replacement detached its counter")
		}
	}
}

func TestLifecycleExpiredLimitsDoNotResurrectUsers(t *testing.T) {
	user := lifecycleUser(1, "one@example.invalid")
	user.SpeedLimit = 100
	for _, changed := range []bool{false, true} {
		c, _, _ := newLifecycleController(t, []api.UserInfo{user})
		c.config.AutoSpeedLimitConfig.Limit = 1
		c.limitedUsers = map[api.UserInfo]LimitInfo{user: {end: time.Now().Unix() - 1, originSpeedLimit: user.SpeedLimit}}
		info, _ := c.dispatcher.Limiter.InboundInfo.Load(c.Tag)
		inboundInfo := info.(*limiter.InboundInfo)
		key := c.buildUserTag(&user)
		active := []api.UserInfo{}
		if changed {
			updated := user
			updated.SpeedLimit = 200
			active = append(active, updated)
			c.UpdateInboundLimiter(c.Tag, &active)
		} else {
			c.DeleteInboundLimiterUsers(c.Tag, []api.UserInfo{user})
		}
		c.userList = &active
		c.userInfoMonitor()
		if len(c.limitedUsers) != 0 {
			t.Fatal("stale auto limit was retained")
		}
		stored, exists := inboundInfo.UserInfo.Load(key)
		if changed {
			if !exists || stored.(limiter.UserInfo).SpeedLimit != 200 {
				t.Fatal("old speed overwrote new parameters")
			}
		} else {
			if exists {
				t.Fatal("removed user limiter was resurrected")
			}
			if _, blocked := inboundInfo.BlockedUsers.Load(key); !blocked {
				t.Fatal("removed user's tombstone erased")
			}
		}
	}
	c, _, _ := newLifecycleController(t, []api.UserInfo{user})
	c.config.AutoSpeedLimitConfig.Limit = 1
	c.limitedUsers = map[api.UserInfo]LimitInfo{user: {end: time.Now().Unix() - 1, originSpeedLimit: user.SpeedLimit}}
	c.dispatcher.Limiter.DeleteInboundLimiter(c.Tag)
	c.userInfoMonitor()
	if len(c.limitedUsers) != 1 {
		t.Fatal("failed speed restoration lost retry state")
	}
	c.AddInboundLimiter(c.Tag, 0, c.userList, nil)
	c.userInfoMonitor()
	if len(c.limitedUsers) != 0 {
		t.Fatal("speed restoration did not retry")
	}
}

func TestLifecycleExceptionsVisibleAtInfoAndDebug(t *testing.T) {
	for _, level := range []log.Level{log.InfoLevel, log.DebugLevel} {
		t.Run(level.String(), func(t *testing.T) {
			user := lifecycleUser(1, "one@example.invalid")
			c, client, manager := newLifecycleController(t, []api.UserInfo{user})
			var output bytes.Buffer
			logger := log.New()
			logger.SetOutput(&output)
			logger.SetLevel(level)
			c.logger = log.NewEntry(logger).WithFields(log.Fields{"Host": "https://panel.example.invalid", "ID": 251, "Type": "Vless"})
			client.failAll = true
			client.report = func(*[]api.UserTraffic) error { return errors.New("simulated traffic timeout") }
			key := c.buildUserTag(&user)
			counter, _ := c.stm.GetOrRegisterCounter("user>>>" + key + ">>>traffic>>>downlink")
			counter.Add(100)
			_, _, _, release := c.dispatcher.Limiter.GetUserBucket(c.Tag, key, "127.0.0.1")
			defer release()
			c.UpdateRule(c.Tag, []api.DetectRule{{ID: 1, Pattern: regexp.MustCompile("bad.invalid")}})
			c.dispatcher.RuleManager.Detect(c.Tag, "bad.invalid", key)
			c.userInfoMonitor()
			if counter.Value() != 100 {
				t.Fatal("failed live report discarded bytes")
			}
			empty := []api.UserInfo{}
			client.lists = []*[]api.UserInfo{&empty}
			manager.failRemove[key] = 1
			c.nodeInfoMonitor()
			c.nodeInfoMonitor()
			for _, message := range []string{"Report node status failed", "Report user traffic failed", "counters retained for next cycle", "Report online users failed", "Report illegal behaviors failed", "Remove users failed", "Report removed-user traffic failed", "traffic discarded and counters unregistered"} {
				if strings.Count(output.String(), message) != 1 {
					t.Fatalf("missing/duplicate exception %q at %s: %s", message, level, output.String())
				}
			}
			if !strings.Contains(output.String(), "panel.example.invalid") || !strings.Contains(output.String(), "ID=251") {
				t.Fatal("exception logs lost node context")
			}
			if err := c.runPeriodic(func() error { return errors.New("unexpected periodic failure") }); err != nil {
				t.Fatal("recoverable refresh error stopped periodic scheduling")
			}
		})
	}
}
