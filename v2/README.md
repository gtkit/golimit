# golimit v2 — 核心限流库

生产级 Go 限流库,基于 `golang.org/x/time/rate`(令牌桶算法).

**设计原则:零第三方框架依赖**。本包只依赖 stdlib + `golang.org/x/time`,**不绑定任何
Web 框架**。框架集成(Gin / echo / chi 等)以**文档示例**形式提供——基于框架无关的
`Check` + `Result` 契约写十几行中间件即可,复制即用,不给你的 `go.sum` 引入任何额外依赖。

Gin 的完整可复制示例(中间件 / per-IP·user key / Skip 白名单 / 429 + 限流头)见
**[`docs/gin.md`](../docs/gin.md)**。

## 安装

```bash
go get github.com/gtkit/golimit/v2@latest
```

## 30 秒上手

```go
import golimit "github.com/gtkit/golimit/v2"

lim := golimit.New(100)               // 100 req/s,burst=100
defer lim.Close()

if !lim.Allow("user:123") {
    // 被限流了
}
```

## 框架适配通过 Check + Result

中间件 / 拦截器层用 `Check` 拿到完整元信息(写响应头、决策、降级):

```go
r := lim.Check("user:123")

w.Header().Set("X-RateLimit-Limit",     strconv.Itoa(r.Limit))
w.Header().Set("X-RateLimit-Remaining", strconv.FormatFloat(r.Remaining, 'f', 0, 64))

if !r.Allowed {
    w.Header().Set("Retry-After", strconv.Itoa(int(r.RetryAfter.Seconds())))
    http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
    return
}
// 继续处理...
```

`Result` 是**框架无关**的接口契约,Gin / echo / chi / 自家 RPC / job 调度等都能直接用。

## 阻塞式整流:Wait

`Allow` / `Check` 是"超额即拒"(服务端防刷);`Wait` 是"超额排队等待"(平滑出口流量),
适合主动调用下游 API / DB / MQ 时把请求速率匀速摊开:

```go
lim := golimit.New(100) // 每秒最多 100 次
defer lim.Close()

for _, job := range jobs {
    if err := lim.Wait(ctx, "downstream-api"); err != nil {
        break // ctx 取消/超时,或 ErrMaxKeys
    }
    callDownstream(job) // 被平滑到 ~100 QPS
}
```

注意:`Wait` 的取消遵循底层 `rate` 包 —— ctx deadline 早于令牌补充时间时会**提前**
返回错误(不傻等),等待中被 cancel 返回 `context.Canceled`。

## 接入 Gin / 其他 Web 框架

本包**不绑定框架**,接入只是基于 `Check` 写十几行中间件胶水。最小骨架:

```go
lim := golimit.New(100)
defer lim.Close()

r := gin.New()
r.Use(func(c *gin.Context) {
    res := lim.Check("ip:" + c.ClientIP())
    if !res.Allowed {
        c.AbortWithStatusJSON(429, gin.H{"message": "rate limit exceeded"})
        return
    }
    c.Next()
})
```

完整可复制示例(per-IP·user key、Skip 白名单、429 + `X-RateLimit-*` 头)见
**[`docs/gin.md`](../docs/gin.md)**。

## 核心 API

| 函数/方法 | 说明 |
|---|---|
| `New(rps float64, opts ...Option) *Limiter` | 创建限流器,启动清理 goroutine |
| `(*Limiter).Allow(key string) bool` | 检查 1 个请求是否允许(零开销快路径) |
| `(*Limiter).AllowN(key string, n int) bool` | 检查 n 个请求(`n<=0` 恒 true) |
| `(*Limiter).Check(key string) Result` | **信息丰富**版本,返回 Result(供中间件写头) |
| `(*Limiter).Wait(ctx, key string) error` | **阻塞式整流**:超额排队等待而非拒绝,支持 ctx 取消 |
| `(*Limiter).WaitN(ctx, key string, n int) error` | `Wait` 的批量版(`n<=0` 恒过) |
| `(*Limiter).Tokens(key string) float64` | 当前可用令牌数 |
| `(*Limiter).Reset(key string)` | 重置指定 key 的限流状态 |
| `(*Limiter).Size() int64` | 当前缓存的 key 数(运维监控) |
| `(*Limiter).Close()` | 停止清理 goroutine,释放资源 |
| `(*Limiter).Rate() float64` | 配置的 rps |
| `(*Limiter).Burst() int` | 配置的 burst |
| `RetryAfterSeconds(rate float64) int` | helper:根据 rate 算 Retry-After 秒数 |

### Result 字段

```go
type Result struct {
    Allowed    bool          // 是否允许
    Limit      int           // 配置的 burst(供 X-RateLimit-Limit)
    Remaining  float64       // 当前剩余令牌
    ResetAt    time.Time     // 下次桶满时间(仅允许路径)
    RetryAfter time.Duration // 建议重试等待(仅拒绝路径)
    Reason     RejectReason  // 拒绝原因:RejectRate / RejectMaxKeys
}
```

### Options

| Option | 默认值 | 说明 |
|---|---|---|
| `WithBurst(n int)` | `max(1, ceil(rps))` | 最大突发容量 |
| `WithCleanupInterval(d)` | 1 分钟 | 清理扫描间隔 |
| `WithMaxIdleTime(d)` | 5 分钟 | key 空闲多久后被清理 |
| `WithMaxKeys(n int)` | 0(无上限) | 限制最多缓存的 key 数,防键基数 DoS |
| `WithOnReject(fn)` | nil | 拒绝回调,reason 区分 `RejectRate` / `RejectMaxKeys` |

所有 Option 的非法值(0 / 负数)都会被规范化为默认值,**`New` 永不 panic**。

## 生产部署须知

### 键基数控制(防 DoS)

如果 key 包含可变维度(路径 / UA / 伪造 IP),攻击者可灌入大量伪造 key 耗光内存:

```go
lim := golimit.New(100,
    golimit.WithMaxKeys(1_000_000),        // 1M key 上限
    golimit.WithMaxIdleTime(time.Minute),  // 缩短 idle 提升清理频率
)
defer lim.Close()
```

超过上限的新 key 会被直接拒绝,通过 `WithOnReject` 回调上报 `RejectMaxKeys`。

### 可观测性

```go
lim := golimit.New(100, golimit.WithOnReject(func(key string, reason golimit.RejectReason) {
    rejectedTotal.WithLabelValues(string(reason)).Inc()  // prometheus
}))
```

`lim.Size()` 返回当前缓存 key 数,适合定期采样到 metrics。

## 旧 v2/gin 用户

早期提供过 `github.com/gtkit/golimit/v2/gin` 适配包,现已**移除**——框架适配改为文档
示例(见 [`docs/gin.md`](../docs/gin.md)),让核心库回归纯粹的通用限流引擎。已发布的
`v2/gin@v2.0.0` 在 module proxy 中仍可拉取,但不再维护;建议照 `docs/gin.md` 自行接入
(十几行,基于 `Check`),顺带摆脱 gin 那串间接依赖。

## 运行测试

```bash
go test -race -v -count=1 ./...   # 功能 + 并发安全
go test -bench=. -benchmem ./...  # 性能基准
```

## License

MIT
