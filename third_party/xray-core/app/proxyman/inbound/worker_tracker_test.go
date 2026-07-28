package inbound

import (
	gonet "net"
	"testing"
	"time"
)

func TestConnectionTrackerClosesAndWaits(t *testing.T) {
	tracked, peer := gonet.Pipe()
	defer peer.Close()

	tracker := new(connectionTracker)
	id, accepted := tracker.add(tracked)
	if !accepted {
		t.Fatal("connection was unexpectedly rejected")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var buffer [1]byte
		tracked.Read(buffer[:])
		tracker.remove(id)
	}()

	tracker.stopAccepting()
	tracker.closeAll()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tracked connection was not closed")
	}

	other, otherPeer := gonet.Pipe()
	defer otherPeer.Close()
	defer other.Close()
	if _, accepted := tracker.add(other); accepted {
		t.Fatal("connection accepted after tracker started closing")
	}
}
