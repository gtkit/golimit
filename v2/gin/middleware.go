// Package golimitgin 是 golimit/v2 的 Gin 框架适配层.
//
// 拆分原因:核心限流库 golimit/v2 坚持零第三方依赖,所有 Web 框架集成
// 通过独立 sub-module 提供.gin 的 27 个 indirect 依赖只影响显式 import 本包的用户.
//
// 用法:
//
//	import (
//	    "github.com/gtkit/golimit/v2"
//	    golimitgin "github.com/gtkit/golimit/v2/gin"
//	)
//
//	r := gin.New()
//	r.Use(golimitgin.IPLimit(100))
package golimitgin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	golimit "github.com/gtkit/golimit/v2"
)

// KeyFunc 从 Gin 请求中提取限流 key.
//
// 返回 "" 视为 KeyFunc 无法识别身份 — Middleware 默认回退到 ClientIP 兜底.
// 调用方若希望对空 key 拒绝,请在 KeyFunc 内返回明确的 sentinel(例如 "anonymous").
type KeyFunc func(c *gin.Context) string

// ErrorHandler 处理请求被限流时的响应.
type ErrorHandler func(c *gin.Context, result golimit.Result)

// SkipFunc 决定某个请求是否跳过限流检查.返回 true 即跳过.
type SkipFunc func(c *gin.Context) bool

// MiddlewareConfig 保存 Gin 中间件配置.
// 只有 Limiter 是必填的,其余均有合理默认值.
type MiddlewareConfig struct {
	// Limiter 是限流器实例(必填).
	Limiter *golimit.Limiter

	// KeyFunc 从请求中提取限流 key.
	// 默认使用 ClientIP + ":" + URL.Path.
	//
	// 安全提示:c.ClientIP() 默认信任 X-Forwarded-For 等代理头.公网部署务必
	// 调用 engine.SetTrustedProxies(...) 限制信任范围,否则攻击者可伪造头绕过限流.
	KeyFunc KeyFunc

	// ErrorHandler 在请求被拒时调用.
	// 默认返回 429 JSON 响应,并写标准限流响应头.
	// 自定义 Handler 收到的 result 已经填充了 RetryAfter / Limit / Reason.
	ErrorHandler ErrorHandler

	// Skip 决定是否跳过限流.nil 表示从不跳过.
	Skip SkipFunc
}

// Middleware 根据 MiddlewareConfig 创建 Gin 中间件.
// 这是最灵活的入口 — 常见场景可使用下面的便捷构造器.
//
// 用法:
//
//	lim := golimit.New(100, golimit.WithBurst(200))
//	r.Use(golimitgin.Middleware(golimitgin.MiddlewareConfig{
//	    Limiter: lim,
//	    Skip:    golimitgin.SkipPaths("/health", "/metrics"),
//	}))
func Middleware(cfg MiddlewareConfig) gin.HandlerFunc {
	if cfg.Limiter == nil {
		panic("golimitgin: Limiter is required")
	}
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = defaultKeyFunc
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = defaultErrorHandler
	}

	return func(c *gin.Context) {
		if cfg.Skip != nil && cfg.Skip(c) {
			c.Next()
			return
		}

		key := cfg.KeyFunc(c)
		// 空 key 兜底:回退到 ClientIP,避免所有空 key 请求共享同一个 bucket(等于全局限流).
		if key == "" {
			key = "_empty:" + c.ClientIP()
		}

		// 通过 Check 一次性拿到限流决策 + 元信息,后续无需重复读 Tokens / 计算 RetryAfter.
		result := cfg.Limiter.Check(key)
		setRateLimitHeaders(c, result)

		if !result.Allowed {
			cfg.ErrorHandler(c, result)
			return
		}
		c.Next()
	}
}

// GinMiddleware 用默认配置创建 Gin 中间件.
// 这是接入限流最简单的方式 — 一行代码即可.
//
// 用法:
//
//	lim := golimit.New(100)
//	r.Use(golimitgin.GinMiddleware(lim))
func GinMiddleware(l *golimit.Limiter) gin.HandlerFunc {
	return Middleware(MiddlewareConfig{Limiter: l})
}

// IPLimit 创建 per-IP 限流中间件.
//
// ⚠ 注意:此便捷构造器内部创建一个 *golimit.Limiter 但**不返回引用**,
// 调用方无法 Close() → cleanup goroutine 永驻进程生命周期.
// 仅适合"进程级常驻、永不卸载"的中间件场景.若需要手动管理生命周期,
// 请使用 IPLimitWithLimiter(返回 *golimit.Limiter 引用).
//
// 用法:
//
//	r.Use(golimitgin.IPLimit(100))              // 每 IP 每秒 100 个请求.
//	r.Use(golimitgin.IPLimit(100, golimit.WithBurst(200)))
func IPLimit(rps float64, opts ...golimit.Option) gin.HandlerFunc {
	mw, _ := IPLimitWithLimiter(rps, opts...)
	return mw
}

