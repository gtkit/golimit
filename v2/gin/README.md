# golimit/v2/gin — Gin 框架适配

[golimit/v2](../) 的 Gin 框架适配层。

**为什么独立 sub-module**:核心限流库 `golimit/v2` 坚持**零第三方依赖**(只引
`golang.org/x/time`)。Gin 会拖入 27 个 indirect 依赖与 19 个 transitive
vulnerability,这些只影响显式 import `v2/gin` 的用户,不污染所有人。

## 安装

```bash
go get github.com/gtkit/golimit/v2/gin@latest
```

## 30 秒上手

```go
import (
    "github.com/gin-gonic/gin"
    "github.com/gtkit/golimit/v2"
    golimitgin "github.com/gtkit/golimit/v2/gin"
)

r := gin.New()
r.Use(golimitgin.IPLimit(100))  // 每 IP 每秒 100 个请求
```

## 用法

### 1. 便捷构造器(覆盖 80% 场景)

```go
r.Use(golimitgin.IPLimit(100))                // 按 IP 限流
r.Use(golimitgin.PathIPLimit(10))             // 按 IP + 路径限流
r.Use(golimitgin.UserLimit(60, func(c *gin.Context) string {
    return c.GetString("user_id")            // 按用户 ID 限流(匿名回退 IP)
}))
r.Use(golimitgin.IPLimit(100, golimit.WithBurst(200)))  // 自定义突发容量
```

### 2. 完全配置模式

```go
lim := golimit.New(100, golimit.WithBurst(200))
defer lim.Close()

r.Use(golimitgin.Middleware(golimitgin.MiddlewareConfig{
    Limiter: lim,
    KeyFunc: func(c *gin.Context) string {
        return c.GetHeader("X-API-Key")
    },
    ErrorHandler: func(c *gin.Context, result golimit.Result) {
        // result 包含完整元信息:RetryAfter / Limit / Remaining / Reason
        c.AbortWithStatusJSON(429, gin.H{
            "code":        429,
            "message":     "请求过于频繁,请稍后再试",
            "retry_after": int(result.RetryAfter.Seconds()),
        })
    },
    Skip: golimitgin.CombineSkips(
        golimitgin.SkipPaths("/health", "/metrics"),
        golimitgin.SkipIPs("127.0.0.1", "10.0.0.1"),
        golimitgin.SkipMethods(http.MethodOptions),
    ),
}))
```

### 3. 优雅关闭(*WithLimiter 变体)

```go
mw, lim := golimitgin.IPLimitWithLimiter(100)
defer lim.Close()  // graceful shutdown 时释放 cleanup goroutine
r.Use(mw)
```

`IPLimit` / `PathIPLimit` / `UserLimit` 的便捷版本**内部 Limiter 不可关闭**,
适合进程级常驻;需要管理生命周期请用 `*WithLimiter` 变体。

### 4. 多 Limiter 实例(不同路由组不同速率)

```go
apiLimiter  := golimit.New(100)
authLimiter := golimit.New(5, golimit.WithBurst(5))
defer apiLimiter.Close()
defer authLimiter.Close()

api := r.Group("/api")
api.Use(golimitgin.GinMiddleware(apiLimiter))

auth := r.Group("/auth")
auth.Use(golimitgin.GinMiddleware(authLimiter))
```

## API

| 函数 | 说明 |
|---|---|
| `Middleware(cfg MiddlewareConfig)` | 完全配置的中间件 |
| `GinMiddleware(l *golimit.Limiter)` | 默认配置中间件 |
| `IPLimit(rps, ...Option)` | 按 IP 限流 |
| `PathIPLimit(rps, ...Option)` | 按 IP+Path 限流 |
| `UserLimit(rps, getUserID, ...Option)` | 按用户 ID 限流 |
| `IPLimitWithLimiter` / `PathIPLimitWithLimiter` / `UserLimitWithLimiter` | 同上,额外返回 `*Limiter` |

### Skip Helpers

| 函数 | 说明 |
|---|---|
| `SkipPaths(paths...)` | 跳过指定路径 |
| `SkipIPs(ips...)` | 跳过指定 IP(白名单) |
| `SkipMethods(methods...)` | 跳过指定 HTTP 方法 |
| `CombineSkips(fns...)` | 组合多个 Skip(任一为 true 即跳过) |

### 类型

| 类型 | 签名 |
|---|---|
| `KeyFunc` | `func(c *gin.Context) string` |
| `ErrorHandler` | `func(c *gin.Context, result golimit.Result)` |
| `SkipFunc` | `func(c *gin.Context) bool` |

## 响应头

成功请求自动携带:
```
X-RateLimit-Limit:     100         # 突发容量
X-RateLimit-Remaining: 87          # 剩余令牌(消耗 1 之后)
X-RateLimit-Reset:     1711234567  # Unix 时间戳
```

被拒绝的请求额外携带:
```
Retry-After: 1   # 按 1/rate 向上取整;rate>=1 时为 1
```

## 生产部署须知

### `c.ClientIP()` 信任配置(重要)

Gin 默认信任所有反代头(`X-Forwarded-For` / `X-Real-IP`)。**公网部署务必限制信任范围**,否则攻击者伪造头即可绕过 per-IP 限流:

```go
r := gin.New()
_ = r.SetTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "100.64.0.0/10"})
r.Use(golimitgin.IPLimit(100))
```

### 空 KeyFunc 行为

`KeyFunc` 返回 `""` 时,Middleware 自动回退到 `"_empty:" + ClientIP()`,
避免所有空 key 共享同一桶(等同全局限流)。

### KeyFunc 设计原则

不要把请求体 / 大字符串 / 高熵参数放进 key — 这会让 `WithMaxKeys` 上限失效,
增加 sync.Map 内存。推荐用低基数维度:IP、user_id、route 等。

## 从 v2 内置 middleware 迁移

```go
// 旧(v2.0.x v2 直接 import):
import golimit "github.com/gtkit/golimit/v2"
r.Use(golimit.IPLimit(100))

// 新(v2.1.0+):
import (
    "github.com/gtkit/golimit/v2"
    golimitgin "github.com/gtkit/golimit/v2/gin"
)
r.Use(golimitgin.IPLimit(100))
```

签名完全兼容,只需替换 import + 包名前缀。**例外**:`ErrorHandler` 签名从
`func(c *gin.Context)` 改为 `func(c *gin.Context, result golimit.Result)`,
新增 `result` 参数让自定义处理拿到完整元信息。

## License

MIT
