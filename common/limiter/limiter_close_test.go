package limiter

import (
	"testing"

	"github.com/XrayR-project/XrayR/api"
)

func TestLimiterCloseClearsInboundState(t *testing.T) {
	limiter := New()
	users := []api.UserInfo{{UID: 1, Email: "user@example.com"}}
	if err := limiter.AddInboundLimiter("test", 0, &users, nil); err != nil {
		t.Fatal(err)
	}

	value, found := limiter.InboundInfo.Load("test")
	if !found {
		t.Fatal("inbound limiter was not added")
	}
	inbound := value.(*InboundInfo)
	if err := limiter.Close(); err != nil {
		t.Fatal(err)
	}
	if !inbound.closed.Load() {
		t.Fatal("inbound limiter was not closed")
	}
	if _, found := limiter.InboundInfo.Load("test"); found {
		t.Fatal("closed inbound limiter is still retained")
	}
	if err := limiter.Close(); err != nil {
		t.Fatal("second close must be harmless:", err)
	}
}
