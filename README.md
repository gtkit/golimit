# golimit

轻量级、并发安全的 Go 限流库,基于 `golang.org/x/time/rate`(令牌桶算法).

仓库内有 **2 个独立 Go module**,按需引入:

| Module | API 风格 | 直接依赖 | 适用场景 |
|---|---|---|---|
| `github.com/gtkit/golimit` | v1:全局单例 `NewLimiter(key, rps)` | `golang.org/x/time` | 一个进程一个全局注册表的简单场景 |
| [`github.com/gtkit/golimit/v2`](./v2/README.md) | v2:实例化 `New(rps)` + `Allow(key)` / `Check(key)` / `Wait(ctx, key)` | `golang.org/x/time` | 需要多实例 / 生命周期管理 / DoS 防护 |

**v1 与 v2 是两套独立 API,按场景选择**。两者都**零第三方框架依赖**,go.sum 仅 2 行。

> 👉 **Gin 用户**:本库不绑定框架,基于 `Check` / `Result` 写十几行中间件即可。完整可复制示例见 **[`docs/gin.md`](./docs/gin.md)**。

详细文档:
- [`v2/README.md`](./v2/README.md) — 核心 API、Check/Result/Wait 用法、生产部署
- [`docs/gin.md`](./docs/gin.md) — 用 golimit 给 Gin 限流(复制即用,含 Skip)
- [`CHANGELOG.md`](./CHANGELOG.md) — 版本变更历史

---

## 选型:golimit 还是 uber-go/ratelimit?

