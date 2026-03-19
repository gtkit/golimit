// Package golimit provides a thin, concurrency-safe rate limiting wrapper
// around golang.org/x/time/rate with automatic per-key isolation and cleanup.
package golimit

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Limiters holds a concurrent map of per-key rate limiters.
type Limiters struct {
	limiters sync.Map // map[string]*Limiter.
}

// Limiter wraps a rate.Limiter with last-access tracking for automatic cleanup.
type Limiter struct {
	limiter *rate.Limiter
	lastGet atomic.Int64 // Unix nano timestamp, updated atomically to avoid data race.
	key     string
}

// GlobalLimiters is the package-level limiter registry.
// All keys share this single registry with background cleanup.
var GlobalLimiters = &Limiters{}

var once sync.Once

// NewLimiter returns a per-key rate limiter that allows rps requests per second
// with an initial burst capacity of rps.
//
// The first call starts a background cleanup goroutine (via sync.Once).
// Subsequent calls for the same key return the existing limiter — they do NOT
// create a new one.
//
// Usage:
//
//	lim := golimit.NewLimiter("ip:192.168.1.1:/api/v1", 100)
//	if !lim.Allow() {
//	    // rate limited
//	}
func NewLimiter(key string, rps int) *Limiter {
	once.Do(func() {
		go GlobalLimiters.clearLimiter()
	})
	return GlobalLimiters.getLimiter(key, rps)
}

// Allow reports whether the request is allowed under the rate limit.
// It is safe to call concurrently from multiple goroutines.
func (l *Limiter) Allow() bool {
	l.lastGet.Store(time.Now().UnixNano())
	return l.limiter.Allow()
}

// getLimiter retrieves an existing limiter or atomically creates a new one.
// Uses LoadOrStore to eliminate the race condition where two goroutines
// both see a missing key and each create a separate limiter.
func (ls *Limiters) getLimiter(key string, rps int) *Limiter {
	if v, ok := ls.limiters.Load(key); ok {
		return v.(*Limiter)
	}

	l := &Limiter{
		// 实例化一个限流器，桶的容量是1，每秒生成一个令牌
		// 1.第一个参数是 r Limit。代表每秒可以向 Token 桶中产生多少 token。Limit 实际上是 float64 的别名
		// 2.第二个参数是 b int。b 代表 初始并发量,看做是桶的容量。
		limiter: rate.NewLimiter(rate.Limit(rps), rps),
		// lastGet 不需要初始化 — Allow() 会立即更新.
		key: key,
	}
	l.lastGet.Store(time.Now().UnixNano())

	if actual, loaded := ls.limiters.LoadOrStore(key, l); loaded {
		return actual.(*Limiter)
	}
	return l
}

// idleThreshold is the maximum idle duration before a limiter is removed by cleanup.
const idleThreshold = 5 * time.Minute

// clearLimiter runs in a background goroutine and periodically removes idle limiters.
func (ls *Limiters) clearLimiter() {
	for {
		time.Sleep(1 * time.Minute)
		ls.clearOnce()
	}
}

// clearOnce performs a single cleanup pass, removing limiters that have not been
// accessed within idleThreshold. Extracted for testability.
func (ls *Limiters) clearOnce() {
	now := time.Now().UnixNano()
	ls.limiters.Range(func(key, value any) bool {
		lim := value.(*Limiter)
		if now-lim.lastGet.Load() > int64(idleThreshold) {
			ls.limiters.Delete(key)
		}
		return true // 始终返回 true，遍历所有 key.
	})
}
