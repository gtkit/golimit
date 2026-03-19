package golimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ================== Core Limiter Tests ==================

func TestAllow_Basic(t *testing.T) {
	l := New(5)
	defer l.Close()

	for i := range 5 {
		if !l.Allow("k") {
			t.Errorf("request %d should be allowed.", i+1)
		}
	}
	if l.Allow("k") {
		t.Error("request 6 should be rejected.")
	}
}

func TestAllow_IndependentKeys(t *testing.T) {
	l := New(3)
	defer l.Close()

	for range 3 {
		l.Allow("a")
	}
	if l.Allow("a") {
		t.Error("key a should be exhausted.")
	}
	if !l.Allow("b") {
		t.Error("key b should be independent and allowed.")
	}
}

func TestAllowN(t *testing.T) {
	l := New(10)
	defer l.Close()

	if !l.AllowN("k", 5) {
		t.Error("AllowN(5) should succeed with 10 burst.")
	}
	if l.AllowN("k", 6) {
		t.Error("AllowN(6) should fail with only 5 tokens left.")
	}
}

func TestTokenRecovery(t *testing.T) {
	l := New(10, WithBurst(1))
	defer l.Close()

	if !l.Allow("k") {
		t.Fatal("first request should pass.")
	}
	if l.Allow("k") {
		t.Fatal("second request should be rejected.")
	}

	// 10 rps, burst=1 → 1 token recovered in 100ms.
	time.Sleep(150 * time.Millisecond)

	if !l.Allow("k") {
		t.Error("should be allowed after token recovery.")
	}
}

func TestRateSemantic(t *testing.T) {
	l := New(100) // 100 tokens/sec.
	defer l.Close()

	// Exhaust burst.
	for range 100 {
		l.Allow("k")
	}
	// Wait 100ms → should recover ~10 tokens.
	time.Sleep(100 * time.Millisecond)

	allowed := 0
	for range 20 {
		if l.Allow("k") {
			allowed++
		}
	}
	if allowed < 5 {
		t.Errorf("expected >= 5 tokens recovered at 100 rps after 100ms, got %d.", allowed)
	}
	t.Logf("rate semantic: %d tokens recovered after 100ms.", allowed)
}

func TestWithBurst(t *testing.T) {
	l := New(10, WithBurst(20))
	defer l.Close()

	allowed := 0
	for range 25 {
		if l.Allow("k") {
			allowed++
		}
	}
	if allowed != 20 {
		t.Errorf("expected 20 allowed (burst=20), got %d.", allowed)
	}
}

func TestReset(t *testing.T) {
	l := New(5)
	defer l.Close()

	for range 5 {
		l.Allow("k")
	}
	if l.Allow("k") {
		t.Error("should be rejected before reset.")
	}

	l.Reset("k")

	if !l.Allow("k") {
		t.Error("should be allowed after reset.")
	}
}

func TestTokens(t *testing.T) {
	l := New(10)
	defer l.Close()

	// Unseen key returns full burst.
	if tok := l.Tokens("new"); tok != 10 {
		t.Errorf("unseen key should have burst tokens, got %f.", tok)
	}

	l.Allow("k")
	tok := l.Tokens("k")
	if tok < 8 || tok > 10 {
		t.Errorf("after 1 Allow, tokens should be ~9, got %f.", tok)
	}
}

func TestClose(t *testing.T) {
	l := New(100)
	l.Allow("k")
	l.Close()
	// After Close, the cleanup goroutine should have exited.
	// We just verify no panic or deadlock.
}

func TestZeroRate(t *testing.T) {
	l := New(0) // Should use default 100.
	defer l.Close()

	if !l.Allow("k") {
		t.Error("should use default rate=100 and allow the first request.")
	}
}

// ================== Concurrency Tests ==================

func TestConcurrency_Correctness(t *testing.T) {
	const burst = 50
	l := New(float64(burst)) // High rate = burst, so no refill interference.
	defer l.Close()

	var allowed atomic.Int64
	var wg sync.WaitGroup

	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if l.Allow("strict") {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	a := allowed.Load()
	t.Logf("concurrency: allowed=%d / total=2000 (burst=%d).", a, burst)
	// Allow small margin for token refill during test.
	if a > int64(burst)+5 {
		t.Errorf("CRITICAL: allowed %d > burst %d. Race condition.", a, burst)
	}
}

func TestConcurrency_NoLoss(t *testing.T) {
	l := New(1000)
	defer l.Close()

	var allowed, rejected atomic.Int64
	var wg sync.WaitGroup

	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if l.Allow("k") {
					allowed.Add(1)
				} else {
					rejected.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	total := allowed.Load() + rejected.Load()
	if total != 10000 {
		t.Errorf("expected 10000 total, got %d.", total)
	}
}

func TestConcurrency_MultiKey(t *testing.T) {
	const burst = 10
	l := New(float64(burst))
	defer l.Close()

	var wg sync.WaitGroup
	results := make([]atomic.Int64, 20)

	for i := range 20 {
		key := fmt.Sprintf("key-%d", i)
		for range 10 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				for range 5 {
					if l.Allow(key) {
						results[idx].Add(1)
					}
				}
			}(i)
		}
	}
	wg.Wait()

	for i, r := range results {
		if a := r.Load(); a > int64(burst)+3 {
			t.Errorf("key %d: allowed %d > burst %d.", i, a, burst)
		}
	}
}

