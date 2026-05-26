package golimitgin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	golimit "github.com/gtkit/golimit/v2"
)

func init() {
	gin.SetMode(gin.TestMode)
}

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
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	req.RemoteAddr = ip + ":12345"
	r.ServeHTTP(w, req)
	return w
}

// ================== 基础中间件测试 ==================

func TestMiddleware_Basic(t *testing.T) {
	l := golimit.New(5)
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
	l := golimit.New(100)
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
	l := golimit.New(1)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	doRequest(r, "/test", "10.0.0.1") // 用尽桶.
	w := doRequest(r, "/test", "10.0.0.1")

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d.", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 响应应包含 Retry-After 头.")
	}
	if w.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("429 响应的 X-RateLimit-Remaining 应为 0,实际 %s.", w.Header().Get("X-RateLimit-Remaining"))
	}
}

func TestMiddleware_DifferentIPs(t *testing.T) {
	l := golimit.New(2)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	// 耗尽 IP1.
	doRequest(r, "/test", "10.0.0.1")
	doRequest(r, "/test", "10.0.0.1")

	// IP2 应独立不受影响.
	w := doRequest(r, "/test", "10.0.0.2")
	if w.Code != http.StatusOK {
		t.Errorf("不同 IP 应允许,实际 %d.", w.Code)
	}
}

// ================== Skip 测试 ==================

func TestMiddleware_Skip(t *testing.T) {
	l := golimit.New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		Skip:    SkipPaths("/health"),
	}))

	// /health 不论桶是否用尽都应通过.
	for range 10 {
		w := doRequest(r, "/health", "10.0.0.1")
		if w.Code != http.StatusOK {
			t.Errorf("skip 路径应始终返回 200,实际 %d.", w.Code)
		}
	}
}

func TestMiddleware_SkipIPs(t *testing.T) {
	l := golimit.New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		Skip:    SkipIPs("10.0.0.99"),
	}))

	for range 10 {
		w := doRequest(r, "/test", "10.0.0.99")
		if w.Code != http.StatusOK {
			t.Errorf("白名单 IP 应始终通过,实际 %d.", w.Code)
		}
	}
}

func TestMiddleware_CombineSkips(t *testing.T) {
	l := golimit.New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		Skip: CombineSkips(
			SkipPaths("/health"),
			SkipIPs("10.0.0.99"),
		),
	}))

	// 提前耗尽非 skip 路径的桶.
	doRequest(r, "/test", "10.0.0.1")
	doRequest(r, "/test", "10.0.0.1")

	w1 := doRequest(r, "/health", "10.0.0.1")
	w2 := doRequest(r, "/test", "10.0.0.99")
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Errorf("组合 skip 都应通过:health=%d,whitelistIP=%d.", w1.Code, w2.Code)
	}
}

func TestMiddleware_SkipMethods(t *testing.T) {
	l := golimit.New(1)
	defer l.Close()
	mw := Middleware(MiddlewareConfig{
		Limiter: l,
		Skip:    SkipMethods(http.MethodOptions),
	})

	r := gin.New()
	r.Use(mw)
	r.OPTIONS("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// OPTIONS 应始终通过.
	for range 5 {
		w := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, "/test", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("OPTIONS 应跳过限流,实际 %d.", w.Code)
		}
	}
}

// ================== 自定义函数测试 ==================

func TestMiddleware_CustomKeyFunc(t *testing.T) {
	l := golimit.New(2)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			return c.GetHeader("X-User-ID")
		},
	}))

	// 同 IP 不同 user 头 → 不同 key.
	for range 2 {
		w := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-User-ID", "user-A")
		r.ServeHTTP(w, req)
	}

	// user-A 已耗尽,但 user-B 应仍可访问.
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-User-ID", "user-B")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("不同 user key 应允许,实际 %d.", w.Code)
	}
}

