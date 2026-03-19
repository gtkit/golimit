# golimit v2

生产级 Go 限流库，基于 `golang.org/x/time/rate`（令牌桶算法）。

v2 是 golimit v1 和 rate.go 中间件的合并重构版本：保留 v1 的简洁内核，加入 Gin 中间件层的配置能力。

## 安装

```bash
go get github.com/gtkit/golimit/v2@latest
```

## 30 秒快速开始

```go
// 最简单 — 一行搞定.
r.Use(golimit.IPLimit(100)) // 每个 IP 每秒 100 个请求.
```

## 架构

```
┌──────────────────────────────────────────────────┐
│              Gin Middleware Layer                 │
│  IPLimit / PathIPLimit / UserLimit / Middleware   │
│  KeyFunc · ErrorHandler · Skip · Headers         │
├──────────────────────────────────────────────────┤
│              Core Limiter Engine                 │
│  golimit.New(rps, opts...)                       │
│  sync.Map (per-key isolation, lock-free read)    │
│  atomic.Int64 (lastSeen, no data race)           │
│  LoadOrStore (race-free creation)                │
│  Background cleanup (configurable interval)      │
├──────────────────────────────────────────────────┤
│              golang.org/x/time/rate              │
│  Token bucket · Go team maintained               │
└──────────────────────────────────────────────────┘
```

**两层设计：**

- **Core**（`limiter.go`）— 纯 Go 限流引擎，不依赖 Gin，可以在任何 Go 程序中使用。
- **Middleware**（`middleware.go`）— Gin 集成层，提供 KeyFunc / ErrorHandler / Skip / Headers。

## 用法

### 1. 便捷构造器（覆盖 80% 场景）

```go
// 按 IP 限流.
r.Use(golimit.IPLimit(100))

// 按 IP + 路径 限流.
r.Use(golimit.PathIPLimit(10))

// 按用户 ID 限流（匿名用户回退到 IP）.
r.Use(golimit.UserLimit(60, func(c *gin.Context) string {
    return c.GetString("user_id")
}))

// 自定义突发容量.
r.Use(golimit.IPLimit(100, golimit.WithBurst(200)))
```

### 2. 完全配置模式

```go
lim := golimit.New(100, golimit.WithBurst(200))
defer lim.Close()

r.Use(golimit.Middleware(golimit.MiddlewareConfig{
    Limiter: lim,
    KeyFunc: func(c *gin.Context) string {
        return c.GetHeader("X-API-Key")
    },
    ErrorHandler: func(c *gin.Context) {
        c.AbortWithStatusJSON(429, gin.H{
            "code":    429,
            "message": "请求过于频繁，请稍后再试",
        })
    },
    Skip: golimit.CombineSkips(
        golimit.SkipPaths("/health", "/metrics"),
        golimit.SkipIPs("127.0.0.1", "10.0.0.1"),
        golimit.SkipMethods("OPTIONS"),
    ),
}))
```

### 3. 单独使用 Core（不需要 Gin）

```go
lim := golimit.New(100)
defer lim.Close()

if !lim.Allow("user:123") {
    // 被限流了.
}

// 批量检查.
if !lim.AllowN("user:123", 5) {
    // 一次请求 5 个配额.
}

// 查询剩余令牌.
tokens := lim.Tokens("user:123")
```

### 4. 多个限流器实例（不同路由组不同速率）

```go
apiLimiter := golimit.New(100)
authLimiter := golimit.New(5, golimit.WithBurst(5)) // 登录接口更严格.
defer apiLimiter.Close()
defer authLimiter.Close()

api := r.Group("/api")
api.Use(golimit.GinMiddleware(apiLimiter))

auth := r.Group("/auth")
auth.Use(golimit.GinMiddleware(authLimiter))
```

## API 参考

### Core

| 函数/方法 | 说明 |
|-----------|------|
| `New(rps float64, opts ...Option) *Limiter` | 创建限流器，启动清理 goroutine. |
| `(*Limiter).Allow(key string) bool` | 检查 1 个请求是否允许. |
| `(*Limiter).AllowN(key string, n int) bool` | 检查 n 个请求是否允许. |
| `(*Limiter).Tokens(key string) float64` | 查询当前可用令牌数. |
| `(*Limiter).Reset(key string)` | 重置指定 key 的限流状态. |
| `(*Limiter).Close()` | 停止清理 goroutine，释放资源. |

