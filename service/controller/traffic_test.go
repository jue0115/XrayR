package controller

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	log "github.com/sirupsen/logrus"
	corestats "github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/features/stats"
)

type trafficTestAPI struct {
	api.API
	report func(*[]api.UserTraffic) error
}

func (a *trafficTestAPI) ReportUserTraffic(traffic *[]api.UserTraffic) error {
	return a.report(traffic)
}

func newTrafficTestController(t *testing.T) (*Controller, *trafficTestAPI, api.UserInfo, stats.Counter, stats.Counter) {
	t.Helper()
	manager, err := corestats.NewManager(context.Background(), &corestats.Config{})
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New()
	logger.SetOutput(io.Discard)
	client := &trafficTestAPI{report: func(*[]api.UserTraffic) error { return nil }}
	c := &Controller{Tag: "test", config: &Config{}, stm: manager, apiClient: client, logger: log.NewEntry(logger)}
	user := api.UserInfo{UID: 1, Email: "test@example.invalid"}
	key := c.buildUserTag(&user)
	up, err := manager.GetOrRegisterCounter("user>>>" + key + ">>>traffic>>>uplink")
	if err != nil {
		t.Fatal(err)
	}
	down, err := manager.GetOrRegisterCounter("user>>>" + key + ">>>traffic>>>downlink")
	if err != nil {
		t.Fatal(err)
	}
	return c, client, user, up, down
}

func TestTrafficSnapshotRetryAndLiveBytes(t *testing.T) {
	c, client, user, up, down := newTrafficTestController(t)
	up.Add(1000)
	down.Add(5 << 30)
	snapshot := c.snapshotTraffic(user)
	if snapshot == nil {
		t.Fatal("missing snapshot")
	}
	client.report = func(traffic *[]api.UserTraffic) error {
		if len(*traffic) != 1 || (*traffic)[0] != snapshot.UserTraffic {
			t.Fatal("retry changed the original payload", *traffic)
		}
		up.Add(50)
		down.Add(1 << 30)
		return errors.New("panel unavailable")
	}
	if err := c.reportTraffic([]*trafficSnapshot{snapshot}); err == nil {
		t.Fatal("expected simulated timeout")
	}
	if up.Value() != 1050 || down.Value() != 6<<30 {
		t.Fatal("failed report changed the live counters")
	}
	retry := c.snapshotTraffic(user)
	if retry == snapshot || retry.Upload != 1050 || retry.Download != 6<<30 {
		t.Fatal("next reporting cycle did not include all unacknowledged bytes")
	}
	client.report = func(traffic *[]api.UserTraffic) error {
		if (*traffic)[0] != retry.UserTraffic {
			t.Fatal("unexpected retry payload")
		}
		up.Add(25)
		down.Add(512)
		return nil
	}
	if err := c.reportTraffic([]*trafficSnapshot{retry}); err != nil {
		t.Fatal(err)
	}
	if up.Value() != 25 || down.Value() != 512 {
		t.Fatalf("live bytes lost: %d/%d", up.Value(), down.Value())
	}
	next := c.snapshotTraffic(user)
	if next.Upload != 25 || next.Download != 512 {
		t.Fatal("new bytes lost or acknowledged bytes reported again", next.UserTraffic)
	}
}

func TestTrafficUserUpdatesKeepActiveCounters(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*api.UserInfo)
	}{
		{"speed", func(u *api.UserInfo) { u.SpeedLimit = 1000000 }},
		{"devices", func(u *api.UserInfo) { u.DeviceLimit = 3 }},
		{"uuid", func(u *api.UserInfo) { u.UUID = "new-uuid" }},
		{"password", func(u *api.UserInfo) { u.Passwd = "new-password" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			c, _, user, _, down := newTrafficTestController(t)
			down.Add(5 << 30)
			active := &mydispatcher.SizeStatWriter{Counter: down, Writer: buf.Discard}
			updated := user
			change.edit(&updated)
			oldList, newList := []api.UserInfo{user}, []api.UserInfo{updated}
			deleted, added := compareUserList(&oldList, &newList)
			removed := removedUsers(deleted, added)
			if len(removed) != 0 {
				t.Fatal("parameter update treated as traffic identity removal")
			}
			c.reportDeletedUserTraffic(removed)
			if err := active.WriteMultiBuffer(buf.MergeBytes(nil, []byte("still flowing"))); err != nil {
				t.Fatal(err)
			}
			_, got, _, current := c.getTraffic(c.buildUserTag(&updated))
			if current != down || got != 5<<30+13 {
				t.Fatalf("active connection's counter detached: %d", got)
			}
		})
	}
}

func TestTrafficDeletedFailureIsDiscarded(t *testing.T) {
	c, client, user, up, down := newTrafficTestController(t)
	up.Add(1000)
	down.Add(5 << 30)
	c.snapshotTraffic(user)
	client.report = func(*[]api.UserTraffic) error { return errors.New("panel unavailable") }
	c.reportDeletedUserTraffic([]api.UserInfo{user})
	u, d, uc, dc := c.getTraffic(c.buildUserTag(&user))
	if u != 0 || d != 0 || uc != nil || dc != nil {
		t.Fatal("deleted user's failed traffic must be discarded")
	}
	if len(removedUsers([]api.UserInfo{user}, nil)) != 1 {
		t.Fatal("genuine deletion was ignored")
	}
}

func TestTrafficDeletedReportsCurrentBytes(t *testing.T) {
	c, client, user, up, down := newTrafficTestController(t)
	up.Add(100)
	down.Add(1000)
	c.snapshotTraffic(user)
	up.Add(50)
	down.Add(500)
	var reports []api.UserTraffic
	client.report = func(traffic *[]api.UserTraffic) error {
		reports = append(reports, (*traffic)...)
		return nil
	}
	c.reportDeletedUserTraffic([]api.UserInfo{user})
	if len(reports) != 1 || reports[0].Upload != 150 || reports[0].Download != 1500 {
		t.Fatal("deletion must report all currently accumulated bytes once", reports)
	}
	_, _, upCounter, downCounter := c.getTraffic(c.buildUserTag(&user))
	if upCounter != nil || downCounter != nil {
		t.Fatal("deleted user's counters remain registered")
	}
}
