// Package limiter is to control the links that go into the dispatcher
package limiter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/marshaler"
	"github.com/eko/gocache/lib/v4/store"
	goCacheStore "github.com/eko/gocache/store/go_cache/v4"
	redisStore "github.com/eko/gocache/store/redis/v4"
	goCache "github.com/patrickmn/go-cache"
	"github.com/redis/go-redis/v9"
	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/time/rate"

	"github.com/XrayR-project/XrayR/api"
)

type UserInfo struct {
	UID         int
	SpeedLimit  uint64
	DeviceLimit int
}

type onlineDevice struct {
	uid      int
	lastSeen int64
}

const onlineDeviceTTL = 5 * time.Minute
const blockedUserTTL = 24 * time.Hour

type InboundInfo struct {
	Tag            string
	NodeSpeedLimit uint64
	UserInfo       *sync.Map // Key: Email value: UserInfo
	BucketHub      *sync.Map // key: Email, value: *rate.Limiter
	UserOnlineIP   *sync.Map // Key: Email, value: {Key: IP, value: UID}
	BlockedUsers   *sync.Map // Key: Email, value: struct{} — users removed by panel; reject their lingering connections
	closed         atomic.Bool
	GlobalLimit    struct {
		access         sync.Mutex
		config         *GlobalDeviceLimitConfig
		globalOnlineIP *marshaler.Marshaler
		cacheManager   *cache.ChainCache[any]
		localCache     *goCache.Cache
		redisClient    *redis.Client
	}
}

type Limiter struct {
	InboundInfo *sync.Map // Key: Tag, Value: *InboundInfo
	closed      atomic.Bool
}

func New() *Limiter {
	return &Limiter{
		InboundInfo: new(sync.Map),
	}
}

func (l *Limiter) AddInboundLimiter(tag string, nodeSpeedLimit uint64, userList *[]api.UserInfo, globalLimit *GlobalDeviceLimitConfig) error {
	if l.closed.Load() {
		return fmt.Errorf("limiter is closed")
	}
	inboundInfo := &InboundInfo{
		Tag:            tag,
		NodeSpeedLimit: nodeSpeedLimit,
		BucketHub:      new(sync.Map),
		UserOnlineIP:   new(sync.Map),
		BlockedUsers:   new(sync.Map),
	}

	if globalLimit != nil && globalLimit.Enable {
		inboundInfo.GlobalLimit.config = globalLimit

		// init local store
		localCache := goCache.New(time.Duration(globalLimit.Expiry)*time.Second, time.Minute)
		gs := goCacheStore.NewGoCache(localCache)

		// init redis store
		redisClient := redis.NewClient(
			&redis.Options{
				Network:  globalLimit.RedisNetwork,
				Addr:     globalLimit.RedisAddr,
				Username: globalLimit.RedisUsername,
				Password: globalLimit.RedisPassword,
				DB:       globalLimit.RedisDB,
			})
		rs := redisStore.NewRedis(redisClient,
			store.WithExpiration(time.Duration(globalLimit.Expiry)*time.Second))

		// init chained cache. First use local go-cache, if go-cache is nil, then use redis cache
		cacheManager := cache.NewChain[any](
			cache.New[any](gs), // go-cache is priority
			cache.New[any](rs),
		)
		inboundInfo.GlobalLimit.globalOnlineIP = marshaler.New(cacheManager)
		inboundInfo.GlobalLimit.cacheManager = cacheManager
		inboundInfo.GlobalLimit.localCache = localCache
		inboundInfo.GlobalLimit.redisClient = redisClient
	}

	userMap := new(sync.Map)
	for _, u := range *userList {
		userMap.Store(fmt.Sprintf("%s|%s|%d", tag, u.Email, u.UID), UserInfo{
			UID:         u.UID,
			SpeedLimit:  u.SpeedLimit,
			DeviceLimit: u.DeviceLimit,
		})
	}
	inboundInfo.UserInfo = userMap
	if old, loaded := l.InboundInfo.Swap(tag, inboundInfo); loaded {
		if err := old.(*InboundInfo).close(); err != nil {
			errors.LogWarningInner(context.Background(), err, "failed to close replaced inbound limiter")
		}
	}
	return nil
}