### Options

| Option | 默认值 | 说明 |
|--------|-------|------|
| `WithBurst(n int)` | 等于 rps | 最大突发容量. |
| `WithCleanupInterval(d)` | 1 分钟 | 清理扫描间隔. |
| `WithMaxIdleTime(d)` | 5 分钟 | key 空闲多久后被清理. |

### Gin Middleware

| 函数 | 说明 |
|------|------|
| `Middleware(cfg MiddlewareConfig)` | 完全配置的中间件. |
| `GinMiddleware(l *Limiter)` | 最简中间件（默认配置）. |
| `IPLimit(rps, ...Option)` | 按 IP 限流. |
| `PathIPLimit(rps, ...Option)` | 按 IP+Path 限流. |
| `UserLimit(rps, getUserID, ...Option)` | 按用户 ID 限流. |

### Skip Helpers

| 函数 | 说明 |
|------|------|
| `SkipPaths(paths...)` | 跳过指定路径. |
| `SkipIPs(ips...)` | 跳过指定 IP（白名单）. |
| `SkipMethods(methods...)` | 跳过指定 HTTP 方法. |
| `CombineSkips(fns...)` | 组合多个 Skip（任一为 true 即跳过）. |

## 响应头

成功的请求自动携带标准限流头：

```
X-RateLimit-Limit: 100        # 突发容量.
X-RateLimit-Remaining: 87     # 剩余令牌数.
X-RateLimit-Reset: 1711234567 # 下一秒的 Unix 时间戳.
```

被拒绝的请求额外携带：

```
Retry-After: 1                # 建议重试等待秒数.
```

## 从 v1 迁移

```go
// v1:
lim := golimit.NewLimiter("key", 100)
lim.Allow()

// v2 — 直接使用 core:
lim := golimit.New(100)
defer lim.Close()
lim.Allow("key")

// v2 — 在 Gin 中间件中使用:
r.Use(golimit.IPLimit(100))
```

**v1 → v2 主要变化：**

- `NewLimiter(key, rps)` → `New(rps)` + `Allow(key)`：速率和 key 分离，一个 Limiter 实例管理所有 key.
- 全局单例 → 实例化：支持多个独立的限流器（不同路由组不同速率）.
- 新增 `Close()` 方法：优雅关闭清理 goroutine，不再泄漏.
- 新增 Gin 中间件层：`IPLimit` / `PathIPLimit` / `UserLimit` / `Middleware`.
- 修复了 v1 的 `rate.Every(1s)` 速率语义错误、`lastGet` data race、`Load+Store` 竞态.

## 运行测试

```bash
go test -race -v -count=1 ./...   # 功能测试 + 并发安全.
go test -bench=. -benchmem ./...  # 性能基准测试.
```

## 小提示
在"路由组挂不同中间件"这个用法上，v1 和 v2 的效果完全一样。真正的区别在底层：

**v1 — 全局单例，靠 key 不同来隔离**

```go
api.Use(LimitIp(100))   // 内部调 golimit.NewLimiter(key, 100)
auth.Use(LimitIp(5))    // 内部调 golimit.NewLimiter(key, 5)
//                         ↑ 都往同一个 GlobalLimiters.sync.Map 里存
```

能工作，但有一个隐患——如果两个路由组碰巧生成了相同的 key（比如你改了 key 生成逻辑不带 path），先创建的那个 limiter 的速率会"赢"，后面的 `rps` 参数被静默忽略。不会报错，只是限流速率不对，排查起来很痛苦。

**v2 — 独立实例，物理隔离**

```go
apiLimiter  := golimit.New(100)
authLimiter := golimit.New(5)

api.Use(golimit.GinMiddleware(apiLimiter))
auth.Use(golimit.GinMiddleware(authLimiter))
//       ↑ 两个完全独立的 sync.Map，不可能互相干扰
```

即使 key 完全相同（比如同一个 IP），两个 limiter 里各自有自己的 `rate.Limiter` 实例，速率各算各的。

所以区别不在用法上，而在**安全边际**上——v1 能工作靠的是"key 恰好不重复"这个前提，v2 靠的是结构上不可能冲突。

## License

MIT