func TestConcurrency_LoadOrStore(t *testing.T) {
	l := New(10)
	defer l.Close()

	var ptrs [100]*visitor
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ptrs[idx] = l.getOrCreate("race-key")
		}(i)
	}
	wg.Wait()

	for i := 1; i < 100; i++ {
		if ptrs[i] != ptrs[0] {
			t.Fatalf("goroutine %d got a different visitor — LoadOrStore race.", i)
		}
	}
}

// ================== Cleanup Tests ==================

func TestCleanup(t *testing.T) {
	l := New(10, WithCleanupInterval(50*time.Millisecond), WithMaxIdleTime(100*time.Millisecond))
	defer l.Close()

	l.Allow("should-expire")
	// Wait for idle + cleanup cycle.
	time.Sleep(250 * time.Millisecond)

	// The expired key should have been cleaned up.
	// Create it again — if cleanup worked, Tokens returns full burst.
	if tok := l.Tokens("should-expire"); tok != 10 {
		t.Errorf("expected full burst after cleanup, got %f.", tok)
	}
}

func TestCleanup_ActiveKeysSurvive(t *testing.T) {
	l := New(100, WithCleanupInterval(50*time.Millisecond), WithMaxIdleTime(200*time.Millisecond))
	defer l.Close()

	// Keep one key active, let the other expire.
	l.Allow("active")
	l.Allow("idle")

	time.Sleep(100 * time.Millisecond)
	l.Allow("active") // Refresh active key.
	time.Sleep(150 * time.Millisecond)

	// Active key should still have its state (not full burst).
	activeTok := l.Tokens("active")
	idleTok := l.Tokens("idle")

	if activeTok >= float64(l.Burst()) {
		t.Error("active key should have consumed tokens, not be full.")
	}
	if idleTok != float64(l.Burst()) {
		t.Errorf("idle key should have been cleaned up and return full burst, got %f.", idleTok)
	}
}

// ================== Gin Middleware Tests ==================

func newTestRouter(mw gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.GET("/test", mw, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	r.GET("/health", mw, func(c *gin.Context) {
		c.String(http.StatusOK, "healthy")
	})
	return r
}

func doRequest(r *gin.Engine, path, ip string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", path, nil)
	req.RemoteAddr = ip + ":12345"
	r.ServeHTTP(w, req)
	return w
}

func TestMiddleware_Basic(t *testing.T) {
	l := New(5)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	for i := range 5 {
		w := doRequest(r, "/test", "10.0.0.1")
		if w.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d.", i+1, w.Code)
		}
	}

	w := doRequest(r, "/test", "10.0.0.1")
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("request 6: expected 429, got %d.", w.Code)
	}
}

func TestMiddleware_Headers(t *testing.T) {
	l := New(100)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	w := doRequest(r, "/test", "10.0.0.1")

	for _, h := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing header %s.", h)
		}
	}
}

func TestMiddleware_429Headers(t *testing.T) {
	l := New(1)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	doRequest(r, "/test", "10.0.0.1") // Exhaust.
	w := doRequest(r, "/test", "10.0.0.1")

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d.", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 response should include Retry-After header.")
	}
	if w.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("429 response should have X-RateLimit-Remaining: 0, got %s.", w.Header().Get("X-RateLimit-Remaining"))
	}
}

func TestMiddleware_DifferentIPs(t *testing.T) {
	l := New(2)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	// Exhaust IP1.
	doRequest(r, "/test", "10.0.0.1")
	doRequest(r, "/test", "10.0.0.1")

	// IP2 should be independent.
	w := doRequest(r, "/test", "10.0.0.2")
	if w.Code != http.StatusOK {
		t.Errorf("different IP should be allowed, got %d.", w.Code)
	}
}

func TestMiddleware_Skip(t *testing.T) {
	l := New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		Skip:    SkipPaths("/health"),
	}))

	// /health should always pass, even after exhausting the limiter.
	for range 10 {
		w := doRequest(r, "/health", "10.0.0.1")
		if w.Code != http.StatusOK {
			t.Errorf("skipped path should always return 200, got %d.", w.Code)
		}
	}
}