func (l *Limiter) UpdateInboundLimiter(tag string, updatedUserList *[]api.UserInfo) error {
	if l.closed.Load() {
		return fmt.Errorf("limiter is closed")
	}
	if value, ok := l.InboundInfo.Load(tag); ok {
		inboundInfo := value.(*InboundInfo)
		// Update User info
		for _, u := range *updatedUserList {
			key := fmt.Sprintf("%s|%s|%d", tag, u.Email, u.UID)
			// A re-added user is no longer blocked.
			if inboundInfo.BlockedUsers != nil {
				inboundInfo.BlockedUsers.Delete(key)
			}
			inboundInfo.UserInfo.Store(key, UserInfo{
				UID:         u.UID,
				SpeedLimit:  u.SpeedLimit,
				DeviceLimit: u.DeviceLimit,
			})
			// Update old limiter bucket
			limit := determineRate(inboundInfo.NodeSpeedLimit, u.SpeedLimit)
			if limit > 0 {
				if bucket, ok := inboundInfo.BucketHub.Load(key); ok {
					limiter := bucket.(*rate.Limiter)
					limiter.SetLimit(rate.Limit(limit))
					limiter.SetBurst(int(limit))
				}
			} else {
				inboundInfo.BucketHub.Delete(key)
			}
		}
	} else {
		return fmt.Errorf("no such inbound in limiter: %s", tag)
	}
	return nil
}

// DeleteUsers removes the given users from an inbound's limiter state and marks
// them blocked, so a connection established before removal is rejected on its
// next dispatch and stops consuming traffic (instead of running for hours and
// dumping one huge traffic report when the user is later re-added).
func (l *Limiter) DeleteUsers(tag string, users []api.UserInfo) error {
	if l.closed.Load() {
		return fmt.Errorf("limiter is closed")
	}
	if value, ok := l.InboundInfo.Load(tag); ok {
		inboundInfo := value.(*InboundInfo)
		if inboundInfo.BlockedUsers == nil {
			inboundInfo.BlockedUsers = new(sync.Map)
		}
		for _, u := range users {
			key := fmt.Sprintf("%s|%s|%d", tag, u.Email, u.UID)
			inboundInfo.UserInfo.Delete(key)
			inboundInfo.BucketHub.Delete(key)
			inboundInfo.UserOnlineIP.Delete(key)
			inboundInfo.BlockedUsers.Store(key, time.Now().Add(blockedUserTTL).UnixNano())
		}
	} else {
		return fmt.Errorf("no such inbound in limiter: %s", tag)
	}
	return nil
}

func (l *Limiter) DeleteInboundLimiter(tag string) error {
	if value, loaded := l.InboundInfo.LoadAndDelete(tag); loaded {
		return value.(*InboundInfo).close()
	}
	return nil
}

func clearMap(m *sync.Map) {
	if m == nil {
		return
	}
	m.Range(func(key, _ any) bool {
		m.Delete(key)
		return true
	})
}

