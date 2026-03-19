# golimit

轻量级、并发安全的 Go 限流库，基于 `golang.org/x/time/rate`（令牌桶算法），支持 per-key 隔离和自动清理。

## 安装

```bash
go get github.com/gtkit/golimit@latest
```

## 快速开始

```go
lim := golimit.NewLimiter("user:123:/api/v1", 100)
if !lim.Allow() {
    // 被限流了
}
```

- `"user:123:/api/v1"` — 限流 key，不同 key 之间完全隔离。
- `100` — 每秒允许 100 个请求（稳态速率），同时初始突发容量也是 100。

## 在 Gin 中使用

```go
func LimitIp(rps int) gin.HandlerFunc {
    return func(c *gin.Context) {
        key := c.ClientIP() + ":" + c.Request.URL.Path
        if !golimit.NewLimiter(key, rps).Allow() {
            c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
                "code":    429,
                "message": "请求过于频繁，请稍后再试",
            })
            return
        }
        c.Next()
    }
}

// 注册中间件.
r := gin.New()
r.Use(LimitIp(100)) // 每个 IP+Path 每秒 100 个请求.
```

## 工作原理

```
            NewLimiter("ip:1.2.3.4:/api", 100)
                        │
                        ▼
              ┌─────────────────────┐
              │   GlobalLimiters    │   ← sync.Map (并发安全)
              │                     │
              │  key → *Limiter     │
              │  key → *Limiter     │
              │  key → *Limiter     │
              └─────────┬───────────┘
                        │ Load / LoadOrStore
                        ▼
              ┌─────────────────────┐
              │     *Limiter        │
              │                     │
              │  rate.Limiter       │   ← golang.org/x/time/rate (令牌桶)
              │  lastGet (atomic)   │   ← 最后访问时间，原子操作
              └─────────────────────┘

         后台清理 goroutine (每分钟):
           遍历 sync.Map，删除 lastGet > 5min 的 entry
```

**令牌桶算法简述：**  桶里最多放 `rps` 个令牌（burst），每秒匀速补充 `rps` 个。每个请求消耗 1 个令牌，桶空则拒绝。这允许短时间突发流量，同时保证长期平均不超过 `rps`。

## API

### `NewLimiter(key string, rps int) *Limiter`

获取或创建指定 key 的限流器。

- **key** — 限流维度的唯一标识（如 `"ip:1.2.3.4:/api/v1"`）。
- **rps** — 每秒允许的请求数（同时也是初始突发容量）。
- 相同 key 多次调用返回同一个 `*Limiter` 实例（不会重复创建）。
- 首次调用会自动启动后台清理 goroutine（`sync.Once` 保证只启动一次）。

### `(*Limiter).Allow() bool`

检查是否允许当前请求。并发安全，可以从任意数量的 goroutine 同时调用。

## 运行测试

```bash
# 功能测试 + race 检测.
go test -race -v ./...

# 基准测试.
go test -bench=. -benchmem ./...
```

## License

MIT