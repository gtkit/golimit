# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与
[语义化版本](https://semver.org/lang/zh-CN/)。

仓库内有**三个**独立 Go module:
- 根目录 `github.com/gtkit/golimit` — v1,全局单例 API,稳定维护
- `v2/` 目录 `github.com/gtkit/golimit/v2` — v2 实例化 API,零第三方依赖
- `v2/gin/` 目录 `github.com/gtkit/golimit/v2/gin` — v2 的 Gin 框架适配

---

## [Unreleased]

## v1.0.7 - 2026-05-26

> 🎯 **纯文档与内部清理**:行为完全等同 v1.0.6,无 API 变化,无性能变化.
> 应下游需求撤销 Deprecated 标记 — v1 仍是稳定维护路径,与 v2 是两套独立 API,各有适用场景.

### Changed

- 撤销 Package GoDoc 与 `NewLimiter` 的 `Deprecated:` 标记.staticcheck SA1019 不再对 v1 使用者报警.
- 内部字段重命名:`Limiters.limiters` → `Limiters.entries`(unexported,无 API 影响),更准确反映"注册表条目"语义.
- README / CHANGELOG / GoDoc 措辞软化:v1 不再描述为"维护模式 / 仅修关键 bug",改为"全局单例 API,稳定维护".v1 与 v2 的关系改述为"两套独立 API,各有适用场景".
- 清理源码注释中的历史版本号引用(v1.0.4 / v1.0.5 / v1.0.6 等)— 这些信息属于 git log + CHANGELOG,不应在注释中累积.

### Unchanged

- 公开 API、性能、行为 — 均与 v1.0.6 完全等价
- 测试 100% 覆盖、benchmark `BenchmarkGetLimiter_RPSUnchanged` 约 4.5 ns/op、race 稳定

## v1.0.6 - 2026-05-26

> 🚀 **纯性能优化版本**:行为完全不变,API 完全兼容,仅把 `getLimiter` 命中已有 key 时
> 的 mutex 比较替换为 atomic 比较,把"模式 B(每请求 NewLimiter)"用户的开销降到极致.
>
> 实测 benchmark 对比:
>
> | 路径 | v1.0.5(mutex 比较) | v1.0.6(atomic 缓存) | 改进 |
> |---|---|---|---|
> | RPS 不变(99% 场景) | ~101 ns/op | **3.9 ns/op** | **快 25 倍** |
> | RPS 变化(<1% 场景) | ~150 ns/op | ~362 ns/op* | 罕见路径,正确性优先 |
>
> *RPS 变化路径包含 atomic Load + SetLimit + SetBurst + atomic Store 四次开销,
> 跟 v1.0.5 同数量级,这是正确性必需的开销.
>
> 无 API 变化,无行为变化,**v1.0.5 用户可直接升级,零代码改动**.

### Changed

- `Limiter` 结构体新增 `rps atomic.Int64` 字段,缓存当前 rps,替代每次 `rate.Limiter.Limit()` 的 mutex 读.
- `getLimiter` 命中已有 key 时,比较改为 `atomic.Int64.Load`(~1 ns,免锁),仅在 rps 实际变化时才走 SetLimit + SetBurst.
- 同步更新 LoadOrStore 输家分支与首次创建分支的 atomic rps 初始化逻辑.
- 内存:每个 Limiter 多 8 字节(atomic.Int64);相对 v1.0.5 已删除 `Limiter.key` 字段省的 16 字节,**净省 8 字节/Limiter**.

### Tests

- 新增 benchmark `BenchmarkGetLimiter_RPSUnchanged`(模式 B hot path,验证 ~4 ns 量级开销).
- 新增 benchmark `BenchmarkGetLimiter_RPSChanged`(rps 实际变化路径,验证 SetLimit + SetBurst 正确触发).
- 移除旧的 `BenchmarkGetLimiter_ExistingKey`(由上述两个更具针对性的 benchmark 取代).

## v2/v2.1.0 - 2026-05-26

> ⚠ **重大重构(BREAKING)**:v2 核心库**移除所有 Gin 依赖**,迁移至独立的
> `github.com/gtkit/golimit/v2/gin` sub-module。这样 v2 核心保持**零第三方依赖**
> (只依赖 `golang.org/x/time`),不再拖累 gin 的 27 个 indirect 依赖 / 19 个
> transitive vulnerability 给所有用户。
>
> **迁移方法**:
>
> ```go
> // 旧(v2.0.x):
> import "github.com/gtkit/golimit/v2"
> r.Use(golimit.IPLimit(100))
>
> // 新(v2.1.0+):
> import (
>     "github.com/gtkit/golimit/v2"
>     golimitgin "github.com/gtkit/golimit/v2/gin"
> )
> r.Use(golimitgin.IPLimit(100))
> ```
>
> 不使用 Gin 的用户(直接调 `Allow` / `AllowN` / `Check`)**无需任何改动,反而
> 受益**:从 27 个 indirect 缩到 0,go.sum 从 41 行缩到 2 行。

### Removed (BREAKING)

- 从 v2 核心库移除全部 Gin 相关 API:
  - `KeyFunc` / `ErrorHandler` / `SkipFunc` / `MiddlewareConfig`
  - `Middleware` / `GinMiddleware`
  - `IPLimit` / `PathIPLimit` / `UserLimit` + `*WithLimiter` 变体
  - `SkipPaths` / `SkipIPs` / `SkipMethods` / `CombineSkips`
- 这些全部迁移到 `github.com/gtkit/golimit/v2/gin`,签名保持兼容(仅包名变化)。
- 直接依赖 `github.com/gin-gonic/gin` 移除。

### Added

- **`Result` 类型**:框架无关的限流结果 struct(`Allowed` / `Limit` / `Remaining` / `ResetAt` / `RetryAfter` / `Reason`),供任意 Web/RPC/调度层适配使用。
- **`(*Limiter).Check(key) Result`**:`Allow` 的"信息丰富"版本,一次调用拿到完整元信息,免去上层多次读 `Tokens` / 重算 `RetryAfter`。
- **`RetryAfterSeconds(rate float64) int`** 导出的辅助函数,框架适配层公用。
- v2 sub-module `v2/gin/` 拆出,内部用 `Check` + `Result` 实现 middleware,ErrorHandler 签名升级为 `func(c *gin.Context, result golimit.Result)` 让用户拿到元信息。

### Changed

- `defaultErrorHandler` 现在通过 `Result` 拿到精准的 `RetryAfter`,不再硬编码或硬算。
- `setRateLimitHeaders` 现在在允许 / 拒绝两条路径都写 X-RateLimit-* 头(此前仅允许路径写),让客户端在 429 时也能从头部知道限流上限。

### Tests

- v2 核心新增 4 个 Check 接口测试:`TestCheck_RetryAfterFractional` / `TestCheck_Allowed` / `TestCheck_MaxKeysRejected` / `TestRetryAfterSeconds_Helper`。
- 原 15 个 Gin 中间件测试迁移到 `v2/gin/middleware_test.go`,并补一个 `TestIPLimitWithLimiter_CloseStopsCleanup`、一个 `TestMiddleware_SkipMethods`。

---

## v2/gin/v2.0.0 - 2026-05-26 (首发)

新 sub-module。从 v2 核心库拆分,集中所有 Gin 框架适配代码。

### Added

- 完整迁移自原 v2 `middleware.go`,API 兼容:
  - `Middleware` / `GinMiddleware`
  - `IPLimit` / `PathIPLimit` / `UserLimit` + `*WithLimiter` 变体
  - `SkipPaths` / `SkipIPs` / `SkipMethods` / `CombineSkips`
- 内部使用 v2 核心的 `Check` + `Result` 实现,响应头精度提升。
- `ErrorHandler` 签名升级为 `func(c *gin.Context, result golimit.Result)`,接收 Result 让自定义错误处理拿到元信息。
- 429 响应也会写 X-RateLimit-Limit / X-RateLimit-Remaining / X-RateLimit-Reset。

### Documentation

- 独立的 `v2/gin/README.md` 讲 Gin 适配用法、ClientIP 信任、Skip 组合等。
- 全文 GoDoc 与内联注释中文化。

---

## v2/v2.0.2 - 2026-05-26

### Fixed

> 本版修复 3 个深度审计发现的隐患,**强烈建议升级**(尤其是隐患 1 — 在高并发 + 短 cleanup 周期组合下可能绕过限流).

- **🔴 关键 bug:新建 visitor 的 `lastSeen` 默认零值,可能被 cleanup goroutine 立即误删**。
  - 场景:`getOrCreate` 创建 visitor 时未 Store `lastSeen`,默认为 0。若 cleanup goroutine 在首次 `Allow` 调用 `v.lastSeen.Store(time.Now())` 之前抢先 Range 到这个 visitor,`0 < threshold` 永远成立,visitor 被即时删除。后续 Allow 同 key 会**新建满 burst 的 visitor**,**绕过限流**。
  - 修复:`getOrCreate` 中在 LoadOrStore 之前立即 `v.lastSeen.Store(time.Now().UnixNano())`。
  - 与 v1 设计一致(v1 在 `getLimiter` 中本就这样做了,v2 重构时漏带过来)。
- **🟠 `WithCleanupInterval(0)` 触发 `time.NewTicker(0)` panic**。
  - stdlib 强制要求 `d > 0`,否则 panic("non-positive interval")。用户配 0 会让 New() 进入 goroutine 后立即崩。
  - 修复:在 `New` 中校验 `cleanupInterval <= 0` 则回退到默认 1 分钟。同时为 `maxIdleTime <= 0` 加同等兜底。
- **🟡 `WithMaxKeys(-n)` 静默等价无上限**。
  - 负数让 `if l.maxKeys > 0 && ...` 永远走不进保护分支,DoS 防护静默失效。用户配错不会察觉。
  - 修复:在 `New` 中校验 `maxKeys < 0` 则规范化为 0(显式无上限)。

### Documentation

- `New` GoDoc 新增"所有 Option 非法值都会被规范化"的明确说明,列举每项默认值。

### Tests

- 新增 6 个回归测试:
  - `TestNewVisitorNotImmediatelyCleaned` + `_Concurrent` — 锁定隐患 1 修复(激进 cleanup 周期下新建 visitor 不被误删).
  - `TestWithCleanupInterval_ZeroSafe` + `_NegativeSafe` — 锁定隐患 2 修复.
  - `TestWithMaxIdleTime_ZeroSafe` — 同类问题(maxIdleTime 也加了兜底).
  - `TestWithMaxKeys_NegativeSafe` — 锁定隐患 3 修复.

## v1.0.5 - 2026-05-26

> ⚠ **重要行为变更(BEHAVIOR CHANGE)**:同一 key 多次调用 `NewLimiter(key, rps)`
> 时,新传入的 `rps` 现在会**热更新**到现有 Limiter(底层调用 stdlib
> `rate.Limiter.SetLimit/SetBurst`,并发安全)。此前版本(<= v1.0.4)会**静默
> 忽略**新 rps,只返回首次创建的实例 —— 这是隐性 bug,现已修复。
>
> **下游若依赖旧行为**(例如先调小、后调大,期待保留小值),请评估升级影响。
> 99% 的使用场景下新行为更符合直觉(改了配置就生效),但变更属于 silent
> behavior change,故在此显著标注。
>
> stdlib 的"放气"语义注意:rps 从高调到低(如 100 → 5)的瞬间,桶内已有的
> token 不会立即被砍到新 burst,要到下次 `Allow` 时才会 cap。切换过渡期可能
> 放过最多 (旧 burst) 个请求,之后才严格按新 rps 限流。

### Fixed

- 同一 key 多次 `NewLimiter` 时新 rps 现已热更新生效(此前静默忽略,详见上方变更说明)。
- `getLimiter` 的 `LoadOrStore` 失败分支(并发首次创建的"输家")同步执行 rps 更新,确保"最后写入者胜出"语义在所有路径上一致。
- 负数 rps(`rps < 0`)规范化为 0,避免 stdlib `SetBurst` 内部 `panic("burst < 0")`。

### Changed

- 删除 `Limiter.key` 死字段(全文件无读取,sync.Map 已用 key 索引,内部无需重复存储)。节省每个 Limiter ~16 字节。
- `version.go` 的 `Version` 常量从 `v1.0.3` 同步至当前发版号 `v1.0.5`(此前因 Makefile tag 流程遗留与 git tag 不同步)。

### Documentation

- `NewLimiter` GoDoc 新增 rps 热更新行为说明 + stdlib 放气语义说明 + 负数处理说明。
- 全文件源码注释中文化(`Deprecated:` 关键字保留英文以兼容 staticcheck SA1019 识别)。

### Tests

- 新增 `TestRPSHotUpdate` 套件覆盖 7 个场景:基础热更新、幂等、实际生效、负数防御、首次负数防御、并发安全、LoadOrStore 输家分支、端到端全局注册表。

## v2/v2.0.1 - 2026-05-26

### Fixed

- 修复 `TestCleanup_ActiveKeysSurvive` 测试参数错误:原 `rate=100/s + burst=100` 会在 sleep 期间令牌补满至 burst,断言永远失败。改为 `rate=2/s + burst=100`。
- 修复 `TestMiddleware_Concurrency` 测试参数错误:原 `rate=10000/s + burst=50` 时令牌补充远超 burst 上限,burst 边界无法被测出。改为 `rate=50/s + burst=50`。
- 修复 `New(rps)` 当 `rps < 1` 时 `burst = int(rps) = 0` 导致**所有请求被拒绝**的边界 bug。改为 `max(1, ceil(rps))`。
- 修复 `defaultErrorHandler` 的 `Retry-After` 头硬编码为 `"1"`,当 `rate < 1` 时返回错误的重试时间。改为按 `1/rate` 向上取整。
- 修复 `v2/version.go` 注释残留 "json package" 字样(应为 "golimit package")。
- 修复 `TestConcurrency_MultiKey` 的 `copylocks` lint 告警(range 拷贝 `atomic.Int64`)。

### Added

- `WithOnReject(fn)`:限流拒绝时触发回调,reason 区分 `RejectRate` / `RejectMaxKeys`,用于接入 metrics / log。
- `WithMaxKeys(n)`:限制 Limiter 内缓存的 key 上限,超限拒绝新建,防止键基数 DoS 攻击。
- `Limiter.Size()`:返回当前缓存 key 数,供运维监控采样。
- `IPLimitWithLimiter` / `PathIPLimitWithLimiter` / `UserLimitWithLimiter` 三个便捷构造器变体,返回 `(gin.HandlerFunc, *Limiter)`,支持 graceful shutdown。
- `AllowN(key, n)` 当 `n <= 0` 时直接返回 `true`(零请求恒允许)。
- Middleware 对空 KeyFunc 输出做兜底:回退到 `"_empty:" + ClientIP()`,避免所有空 key 共享同一 bucket。

### Documentation

- README 新增"生产部署须知":`SetTrustedProxies` 信任配置、`WithMaxKeys` 防 DoS、可观测性接入、便捷构造器的 goroutine 生命周期、空 KeyFunc 行为。
- 各便捷构造器函数 GoDoc 加入"内部 Limiter 不可关闭"警告。

## v2/v2.0.0

### Added

- 全新 v2 API:从全局单例改为 `*Limiter` 实例,支持多个独立限流器。
- `Close()` 优雅关闭 cleanup goroutine,不再泄漏。
- Gin 中间件层:`Middleware` / `GinMiddleware` / `IPLimit` / `PathIPLimit` / `UserLimit`。
- Skip helpers:`SkipPaths` / `SkipIPs` / `SkipMethods` / `CombineSkips`。
- Options:`WithBurst` / `WithCleanupInterval` / `WithMaxIdleTime`。
- 标准限流响应头:`X-RateLimit-Limit/Remaining/Reset` + `Retry-After`。

### Fixed

- 修复 v1 `rate.Every(1*time.Second)` 导致 rps 参数被无视的速率语义错误。
- 修复 v1 `lastGet time.Time` 在并发下的 data race(改为 `atomic.Int64`)。
- 修复 v1 `Load + Store` 创建竞态(改为 `LoadOrStore`)。

---

## v1.0.4

v1 进入维护模式 — 仅修关键 bug,新功能请使用 v2。

详见 git commit history:`git log v1.0.0..v1.0.4`。

## v1.0.0 ~ v1.0.3

参见 git tag history。
