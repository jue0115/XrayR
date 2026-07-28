package limiter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/marshaler"
	goCacheStore "github.com/eko/gocache/store/go_cache/v4"
	goCache "github.com/patrickmn/go-cache"
)

func TestGlobalLimitConcurrentCapacity(t *testing.T) {
	inbound := &InboundInfo{Tag: "test"}
	inbound.GlobalLimit.config = &GlobalDeviceLimitConfig{Enable: true, Timeout: 1, Expiry: 60}
	local := goCache.New(time.Minute, time.Minute)
	chain := cache.NewChain[any](cache.New[any](goCacheStore.NewGoCache(local)))
	inbound.GlobalLimit.globalOnlineIP = marshaler.New(chain)

	const limit = 10
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !globalLimit(inbound, "test|user|1", 1, fmt.Sprintf("192.0.2.%d", i), limit) {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := accepted.Load(); got != limit {
		t.Fatalf("accepted %d devices, want exactly %d", got, limit)
	}
	value, err := inbound.GlobalLimit.globalOnlineIP.Get(context.Background(), "10|user|1", new(map[string]int))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(*value.(*map[string]int)); got != limit {
		t.Fatalf("cached %d devices, want %d", got, limit)
	}
}
