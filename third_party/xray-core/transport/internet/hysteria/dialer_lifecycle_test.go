package hysteria

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

func TestClientLockWaitHonorsCancellation(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			c := &client{lock: make(chan struct{}, 1)}
			c.lock <- struct{}{}
			defer c.unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			var err error
			if network == "tcp" {
				_, err = c.tcp(ctx)
			} else {
				_, err = c.udp(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lock wait must respect cancellation: %v", err)
			}
		})
	}
}

func TestClientManagerRemovesInactiveEntries(t *testing.T) {
	manager := &clientManager{m: map[dialerConf]*client{
		{}: {lock: make(chan struct{}, 1), lastUsed: time.Now().Add(-2 * time.Minute)},
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
	closed := &client{lock: make(chan struct{}, 1), instance: &core.Instance{}, lastUsed: time.Now()}
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
	blocked := &client{lock: make(chan struct{}, 1), lastUsed: time.Now().Add(-2 * time.Minute)}
	manager := &clientManager{m: map[dialerConf]*client{{}: blocked}}
	blocked.lock <- struct{}{}
	done := make(chan struct{})
	go func() { manager.clean(); close(done) }()
	// Let cleanup reach the deliberately blocked client before probing the pool.
	time.Sleep(20 * time.Millisecond)
	replacement := &client{lock: make(chan struct{}, 1), lastUsed: time.Now()}
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
		blocked.unlock()
		<-done
		<-updated
		t.Fatal("one busy client blocked the shared pool")
	}
	blocked.unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	if manager.m[dialerConf{}] != replacement {
		t.Fatal("cleanup removed a newer client under the same cache key")
	}
}
