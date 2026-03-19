package golimit

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// KeyFunc extracts the rate limiting key from a Gin request.
type KeyFunc func(c *gin.Context) string

// ErrorHandler handles the response when a request is rate limited.
type ErrorHandler func(c *gin.Context)

// SkipFunc determines whether to bypass rate limiting for a request.
// Returning true means the request will not be checked.
type SkipFunc func(c *gin.Context) bool

// MiddlewareConfig holds the Gin middleware configuration.
// Only Limiter is required — everything else has sensible defaults.
type MiddlewareConfig struct {
	// Limiter is the rate limiter instance (required).
	Limiter *Limiter

	// KeyFunc extracts the rate limit key from each request.
	// Defaults to ClientIP + ":" + URL.Path.
	KeyFunc KeyFunc

	// ErrorHandler is called when a request is rejected.
	// Defaults to a 429 JSON response with standard headers.
	ErrorHandler ErrorHandler

	// Skip determines whether to bypass rate limiting.
	// Nil means never skip.
	Skip SkipFunc
}

// Middleware creates a Gin middleware from a MiddlewareConfig.
// This is the most flexible entry point — use the convenience constructors
// below for common cases.
//
// Usage:
//
//	lim := golimit.New(100, golimit.WithBurst(200))
//	r.Use(golimit.Middleware(golimit.MiddlewareConfig{
//	    Limiter: lim,
//	    Skip:    golimit.SkipPaths("/health", "/metrics"),
//	}))
func Middleware(cfg MiddlewareConfig) gin.HandlerFunc {
	if cfg.Limiter == nil {
		panic("golimit: Limiter is required")
	}
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = defaultKeyFunc
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = defaultErrorHandler(cfg.Limiter)
	}

	return func(c *gin.Context) {
		if cfg.Skip != nil && cfg.Skip(c) {
			c.Next()
			return
		}

		key := cfg.KeyFunc(c)

		if !cfg.Limiter.Allow(key) {
			cfg.ErrorHandler(c)
			return
		}

		// Set standard rate limit headers on successful requests.
		setRateLimitHeaders(c, cfg.Limiter, key)
		c.Next()
	}
}

// GinMiddleware creates a Gin middleware with default settings.
// This is the simplest way to add rate limiting — one line.
//
// Usage:
//
//	lim := golimit.New(100)
//	r.Use(golimit.GinMiddleware(lim))
func GinMiddleware(l *Limiter) gin.HandlerFunc {
	return Middleware(MiddlewareConfig{Limiter: l})
}

// IPLimit creates a per-IP rate limiting middleware.
//
// Usage:
//
//	r.Use(golimit.IPLimit(100))              // 100 req/s per IP.
//	r.Use(golimit.IPLimit(100, golimit.WithBurst(200)))
func IPLimit(rps float64, opts ...Option) gin.HandlerFunc {
	l := New(rps, opts...)
	return Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			return "ip:" + c.ClientIP()
		},
	})
}

// PathIPLimit creates a per-IP-per-path rate limiting middleware.
//
// Usage:
//
//	r.Use(golimit.PathIPLimit(10))           // 10 req/s per IP per path.
func PathIPLimit(rps float64, opts ...Option) gin.HandlerFunc {
	l := New(rps, opts...)
	return Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			return c.ClientIP() + ":" + c.Request.URL.Path
		},
	})
}

// UserLimit creates a per-user rate limiting middleware.
// getUserID should extract the user ID from the request (e.g., from JWT claims).
// Anonymous users fall back to IP-based limiting.
//
// Usage:
//
//	r.Use(golimit.UserLimit(60, func(c *gin.Context) string {
//	    return c.GetString("user_id")
//	}))
func UserLimit(rps float64, getUserID func(c *gin.Context) string, opts ...Option) gin.HandlerFunc {
	l := New(rps, opts...)
	return Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			uid := getUserID(c)
			if uid == "" {
				return "anon:" + c.ClientIP()
			}
			return "user:" + uid
		},
	})
}

// ================== Skip Helpers ==================

// SkipPaths returns a SkipFunc that bypasses rate limiting for the given URL paths.
func SkipPaths(paths ...string) SkipFunc {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return func(c *gin.Context) bool {
		_, ok := set[c.Request.URL.Path]
		return ok
	}
}

// SkipIPs returns a SkipFunc that bypasses rate limiting for the given IP whitelist.
func SkipIPs(ips ...string) SkipFunc {
	set := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		set[ip] = struct{}{}
	}
	return func(c *gin.Context) bool {
		_, ok := set[c.ClientIP()]
		return ok
	}
}

// SkipMethods returns a SkipFunc that bypasses rate limiting for the given HTTP methods.
func SkipMethods(methods ...string) SkipFunc {
	set := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		set[m] = struct{}{}
	}
	return func(c *gin.Context) bool {
		_, ok := set[c.Request.Method]
		return ok
	}
}

// CombineSkips returns a SkipFunc that skips if any of the given functions returns true.
func CombineSkips(fns ...SkipFunc) SkipFunc {
	return func(c *gin.Context) bool {
		for _, fn := range fns {
			if fn(c) {
				return true
			}
		}
		return false
	}
}

// ================== Internal Helpers ==================

// defaultKeyFunc uses ClientIP + URL.Path as the rate limit key.
func defaultKeyFunc(c *gin.Context) string {
	return c.ClientIP() + ":" + c.Request.URL.Path
}

// defaultErrorHandler returns a handler that sends a 429 JSON response with headers.
func defaultErrorHandler(l *Limiter) ErrorHandler {
	return func(c *gin.Context) {
		c.Header("X-RateLimit-Limit", l.FormatBurst())
		c.Header("X-RateLimit-Remaining", "0")
		c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10))
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"code":    http.StatusTooManyRequests,
			"message": "rate limit exceeded",
		})
	}
}

// setRateLimitHeaders writes standard X-RateLimit-* headers to successful responses.
func setRateLimitHeaders(c *gin.Context, l *Limiter, key string) {
	c.Header("X-RateLimit-Limit", l.FormatBurst())
	c.Header("X-RateLimit-Remaining", strconv.FormatFloat(l.Tokens(key), 'f', 0, 64))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10))
}
