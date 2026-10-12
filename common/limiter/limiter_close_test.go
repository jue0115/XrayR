package limiter

import (
	"fmt"
	"testing"
	"time"

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

func TestMemoryExpiredBlockedUsersCleanup(t *testing.T) {
	l := New()
	defer l.Close()
	users := []api.UserInfo{{UID: 1, Email: "expired@example.invalid"}, {UID: 2, Email: "blocked@example.invalid"}, {UID: 3, Email: "restored@example.invalid"}}
	if err := l.AddInboundLimiter("test", 0, &users, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteUsers("test", users); err != nil {
		t.Fatal(err)
	}
	v, _ := l.InboundInfo.Load("test")
	inbound := v.(*InboundInfo)
	key := func(user api.UserInfo) string { return fmt.Sprintf("test|%s|%d", user.Email, user.UID) }
	inbound.BlockedUsers.Store(key(users[0]), time.Now().Add(-time.Hour).UnixNano())
	future := time.Now().Add(time.Hour).UnixNano()
	inbound.BlockedUsers.Store(key(users[1]), future)
	restored := users[2:]
	if err := l.UpdateInboundLimiter("test", &restored); err != nil {
		t.Fatal(err)
	}
	if _, err := l.GetOnlineDevice("test"); err != nil {
		t.Fatal(err)
	}
	if _, found := inbound.BlockedUsers.Load(key(users[0])); found {
		t.Fatal("expired record retained without a reconnect")
	}
	if got, found := inbound.BlockedUsers.Load(key(users[1])); !found || got != future {
		t.Fatal("active ban was deleted or changed")
	}
	if _, found := inbound.BlockedUsers.Load(key(users[2])); found {
		t.Fatal("restored user is still blocked")
	}
	if _, _, rejected, _ := l.GetUserBucket("test", key(users[1]), "127.0.0.1"); !rejected {
		t.Fatal("cleanup weakened an active ban")
	}
	if _, _, rejected, release := l.GetUserBucket("test", key(users[2]), "127.0.0.1"); rejected {
		t.Fatal("restored user was rejected")
	} else {
		release()
	}
}
