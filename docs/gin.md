# 用 golimit/v2 给 Gin 限流(复制即用)

`golimit/v2` 是**框架无关**的限流引擎,核心只暴露 `Check(key) Result`。接入任何 Web
框架都只是十几行胶水——下面是 Gin 的完整示例,直接复制到你的项目即可,**不引入额外依赖**
(gin 只进你自己的 `go.mod`,不进 golimit)。

## 基础中间件

```go
package middleware

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	golimit "github.com/gtkit/golimit/v2"
)

// RateLimit 返回一个 per-key 限流中间件.
// keyFunc 决定限流维度(按 IP / 用户 / 路径等).
func RateLimit(lim *golimit.Limiter, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		res := lim.Check(keyFunc(c))

		// 标准限流响应头.
		c.Header("X-RateLimit-Limit", strconv.Itoa(res.Limit))
		c.Header("X-RateLimit-Remaining", strconv.FormatFloat(res.Remaining, 'f', 0, 64))

		if !res.Allowed {
			c.Header("Retry-After", strconv.Itoa(int(res.RetryAfter.Seconds())))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    http.StatusTooManyRequests,
				"message": "rate limit exceeded",
			})
			return
		}
		c.Next()
	}
}
```

用法:

```go
lim := golimit.New(100, golimit.WithBurst(200))
defer lim.Close() // 进程退出时停止后台清理 goroutine.

r := gin.New()
r.Use(RateLimit(lim, func(c *gin.Context) string {
	return "ip:" + c.ClientIP() // 按 IP 限流.
}))
```

> ⚠ 公网部署务必 `r.SetTrustedProxies(...)` 限制信任的代理头,否则攻击者伪造
> `X-Forwarded-For` 即可绕过 per-IP 限流。

## 常见 KeyFunc

```go
// 按 IP:
func(c *gin.Context) string { return "ip:" + c.ClientIP() }

// 按 IP + 路径:
func(c *gin.Context) string { return c.ClientIP() + ":" + c.Request.URL.Path }

// 按用户(匿名回退 IP):
func(c *gin.Context) string {
	if uid := c.GetString("user_id"); uid != "" {
		return "user:" + uid
	}
	return "anon:" + c.ClientIP()
}
```

## 跳过部分请求(健康检查 / 白名单)

Skip 是**中间件层**的事——核心限流器只认 key,不关心 path / method / IP。用一个 `map`
查表即可,无需库提供:

```go
// skipPaths 返回一个判断"是否跳过"的函数.
func skipPaths(paths ...string) func(*gin.Context) bool {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return func(c *gin.Context) bool {
		_, ok := set[c.Request.URL.Path]
		return ok
	}
}

// 包一层:命中 skip 直接放行,否则走限流.
func RateLimitWithSkip(lim *golimit.Limiter, keyFunc func(*gin.Context) string, skip func(*gin.Context) bool) gin.HandlerFunc {
	mw := RateLimit(lim, keyFunc)
	return func(c *gin.Context) {
		if skip != nil && skip(c) {
			c.Next()
			return
		}
		mw(c)
	}
}
```

用法:

```go
r.Use(RateLimitWithSkip(lim,
	func(c *gin.Context) string { return "ip:" + c.ClientIP() },
	skipPaths("/health", "/metrics"),
))
```

> 按 **IP 白名单**或 **HTTP 方法**跳过同理——把 `c.Request.URL.Path` 换成
> `c.ClientIP()` 或 `c.Request.Method` 即可。多条件组合就是几个 `skip` 函数 `||` 起来。

## 阻塞式整流(主动调用下游时)

如果不是服务端防刷,而是**主动调用下游**(API / DB / MQ)想把速率平滑摊开,用 `Wait`
而非 `Check`(详见 [`v2/README.md`](../v2/README.md)):

```go
if err := lim.Wait(ctx, "downstream-api"); err != nil {
	// ctx 取消/超时,或 golimit.ErrMaxKeys
	return err
}
callDownstream() // 被平滑到配置的 QPS.
```
