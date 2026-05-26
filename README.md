# golimit

轻量级、并发安全的 Go 限流库,基于 `golang.org/x/time/rate`(令牌桶算法).

仓库内有 **3 个独立 Go module**,按需引入:

| Module | 用途 | 直接依赖 | 推荐 |
|---|---|---|---|
| `github.com/gtkit/golimit` | v1,维护模式(仅修关键 bug) | `golang.org/x/time` | ❌ 新项目勿用 |
| `github.com/gtkit/golimit/v2` | **v2 核心**:零第三方,实例化,Check + Result | `golang.org/x/time` | ✅ 推荐 |
| `github.com/gtkit/golimit/v2/gin` | v2 的 Gin 框架适配 | core + `gin-gonic/gin` | ✅ Gin 用户 |

**不用 Gin 的用户只引 v2 核心,go.sum 仅 2 行,0 transitive vulnerability**.

详细文档:
- [`v2/README.md`](./v2/README.md) — 核心 API、Check/Result 用法、生产部署
- [`v2/gin/README.md`](./v2/gin/README.md) — Gin 中间件 / Skip / KeyFunc 用法
- [`CHANGELOG.md`](./CHANGELOG.md) — 版本变更历史

---

## 维护者文档:发版与质量门 workflow

### 日常验证(改完代码必跑)

```bash
make release-check-all     # 三模块跑 vet + race + lint + vuln + sec + audit
```

底层等价于:
```bash
bash scripts/run-all.sh release-check
```

也支持单 target 跨模块:
```bash
make all-vet       # 三模块各跑 go vet
make all-test      # 三模块各跑 race 测试
make all-lint      # 三模块各跑 golangci-lint + gofumpt
make all-check     # 三模块各跑 govulncheck + gosec
make audit         # 仓库级发版审计(检测哪些 module 需要发版)
```

### 一键发版

```bash
make release       # 自动判定 + 按依赖顺序发所有需要发版的 module
```

发版流程:
1. 检查工作区 clean(否则 abort)
2. 跑 `scripts/check-modules.sh` 判定哪些 module 需要发版
3. 按依赖顺序排:`.`(v1) → `v2` → `v2/gin`
4. 每个待发模块自动:
   - 跑 `make release-check`(vet + race + lint + vuln + sec + audit 全套)
   - bump version.go 的 patch 号
   - commit + push HEAD
   - tag + push tag
5. 发版 `v2` 后,若 `v2/gin` 也需要发版,**自动同步** `v2/gin/go.mod` 的 `require` 行指向新 v2 tag,再继续发 `v2/gin`

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
| v2 | `v2/v` | `v2/v2.1.0` |
| v2/gin | `v2/gin/v` | `v2/gin/v2.0.0` |

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

## v1 用户文档(维护模式 - 不再扩展功能)

> ⚠ v1 已停止新功能,仅接受关键 bug 修复.新项目请用 [v2](./v2/).

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

- 同 key 多次 `NewLimiter` 时,**自 v1.0.5 起新 rps 会热更新**(之前会被静默忽略).
- 首次调用启动后台清理 goroutine,**无法关闭**(进程级常驻).

### v1 限制(v2 已解决)

- 全局单例 `GlobalLimiters`,无法多实例隔离
- cleanup goroutine 无 Close() 接口,进程内永驻
- `rps int` 不支持小数(v2 用 `float64`)
- 无 KeyFunc / Skip / 自定义 Headers
- 无键基数上限(v2 提供 `WithMaxKeys` 防 DoS)
- 无拒绝回调(v2 提供 `WithOnReject` 接入 metrics)

## License

MIT(详见 [LICENSE](./LICENSE))
