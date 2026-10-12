package limiter

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/XrayR-project/XrayR/api"
)

func connectionLimiter(t *testing.T, speed uint64) (*Limiter, *InboundInfo, []api.UserInfo, string) {
	t.Helper()
	l := New()
	t.Cleanup(func() { l.Close() })
	users := []api.UserInfo{{UID: 1, Email: "live@example.invalid", SpeedLimit: speed, DeviceLimit: 1}}
	if err := l.AddInboundLimiter("test", 0, &users, nil); err != nil {
		t.Fatal(err)
	}
	value, _ := l.InboundInfo.Load("test")
	return l, value.(*InboundInfo), users, "test|live@example.invalid|1"
}

func expireConnectionDevice(t *testing.T, inbound *InboundInfo, email, ip string) {
	t.Helper()
	inbound.onlineAccess.Lock()
	defer inbound.onlineAccess.Unlock()
	value, found := inbound.UserOnlineIP.Load(email)
	if !found {
		t.Fatal("online user was removed")
	}
	ips := value.(*sync.Map)
	value, found = ips.Load(ip)
	if !found {
		t.Fatal("online IP was removed")
	}
	device := value.(onlineDevice)
	device.lastSeen = time.Now().Add(-onlineDeviceTTL - time.Minute).UnixNano()
	ips.Store(ip, device)
}

func TestLimiterActiveConnectionSurvivesOnlineTTL(t *testing.T) {
	for _, speed := range []uint64{0, 1024} {
		t.Run(fmt.Sprintf("speed=%d", speed), func(t *testing.T) {
			l, inbound, users, email := connectionLimiter(t, speed)
			bucket, limited, rejected, release := l.GetUserBucket("test", email, "192.0.2.1")
			if rejected || release == nil || limited != (speed > 0) {
				t.Fatal("first connection was not admitted")
			}
			defer release()
			expireConnectionDevice(t, inbound, email, "192.0.2.1")
			online, err := l.GetOnlineDevice("test")
			if err != nil || len(*online) != 1 || (*online)[0].IP != "192.0.2.1" {
				t.Fatal("open connection was evicted by TTL", online, err)
			}
			// Check admission's own eviction path, not only online reporting.
			if _, _, rejected, _ := l.GetUserBucket("test", email, "192.0.2.2"); !rejected {
				t.Fatal("expired timestamp allowed another device beside a live connection")
			}
			next, _, rejected, releaseNext := l.GetUserBucket("test", email, "192.0.2.1")
			if rejected || releaseNext == nil || next != bucket {
				t.Fatal("same-IP connection did not share the live bucket")
			}
			defer releaseNext()
			release()
			release() // Repeated cleanup must not release the second connection.
			expireConnectionDevice(t, inbound, email, "192.0.2.1")
			if online, err := l.GetOnlineDevice("test"); err != nil || len(*online) != 1 {
				t.Fatal("one finished connection evicted another live connection", err)
			}
			if speed > 0 {
				users[0].SpeedLimit = 2048
				if err := l.UpdateInboundLimiter("test", &users); err != nil {
					t.Fatal(err)
				}
				if bucket.Limit() != 2048 || bucket.Burst() != 2048 {
					t.Fatal("live shared bucket missed the updated speed limit")
				}
			}
			releaseNext()
			// Keep the normal idle grace period, then release memory once no
			// connection references the IP/bucket anymore.
			if online, err := l.GetOnlineDevice("test"); err != nil || len(*online) != 1 {
				t.Fatal("connection close skipped the idle grace period", err)
			}
			expireConnectionDevice(t, inbound, email, "192.0.2.1")
			if online, err := l.GetOnlineDevice("test"); err != nil || len(*online) != 0 {
				t.Fatal("finished connection survived idle cleanup", err)
			}
			if _, found := inbound.UserOnlineIP.Load(email); found {
				t.Fatal("idle online-IP map was retained")
			}
			if _, found := inbound.BucketHub.Load(email); found {
				t.Fatal("unused bucket was retained")
			}
			fresh, _, rejected, releaseFresh := l.GetUserBucket("test", email, "192.0.2.2")
			if rejected || releaseFresh == nil || (speed > 0 && fresh == bucket) {
				t.Fatal("device slot was not reusable after idle expiry")
			}
			releaseFresh()
		})
	}
}

func TestLimiterConnectionAdmissionAndCleanupConcurrent(t *testing.T) {
	l, inbound, _, email := connectionLimiter(t, 1024)
	bucket, _, _, release := l.GetUserBucket("test", email, "192.0.2.1")
	defer release()
	expireConnectionDevice(t, inbound, email, "192.0.2.1")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if online, err := l.GetOnlineDevice("test"); err != nil || len(*online) != 1 {
				t.Error("concurrent cleanup lost the live device", err)
			}
			next, _, rejected, done := l.GetUserBucket("test", email, "192.0.2.1")
			if done != nil {
				defer done()
			}
			if rejected || done == nil || next != bucket {
				t.Error("concurrent admission replaced the live bucket")
			}
			if _, _, rejected, done := l.GetUserBucket("test", email, "192.0.2.2"); !rejected {
				t.Error("concurrent admission bypassed the device limit")
				if done != nil {
					done()
				}
			}
		}()
	}
	wg.Wait()
	value, _ := inbound.UserOnlineIP.Load(email)
	value, _ = value.(*sync.Map).Load("192.0.2.1")
	if got := value.(onlineDevice).connections; got != 1 {
		t.Fatalf("finished concurrent dispatches retained %d references, want 1", got)
	}
}

func TestLimiterOldReleaseDoesNotChangeReaddedUser(t *testing.T) {
	l, inbound, users, email := connectionLimiter(t, 1024)
	_, _, _, releaseOld := l.GetUserBucket("test", email, "192.0.2.1")
	if err := l.DeleteUsers("test", users); err != nil {
		t.Fatal(err)
	}
	if err := l.UpdateInboundLimiter("test", &users); err != nil {
		t.Fatal(err)
	}
	_, _, rejected, releaseNew := l.GetUserBucket("test", email, "192.0.2.1")
	if rejected || releaseNew == nil {
		t.Fatal("re-added user was rejected")
	}
	releaseOld()
	expireConnectionDevice(t, inbound, email, "192.0.2.1")
	if online, err := l.GetOnlineDevice("test"); err != nil || len(*online) != 1 {
		t.Fatal("old release cleared a re-added user's live connection", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	releaseNew()
	if _, found := inbound.UserOnlineIP.Load(email); found {
		t.Fatal("release after shutdown recreated online state")
	}
}