func TestMiddleware_CustomErrorHandler(t *testing.T) {
	var (
		called    atomic.Bool
		gotRetry  atomic.Int64 // 检查回调收到的 RetryAfter 是否被正确填充.
		gotLimit  atomic.Int64
		gotReason atomic.Value
	)

	l := golimit.New(1)
	defer l.Close()
	r := newTestRouter(Middleware(MiddlewareConfig{
		Limiter: l,
		ErrorHandler: func(c *gin.Context, result golimit.Result) {
			called.Store(true)
			gotRetry.Store(int64(result.RetryAfter.Seconds()))
			gotLimit.Store(int64(result.Limit))
			gotReason.Store(string(result.Reason))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"custom": true})
		},
	}))

	doRequest(r, "/test", "10.0.0.1") // 用尽.
	doRequest(r, "/test", "10.0.0.1") // 触发自定义 handler.

	if !called.Load() {
		t.Fatal("自定义 ErrorHandler 未被调用.")
	}
	if gotLimit.Load() != 1 {
		t.Errorf("ErrorHandler 收到的 Limit 应为 1,实际 %d.", gotLimit.Load())
	}
	if gotRetry.Load() != 1 {
		t.Errorf("ErrorHandler 收到的 RetryAfter 应为 1 秒,实际 %d.", gotRetry.Load())
	}
	if r := gotReason.Load(); r == nil || r.(string) != string(golimit.RejectRate) {
		t.Errorf("ErrorHandler 收到的 Reason 应为 RejectRate,实际 %v.", r)
	}
}

// ================== 便捷构造器测试 ==================

func TestIPLimit(t *testing.T) {
	mw := IPLimit(3)
	r := newTestRouter(mw)

	for range 3 {
		w := doRequest(r, "/test", "10.0.0.1")
		if w.Code != http.StatusOK {
			t.Error("配额内应允许.")
		}
	}
	w := doRequest(r, "/test", "10.0.0.1")
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("应被拒绝,实际 %d.", w.Code)
	}
}

func TestPathIPLimit(t *testing.T) {
	mw := PathIPLimit(2)
	r := gin.New()
	r.GET("/a", mw, func(c *gin.Context) { c.String(http.StatusOK, "a") })
	r.GET("/b", mw, func(c *gin.Context) { c.String(http.StatusOK, "b") })

	// 耗尽 /a.
	doReq := func(path string) int {
		w := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.1:1234"
		r.ServeHTTP(w, req)
		return w.Code
	}

	doReq("/a")
	doReq("/a")
	if code := doReq("/a"); code != http.StatusTooManyRequests {
		t.Errorf("/a 应已耗尽,实际 %d.", code)
	}
	// /b 应独立.
	if code := doReq("/b"); code != http.StatusOK {
		t.Errorf("/b 应允许,实际 %d.", code)
	}
}

func TestUserLimit(t *testing.T) {
	mw := UserLimit(2, func(c *gin.Context) string {
		return c.GetHeader("X-User-ID")
	})
	r := newTestRouter(mw)

	sendAs := func(uid string) int {
		w := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		if uid != "" {
			req.Header.Set("X-User-ID", uid)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	sendAs("alice")
	sendAs("alice")
	if code := sendAs("alice"); code != http.StatusTooManyRequests {
		t.Errorf("alice 应已耗尽,实际 %d.", code)
	}
	if code := sendAs("bob"); code != http.StatusOK {
		t.Errorf("bob 应允许,实际 %d.", code)
	}
}

// ================== *WithLimiter 变体 ==================

func TestIPLimitWithLimiter_CloseStopsCleanup(t *testing.T) {
	mw, lim := IPLimitWithLimiter(10)
	if mw == nil {
		t.Fatal("middleware 不应为 nil.")
	}
	if lim == nil {
		t.Fatal("返回的 *Limiter 不应为 nil.")
	}
	// 关闭不应 panic / 死锁.
	lim.Close()
}

// ================== 并发 ==================

func TestMiddleware_Concurrency(t *testing.T) {
	// rate=50/s + burst=50:测试期间(< 100ms)最多补充 ~5 个令牌,可以稳定验证 burst 边界.
	l := golimit.New(50, golimit.WithBurst(50))
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	var allowed atomic.Int64
	var wg sync.WaitGroup

	for range 200 {
		wg.Go(func() {
			for range 10 {
				w := doRequest(r, "/test", "10.0.0.1")
				if w.Code == http.StatusOK {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	a := allowed.Load()
	t.Logf("中间件并发:allowed=%d (burst=50).", a)
	if a > 60 {
		t.Errorf("放过 %d 个请求,超过 burst 50 + margin,可能存在 race.", a)
	}
}

// ================== Benchmark ==================

func BenchmarkMiddleware(b *testing.B) {
	l := golimit.New(1000000)
	defer l.Close()
	r := newTestRouter(GinMiddleware(l))

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			req, _ := http.NewRequestWithContext(b.Context(), http.MethodGet, "/test", nil)
			req.RemoteAddr = "10.0.0.1:1234"
			r.ServeHTTP(w, req)
		}
	})
}