func TestMiddleware_SkipIPs(t *testing.T) {
	l := New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		Skip:    SkipIPs("10.0.0.99"),
	}))

	for range 10 {
		w := doRequest(r, "/test", "10.0.0.99")
		if w.Code != http.StatusOK {
			t.Errorf("whitelisted IP should always pass, got %d.", w.Code)
		}
	}
}

func TestMiddleware_CombineSkips(t *testing.T) {
	l := New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		Skip: CombineSkips(
			SkipPaths("/health"),
			SkipIPs("10.0.0.99"),
		),
	}))

	// Both should be skipped.
	doRequest(r, "/test", "10.0.0.1") // Exhaust the limiter for non-skipped.
	doRequest(r, "/test", "10.0.0.1")

	w1 := doRequest(r, "/health", "10.0.0.1")
	w2 := doRequest(r, "/test", "10.0.0.99")
	if w1.Code != 200 || w2.Code != 200 {
		t.Errorf("combined skips should pass: health=%d, whitelistIP=%d.", w1.Code, w2.Code)
	}
}

func TestMiddleware_CustomKeyFunc(t *testing.T) {
	l := New(2)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			return c.GetHeader("X-User-ID")
		},
	}))

	// Same IP, different user headers → different keys.
	for range 2 {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-User-ID", "user-A")
		r.ServeHTTP(w, req)
	}

	// user-A exhausted, but user-B should still work.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-User-ID", "user-B")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("different user key should be allowed, got %d.", w.Code)
	}
}

func TestMiddleware_CustomErrorHandler(t *testing.T) {
	var called atomic.Bool

	l := New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		ErrorHandler: func(c *gin.Context) {
			called.Store(true)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"custom": true})
		},
	}))

	doRequest(r, "/test", "10.0.0.1") // Exhaust.
	doRequest(r, "/test", "10.0.0.1") // Trigger custom handler.

	if !called.Load() {
		t.Error("custom error handler was not called.")
	}
}

func TestIPLimit(t *testing.T) {
	mw := IPLimit(3)
	r := newTestRouter(mw)

	for range 3 {
		w := doRequest(r, "/test", "10.0.0.1")
		if w.Code != 200 {
			t.Error("should be allowed within limit.")
		}
	}
	w := doRequest(r, "/test", "10.0.0.1")
	if w.Code != 429 {
		t.Errorf("should be rejected, got %d.", w.Code)
	}
}

func TestPathIPLimit(t *testing.T) {
	mw := PathIPLimit(2)
	r := gin.New()
	r.GET("/a", mw, func(c *gin.Context) { c.String(200, "a") })
	r.GET("/b", mw, func(c *gin.Context) { c.String(200, "b") })

	// Exhaust /a.
	doReq := func(path string) int {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", path, nil)
		req.RemoteAddr = "10.0.0.1:1234"
		r.ServeHTTP(w, req)
		return w.Code
	}

	doReq("/a")
	doReq("/a")
	if code := doReq("/a"); code != 429 {
		t.Errorf("/a should be exhausted, got %d.", code)
	}
	// /b should be independent.
	if code := doReq("/b"); code != 200 {
		t.Errorf("/b should be allowed, got %d.", code)
	}
}

func TestUserLimit(t *testing.T) {
	mw := UserLimit(2, func(c *gin.Context) string {
		return c.GetHeader("X-User-ID")
	})
	r := newTestRouter(mw)

	sendAs := func(uid string) int {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		if uid != "" {
			req.Header.Set("X-User-ID", uid)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	sendAs("alice")
	sendAs("alice")
	if code := sendAs("alice"); code != 429 {
		t.Errorf("alice should be exhausted, got %d.", code)
	}
	if code := sendAs("bob"); code != 200 {
		t.Errorf("bob should be allowed, got %d.", code)
	}
}

func TestMiddleware_Concurrency(t *testing.T) {
	l := New(10000, WithBurst(50))
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	var allowed atomic.Int64
	var wg sync.WaitGroup

	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				w := doRequest(r, "/test", "10.0.0.1")
				if w.Code == 200 {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	a := allowed.Load()
	t.Logf("middleware concurrency: allowed=%d (burst=50).", a)
	if a > 55 {
		t.Errorf("allowed %d > burst 50 + margin. Race condition.", a)
	}
}

// ================== Benchmarks ==================

func BenchmarkAllow_SingleKey(b *testing.B) {
	l := New(1000000)
	defer l.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Allow("bench")
		}
	})
}

func BenchmarkAllow_MultiKey(b *testing.B) {
	l := New(1000000)
	defer l.Close()

	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.Allow(keys[i%len(keys)])
			i++
		}
	})
}

func BenchmarkMiddleware(b *testing.B) {
	l := New(1000000)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "10.0.0.1:1234"
			r.ServeHTTP(w, req)
		}
	})
}