[uber-go/ratelimit](https://github.com/uber-go/ratelimit) 是另一个常见的 Go 限流库.两者都叫"限流",但解决的是**相反方向**的问题,不是竞品 —— 选错方向会用得很别扭:

- **golimit** = 令牌桶 + **拒绝式**.保护**你自己这个服务端**不被打爆:超额**立即拒绝**(返回 429),不阻塞调用方.
- **uber-go/ratelimit** = 漏桶 + **阻塞式**.保护**你要调用的下游**:超额**阻塞等待**,把请求匀速摊开发出去.

一句话:**入口防刷用 golimit,出口整流用 uber-go/ratelimit.**

### 按场景选

| 你的需求 | 选谁 |
|---|---|
| HTTP / RPC 服务端入口防刷、过载保护 | **golimit** |
| 按 IP / 用户 / 路径分别限流(per-key) | **golimit**(uber 不支持) |
| 超额要返回 429 + `Retry-After` / `X-RateLimit-*` 头 | **golimit**(见 [`docs/gin.md`](./docs/gin.md)) |
| 防伪造海量 key 耗内存的 DoS(`WithMaxKeys`) | **golimit**(uber 不支持) |
| 主动调用下游 API / DB / MQ,要把发送速率平滑匀速 | **uber-go/ratelimit** |
| 超额时希望**阻塞排队**而不是被拒绝 | **uber-go/ratelimit** |
| 单一全局速率、强制匀速抑制突发 | **uber-go/ratelimit** |

### 关键差异速查

| | golimit | uber-go/ratelimit |
|---|---|---|
| 算法 | 令牌桶(`x/time/rate`) | 漏桶(自研,零依赖) |
| 行为 | 非阻塞,超额即拒 | 阻塞,超额即等 |
| 核心 API | `Allow() bool` / `Check() Result` | `Take() time.Time` |
| per-key 隔离 | ✅ | ❌(单一速率) |
| 突发 | 允许(burst 容量) | 抑制(强制匀速) |
| 典型位置 | 服务端入口 | 客户端出口 |

> 提示:golimit 目前只提供**非阻塞**的 `Allow` / `Check`.若你需要"阻塞等待"式整流,请用 uber-go/ratelimit,或直接使用底层 `golang.org/x/time/rate` 的 `Wait(ctx)` / `Reserve()`.

---

## 维护者文档:发版与质量门 workflow

### 日常验证(改完代码必跑)

```bash
make release-check-all     # 两模块跑 vet + race + lint + vuln + sec + audit
```

底层等价于:
```bash
bash scripts/run-all.sh release-check
```

也支持单 target 跨模块:
```bash
make all-vet       # 两模块各跑 go vet
make all-test      # 两模块各跑 race 测试
make all-lint      # 两模块各跑 golangci-lint + gofumpt
make all-check     # 两模块各跑 govulncheck + gosec
make audit         # 仓库级发版审计(检测哪些 module 需要发版)
```

### 一键发版

```bash
make release       # 自动判定 + 按依赖顺序发所有需要发版的 module
```

发版流程:
1. 检查工作区 clean(否则 abort)
2. 跑 `scripts/check-modules.sh` 判定哪些 module 需要发版
3. 按依赖顺序排:`.`(v1) → `v2`(两者互相独立)
4. 每个待发模块自动:
   - 跑 `make release-check`(vet + race + lint + vuln + sec + audit 全套)
   - bump version.go 的 patch 号
   - commit + push HEAD
   - tag + push tag

### 预演发版(dry-run)

```bash
make release       # 实际发版
bash scripts/release.sh --dry   # 仅打印计划
```

dry-run 在工作区不 clean 时仍可预演,便于在 commit 前检查哪些模块会被发版。

### 单 module 发版(minor / major bump)

`make release` 仅支持 patch bump(`vX.Y.Z → vX.Y.Z+1`).如需 minor / major:

```bash
# 1. 手动改 version.go
vim v2/version.go    # 例如 v2.1.0 → v2.2.0

# 2. 单跑该 module 的 tag(内部跑 release-check)
cd v2 && make tag
```

### Tag 命名规范

| Module | Tag 前缀 | 示例 |
|---|---|---|
| 根(v1) | `v` | `v1.0.5` |
| v2 | `v2/v` | `v2/v2.2.0` |

`scripts/check-modules.sh` 按此前缀分别审计每个 module,**互不干扰**.

### 全局规则 4 合规

发版前 `release-check` 包含全局规则 4 要求的全部 checklist 项:
- `go vet ./...` 全模块
- `go test -race -count=1` 全模块
- `golangci-lint run` 全模块
- `govulncheck ./...` 全模块
- `gosec ./...` 全模块
- README 与代码同步(人工最后过一遍 CHANGELOG / README)

`make tag` 内部强制依赖 `release-check`,**任一项失败禁止发版**.

---

## v1 用户文档

### 安装

```bash
go get github.com/gtkit/golimit@latest
```

### 快速开始

```go
import "github.com/gtkit/golimit"

lim := golimit.NewLimiter("user:123:/api/v1", 100)
if !lim.Allow() {
    // 被限流了
}
```

- 同 key 多次 `NewLimiter` 时,新 rps 会**热更新**到现有 Limiter(stdlib SetLimit/SetBurst,并发安全).
- 首次调用启动后台清理 goroutine,生命周期与进程相同(不可关闭).

### v1 与 v2 的设计取舍

v1 走"全局单例 + 零配置"路线,简单直接:
- 全局 `GlobalLimiters` 共享所有 key
- cleanup goroutine 与进程生命周期一致
- `rps int` 参数同时控制 rate 与 burst

v2 在 v1 之外,**额外提供**这些能力(适合复杂场景,与 v1 共存):
- 实例化 `*Limiter`,多个独立注册表
- `Close()` 优雅关闭 cleanup goroutine
- `float64` rps,支持 fractional rate
- `Check(key) Result` 框架无关接口契约
- `WithMaxKeys` 防键基数 DoS、`WithOnReject` metrics 接入
- `Wait(ctx, key)` 阻塞式整流(平滑出口流量)
- 框架接入示例(见 [`docs/gin.md`](./docs/gin.md))

## License

MIT(详见 [LICENSE](./LICENSE))