// IPLimitWithLimiter 等价于 IPLimit,额外返回内部 *golimit.Limiter 用于优雅关闭.
//
// 用法:
//
//	mw, lim := golimitgin.IPLimitWithLimiter(100)
//	defer lim.Close()
//	r.Use(mw)
func IPLimitWithLimiter(rps float64, opts ...golimit.Option) (gin.HandlerFunc, *golimit.Limiter) {
	l := golimit.New(rps, opts...)
	mw := Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			return "ip:" + c.ClientIP()
		},
	})
	return mw, l
}

// PathIPLimit 创建 per-IP-per-path 限流中间件.
//
// ⚠ 同 IPLimit — 内部 *golimit.Limiter 不可关闭,长期持有.需手动管理请用 PathIPLimitWithLimiter.
//
// 用法:
//
//	r.Use(golimitgin.PathIPLimit(10))           // 每 IP 每路径每秒 10 个请求.
func PathIPLimit(rps float64, opts ...golimit.Option) gin.HandlerFunc {
	mw, _ := PathIPLimitWithLimiter(rps, opts...)
	return mw
}

// PathIPLimitWithLimiter 等价于 PathIPLimit,额外返回内部 *golimit.Limiter 用于优雅关闭.
func PathIPLimitWithLimiter(rps float64, opts ...golimit.Option) (gin.HandlerFunc, *golimit.Limiter) {
	l := golimit.New(rps, opts...)
	mw := Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			return c.ClientIP() + ":" + c.Request.URL.Path
		},
	})
	return mw, l
}

// UserLimit 创建 per-user 限流中间件.
// getUserID 应从请求中提取用户 ID(例如 JWT claims).匿名用户回退到 IP 限流.
//
// ⚠ 同 IPLimit — 内部 *golimit.Limiter 不可关闭,长期持有.需手动管理请用 UserLimitWithLimiter.
//
// 用法:
//
//	r.Use(golimitgin.UserLimit(60, func(c *gin.Context) string {
//	    return c.GetString("user_id")
//	}))
func UserLimit(rps float64, getUserID func(c *gin.Context) string, opts ...golimit.Option) gin.HandlerFunc {
	mw, _ := UserLimitWithLimiter(rps, getUserID, opts...)
	return mw
}

// UserLimitWithLimiter 等价于 UserLimit,额外返回内部 *golimit.Limiter 用于优雅关闭.
func UserLimitWithLimiter(rps float64, getUserID func(c *gin.Context) string, opts ...golimit.Option) (gin.HandlerFunc, *golimit.Limiter) {
	l := golimit.New(rps, opts...)
	mw := Middleware(MiddlewareConfig{
		Limiter: l,
		KeyFunc: func(c *gin.Context) string {
			uid := getUserID(c)
			if uid == "" {
				return "anon:" + c.ClientIP()
			}
			return "user:" + uid
		},
	})
	return mw, l
}

// ================== Skip Helpers ==================

// SkipPaths 返回一个跳过指定 URL 路径的 SkipFunc.
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

// SkipIPs 返回一个跳过指定 IP 白名单的 SkipFunc.
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

// SkipMethods 返回一个跳过指定 HTTP 方法的 SkipFunc.
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

// CombineSkips 把多个 SkipFunc 合并 — 任一返回 true 即跳过.
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

// ================== 内部辅助函数 ==================

// defaultKeyFunc 用 ClientIP + URL.Path 作为限流 key.
func defaultKeyFunc(c *gin.Context) string {
	return c.ClientIP() + ":" + c.Request.URL.Path
}

// defaultErrorHandler 返回 429 JSON 响应 + Retry-After 头.
// X-RateLimit-* 头由 setRateLimitHeaders 统一在 Middleware 主流程中写,这里只补 Retry-After.
func defaultErrorHandler(c *gin.Context, result golimit.Result) {
	c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"code":    http.StatusTooManyRequests,
		"message": "rate limit exceeded",
	})
}

// setRateLimitHeaders 把标准的 X-RateLimit-* 响应头写到响应中.
// 不论允许 / 拒绝都会写,Reset 仅在允许时填充.
func setRateLimitHeaders(c *gin.Context, r golimit.Result) {
	c.Header("X-RateLimit-Limit", strconv.Itoa(r.Limit))
	c.Header("X-RateLimit-Remaining", strconv.FormatFloat(r.Remaining, 'f', 0, 64))
	if !r.ResetAt.IsZero() {
		c.Header("X-RateLimit-Reset", strconv.FormatInt(r.ResetAt.Unix(), 10))
	} else {
		// 拒绝路径:估算下次桶可用的时刻 = now + RetryAfter.
		c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(r.RetryAfter).Unix(), 10))
	}
}