func (i *InboundInfo) close() error {
	if i == nil || i.closed.Swap(true) {
		return nil
	}

	var errs []error
	if i.GlobalLimit.cacheManager != nil {
		if err := i.GlobalLimit.cacheManager.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if i.GlobalLimit.localCache != nil {
		i.GlobalLimit.localCache.Flush()
	}
	if i.GlobalLimit.redisClient != nil {
		if err := i.GlobalLimit.redisClient.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	clearMap(i.UserInfo)
	clearMap(i.BucketHub)
	clearMap(i.UserOnlineIP)
	clearMap(i.BlockedUsers)
	return errors.Combine(errs...)
}

// Close releases all per-inbound limiter state and cache clients.
func (l *Limiter) Close() error {
	if l == nil || l.closed.Swap(true) {
		return nil
	}

	var errs []error
	l.InboundInfo.Range(func(key, _ any) bool {
		if value, loaded := l.InboundInfo.LoadAndDelete(key); loaded {
			if err := value.(*InboundInfo).close(); err != nil {
				errs = append(errs, err)
			}
		}
		return true
	})
	return errors.Combine(errs...)
}

func (l *Limiter) GetOnlineDevice(tag string) (*[]api.OnlineUser, error) {
	if l.closed.Load() {
		return nil, fmt.Errorf("limiter is closed")
	}
	var onlineUser []api.OnlineUser

	if value, ok := l.InboundInfo.Load(tag); ok {
		inboundInfo := value.(*InboundInfo)
		now := time.Now().UnixNano()
		inboundInfo.UserOnlineIP.Range(func(key, value interface{}) bool {
			email := key.(string)
			ipMap := value.(*sync.Map)
			active := 0
			ipMap.Range(func(key, value interface{}) bool {
				device := value.(onlineDevice)
				if time.Duration(now-device.lastSeen) > onlineDeviceTTL {
					ipMap.Delete(key)
					return true
				}
				active++
				ip := key.(string)
				onlineUser = append(onlineUser, api.OnlineUser{UID: device.uid, IP: ip})
				return true
			})
			if active == 0 {
				inboundInfo.UserOnlineIP.Delete(email)
				inboundInfo.BucketHub.Delete(email)
			}
			return true
		})
	} else {
		return nil, fmt.Errorf("no such inbound in limiter: %s", tag)
	}

	return &onlineUser, nil
}

func (l *Limiter) GetUserBucket(tag string, email string, ip string) (limiter *rate.Limiter, SpeedLimit bool, Reject bool) {
	if l.closed.Load() {
		return nil, false, true
	}
	if value, ok := l.InboundInfo.Load(tag); ok {
		var (
			userLimit        uint64 = 0
			deviceLimit, uid int
		)

		inboundInfo := value.(*InboundInfo)
		if inboundInfo.closed.Load() {
			return nil, false, true
		}
		nodeLimit := inboundInfo.NodeSpeedLimit

		// Reject users removed by the panel: their auth is gone, but a connection
		// established before removal would otherwise keep consuming traffic.
		if inboundInfo.BlockedUsers != nil {
			if value, blocked := inboundInfo.BlockedUsers.Load(email); blocked {
				if time.Now().UnixNano() < value.(int64) {
					return nil, false, true
				}
				inboundInfo.BlockedUsers.Delete(email)
			}
		}

		if v, ok := inboundInfo.UserInfo.Load(email); ok {
			u := v.(UserInfo)
			uid = u.UID
			userLimit = u.SpeedLimit
			deviceLimit = u.DeviceLimit
		}

		// Local device limit
		// Fast path: reuse the user's existing online-IP map instead of
		// allocating a throwaway sync.Map on every connection. Only allocate
		// when the user is not online yet.
		v, online := inboundInfo.UserOnlineIP.Load(email)
		if !online {
			ipMap := new(sync.Map)
			ipMap.Store(ip, onlineDevice{uid: uid, lastSeen: time.Now().UnixNano()})
			// LoadOrStore still resolves the race where two connections of a
			// first-time-online user arrive concurrently.
			v, online = inboundInfo.UserOnlineIP.LoadOrStore(email, ipMap)
		}
		// If any device is already online
		if online {
			ipMap := v.(*sync.Map)
			now := time.Now().UnixNano()
			ipMap.Range(func(key, value interface{}) bool {
				device := value.(onlineDevice)
				if time.Duration(now-device.lastSeen) > onlineDeviceTTL {
					ipMap.Delete(key)
				}
				return true
			})
			// If this is a new ip
			if _, ok := ipMap.LoadOrStore(ip, onlineDevice{uid: uid, lastSeen: now}); !ok {
				counter := 0
				ipMap.Range(func(key, value interface{}) bool {
					counter++
					return true
				})
				if counter > deviceLimit && deviceLimit > 0 {
					ipMap.Delete(ip)
					return nil, false, true
				}
			} else {
				ipMap.Store(ip, onlineDevice{uid: uid, lastSeen: now})
			}
		}

		// GlobalLimit
		if inboundInfo.GlobalLimit.config != nil && inboundInfo.GlobalLimit.config.Enable {
			if reject := globalLimit(inboundInfo, email, uid, ip, deviceLimit); reject {
				return nil, false, true
			}
		}

		// Speed limit
		limit := determineRate(nodeLimit, userLimit) // Determine the speed limit rate
		if limit > 0 {
			// Fast path: reuse the existing bucket instead of allocating a new
			// rate.Limiter on every connection. The stored bucket is kept in sync
			// with the current limit by UpdateInboundLimiter.
			if v, ok := inboundInfo.BucketHub.Load(email); ok {
				return v.(*rate.Limiter), true, false
			}
			limiter := rate.NewLimiter(rate.Limit(limit), int(limit)) // Byte/s
			v, _ := inboundInfo.BucketHub.LoadOrStore(email, limiter)
			return v.(*rate.Limiter), true, false
		} else {
			return nil, false, false
		}
	} else {
		errors.LogDebug(context.Background(), "Get Inbound Limiter information failed")
		return nil, false, false
	}
}

// Global device limit
func globalLimit(inboundInfo *InboundInfo, email string, uid int, ip string, deviceLimit int) bool {
	if inboundInfo.closed.Load() {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(inboundInfo.GlobalLimit.config.Timeout)*time.Second)
	defer cancel()

	// reformat email for unique key
	uniqueKey := strings.Replace(email, inboundInfo.Tag, strconv.Itoa(deviceLimit), 1)

	// Redis is the source of truth when global limiting is enabled. The Lua
	// script makes the existence check, capacity check and insertion atomic
	// across multiple XrayR instances.
	if client := inboundInfo.GlobalLimit.redisClient; client != nil {
		const deviceScript = `
local key = KEYS[1]
local ip = ARGV[1]
local limit = tonumber(ARGV[2])
local expiry = tonumber(ARGV[3])
if redis.call('SISMEMBER', key, ip) == 1 then
  redis.call('EXPIRE', key, expiry)
  return 0
end
if limit > 0 and redis.call('SCARD', key) >= limit then
  return 1
end
redis.call('SADD', key, ip)
redis.call('EXPIRE', key, expiry)
return 0`
		expiry := inboundInfo.GlobalLimit.config.Expiry
		if expiry <= 0 {
			expiry = 60
		}
		result, err := client.Eval(ctx, deviceScript,
			[]string{"xrayr:devices:" + uniqueKey}, ip, deviceLimit, expiry).Int()
		if err == nil {
			return result == 1
		}
		errors.LogWarningInner(context.Background(), err, "global device limit redis script failed, using local fallback")
	}

	// Serialize the local cache fallback. Cache values are ordinary Go maps and
	// must never be read or mutated concurrently.
	inboundInfo.GlobalLimit.access.Lock()
	defer inboundInfo.GlobalLimit.access.Unlock()

	v, err := inboundInfo.GlobalLimit.globalOnlineIP.Get(ctx, uniqueKey, new(map[string]int))
	if err != nil {
		if _, ok := err.(*store.NotFound); ok {
			pushIP(ctx, inboundInfo, uniqueKey, &map[string]int{ip: uid})
		} else {
			errors.LogErrorInner(context.Background(), err, "cache service")
		}
		return false
	}

	ipMap := v.(*map[string]int)
	// Reject device reach limit directly
	// If the ip is not in cache
	if _, ok := (*ipMap)[ip]; !ok {
		if deviceLimit > 0 && len(*ipMap) >= deviceLimit {
			return true
		}
		(*ipMap)[ip] = uid
		pushIP(ctx, inboundInfo, uniqueKey, ipMap)
	}

	return false
}

// push the ip to cache
func pushIP(ctx context.Context, inboundInfo *InboundInfo, uniqueKey string, ipMap *map[string]int) {
	if inboundInfo.closed.Load() {
		return
	}
	if err := inboundInfo.GlobalLimit.globalOnlineIP.Set(ctx, uniqueKey, ipMap); err != nil {
		errors.LogErrorInner(context.Background(), err, "cache service")
	}
}

// determineRate returns the minimum non-zero rate
func determineRate(nodeLimit, userLimit uint64) (limit uint64) {
	if nodeLimit == 0 || userLimit == 0 {
		if nodeLimit > userLimit {
			return nodeLimit
		} else if nodeLimit < userLimit {
			return userLimit
		} else {
			return 0
		}
	} else {
		if nodeLimit > userLimit {
			return userLimit
		} else if nodeLimit < userLimit {
			return nodeLimit
		} else {
			return nodeLimit
		}
	}
}
