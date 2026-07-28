package dns

import "testing"

func TestCacheControllerClose(t *testing.T) {
	cache := NewCacheController("test", false, false, 0)
	cache.ips["example.com"] = &record{}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if !cache.closed.Load() || len(cache.ips) != 0 {
		t.Fatal("cache controller retained state after close")
	}
	if err := cache.Close(); err != nil {
		t.Fatal("second close must be harmless:", err)
	}
}
