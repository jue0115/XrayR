package hysteria

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

func TestClientManagerRemovesInactiveEntries(t *testing.T) {
	manager := &clientManager{m: map[dialerConf]*client{
		{}: {lastUsed: time.Now().Add(-2 * time.Minute)},
	}}
	manager.clean()
	if len(manager.m) != 0 {
		t.Fatal("inactive Hysteria client is still retained")
	}
	if err := manager.close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.close(); err != nil {
		t.Fatal("second close must be harmless:", err)
	}
}

func TestClientManagerRemovesClosedInstances(t *testing.T) {
	closed := &client{instance: &core.Instance{}, lastUsed: time.Now()}
	manager := &clientManager{m: map[dialerConf]*client{{}: closed}}
	manager.clean()
	if len(manager.m) != 0 || !closed.forced {
		t.Fatal("closed instance must be removed and its client permanently stopped")
	}
	if err := closed.dial(context.Background()); err == nil {
		t.Fatal("removed client must not dial again")
	}
}

func TestClientManagerCleanupDoesNotHoldPoolLock(t *testing.T) {
	blocked := &client{lastUsed: time.Now().Add(-2 * time.Minute)}
	manager := &clientManager{m: map[dialerConf]*client{{}: blocked}}
	blocked.Lock()
	done := make(chan struct{})
	go func() { manager.clean(); close(done) }()
	// Let cleanup reach the deliberately blocked client before probing the pool.
	time.Sleep(20 * time.Millisecond)
	replacement := &client{lastUsed: time.Now()}
	updated := make(chan struct{})
	go func() {
		manager.Lock()
		manager.m[dialerConf{}] = replacement
		manager.Unlock()
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(time.Second):
		blocked.Unlock()
		<-done
		<-updated
		t.Fatal("one busy client blocked the shared pool")
	}
	blocked.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	if manager.m[dialerConf{}] != replacement {
		t.Fatal("cleanup removed a newer client under the same cache key")
	}
}
