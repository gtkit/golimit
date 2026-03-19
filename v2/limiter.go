// Package golimit provides production-grade, per-key rate limiting
// based on golang.org/x/time/rate (token bucket algorithm).
//
// v2 redesign: the limiter is an instance (not a global singleton),
// supports graceful shutdown via Close(), and integrates directly
// with Gin as middleware while remaining usable standalone.
package golimit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Limiter manages per-key token bucket rate limiters with automatic cleanup.
// It is safe for concurrent use from multiple goroutines.
//
// Each unique key gets its own independent rate.Limiter stored in a sync.Map,
// so different keys (IPs, users, paths) never contend on the same lock.
type Limiter struct {
	rate  float64 // Tokens per second.
	burst int     // Maximum burst size.

	visitors sync.Map // map[string]*visitor.
	stopCh   chan struct{}
	wg       sync.WaitGroup

	cleanupInterval time.Duration
	maxIdleTime     time.Duration
}

// visitor tracks a per-key rate limiter and its last access time.
type visitor struct {
	lim      *rate.Limiter
	lastSeen atomic.Int64 // UnixNano, updated atomically — no lock needed.
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithBurst sets the maximum burst size.
// Defaults to the same value as rate if unset.
func WithBurst(burst int) Option {
	return func(l *Limiter) {
		l.burst = burst
	}
}

// WithCleanupInterval sets how often the background goroutine scans for idle entries.
// Defaults to 1 minute.
func WithCleanupInterval(d time.Duration) Option {
	return func(l *Limiter) {
		l.cleanupInterval = d
	}
}

// WithMaxIdleTime sets how long a key must be idle before it is removed.
// Defaults to 5 minutes.
func WithMaxIdleTime(d time.Duration) Option {
	return func(l *Limiter) {
		l.maxIdleTime = d
	}
}

// New creates a rate limiter that allows rps requests per second per key.
// It starts a background cleanup goroutine that is stopped by calling Close.
//
// Usage:
//
//	lim := golimit.New(100)                      // 100 req/s, burst=100.
//	lim := golimit.New(100, golimit.WithBurst(200)) // 100 req/s, burst=200.
//	defer lim.Close()
func New(rps float64, opts ...Option) *Limiter {
	if rps <= 0 {
		rps = 100
	}

	l := &Limiter{
		rate:            rps,
		burst:           int(rps),
		stopCh:          make(chan struct{}),
		cleanupInterval: time.Minute,
		maxIdleTime:     5 * time.Minute,
	}
	for _, opt := range opts {
		opt(l)
	}
	if l.burst <= 0 {
		l.burst = int(rps)
	}

	l.wg.Add(1)
	go l.cleanupLoop()

	return l
}

// Allow reports whether a request for the given key is allowed.
// It is safe to call concurrently from any number of goroutines.
func (l *Limiter) Allow(key string) bool {
	v := l.getOrCreate(key)
	v.lastSeen.Store(time.Now().UnixNano())
	return v.lim.Allow()
}

// AllowN reports whether n requests for the given key are allowed.
func (l *Limiter) AllowN(key string, n int) bool {
	v := l.getOrCreate(key)
	v.lastSeen.Store(time.Now().UnixNano())
	return v.lim.AllowN(time.Now(), n)
}

// Tokens returns the approximate number of available tokens for the given key.
// Returns burst if the key has not been seen before.
func (l *Limiter) Tokens(key string) float64 {
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor).lim.Tokens()
	}
	return float64(l.burst)
}

// Reset removes the rate limit state for the given key.
// The next request for this key starts with a full burst allowance.
func (l *Limiter) Reset(key string) {
	l.visitors.Delete(key)
}

// Close stops the background cleanup goroutine and waits for it to exit.
// After Close returns, the Limiter should not be used.
func (l *Limiter) Close() {
	close(l.stopCh)
	l.wg.Wait()
}

// Rate returns the configured requests-per-second value.
func (l *Limiter) Rate() float64 {
	return l.rate
}

// Burst returns the configured burst size.
func (l *Limiter) Burst() int {
	return l.burst
}

// FormatRate returns Rate as a string (convenience for headers).
func (l *Limiter) FormatRate() string {
	return strconv.FormatFloat(l.rate, 'f', -1, 64)
}

// FormatBurst returns Burst as a string (convenience for headers).
func (l *Limiter) FormatBurst() string {
	return strconv.Itoa(l.burst)
}

// getOrCreate retrieves or atomically creates the visitor for a key.
func (l *Limiter) getOrCreate(key string) *visitor {
	if v, ok := l.visitors.Load(key); ok {
		return v.(*visitor)
	}

	v := &visitor{
		lim: rate.NewLimiter(rate.Limit(l.rate), l.burst),
	}
	if actual, loaded := l.visitors.LoadOrStore(key, v); loaded {
		return actual.(*visitor)
	}
	return v
}

// cleanupLoop periodically removes idle visitors.
func (l *Limiter) cleanupLoop() {
	defer l.wg.Done()
	ticker := time.NewTicker(l.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stopCh:
			return
		}
	}
}

// cleanup removes visitors that have been idle longer than maxIdleTime.
func (l *Limiter) cleanup() {
	threshold := time.Now().Add(-l.maxIdleTime).UnixNano()

	l.visitors.Range(func(key, value any) bool {
		v := value.(*visitor)
		if v.lastSeen.Load() < threshold {
			l.visitors.Delete(key)
		}
		return true
	})
}
