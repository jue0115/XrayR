package hysteria

import (
	"testing"
	"time"
)

func TestClientManagerRemovesInactiveEntries(t *testing.T) {
	manager := &clientManager{m: map[string]*client{
		"stale": {lastUsed: time.Now().Add(-2 * time.Minute)},
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
