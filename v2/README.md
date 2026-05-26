# golimit v2 — 核心限流库

生产级 Go 限流库,基于 `golang.org/x/time/rate`(令牌桶算法).

**设计原则:零第三方框架依赖**。核心库只依赖 stdlib + `golang.org/x/time`,
Web 框架集成通过独立 sub-module 提供:

| Module | 用途 | 依赖 |
|---|---|---|
| `github.com/gtkit/golimit/v2` | **核心限流引擎**(本包) | 仅 `golang.org/x/time`,0 第三方 |
| `github.com/gtkit/golimit/v2/gin` | Gin 框架适配 | `golimit/v2` + `gin-gonic/gin` |

不需要 Gin 的用户(直接限流、其他框架、RPC、job)**只引入 v2 核心**,
不会被拖入 gin 的 27 个 indirect 依赖。

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

## Gin 适配

```go
import (
    "github.com/gtkit/golimit/v2"
    golimitgin "github.com/gtkit/golimit/v2/gin"
)

r := gin.New()
r.Use(golimitgin.IPLimit(100))
```

完整 Gin 用法见 [`v2/gin/README.md`](./gin/README.md)。

## 核心 API

| 函数/方法 | 说明 |
|---|---|
| `New(rps float64, opts ...Option) *Limiter` | 创建限流器,启动清理 goroutine |
| `(*Limiter).Allow(key string) bool` | 检查 1 个请求是否允许(零开销快路径) |
| `(*Limiter).AllowN(key string, n int) bool` | 检查 n 个请求(`n<=0` 恒 true) |
| `(*Limiter).Check(key string) Result` | **信息丰富**版本,返回 Result(供中间件写头) |
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

## 从 v2.0.x 迁移到 v2.1.0

```go
// 旧 v2.0.x:
import golimit "github.com/gtkit/golimit/v2"
r.Use(golimit.IPLimit(100))

// 新 v2.1.0+:
import (
    "github.com/gtkit/golimit/v2"
    golimitgin "github.com/gtkit/golimit/v2/gin"
)
r.Use(golimitgin.IPLimit(100))
```

迁移收益:不用 Gin 的代码路径完全摆脱 gin transitive 依赖。

## 运行测试

```bash
go test -race -v -count=1 ./...   # 功能 + 并发安全
go test -bench=. -benchmem ./...  # 性能基准
```

## License

MIT
