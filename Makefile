.PHONY: lint tool check test vet audit release-check tag gittag delcommit help \
	all-vet all-test all-lint all-check release-check-all release

LINT_TARGETS ?= ./...

help: ## 显示可用 target
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-22s %s\n", $$1, $$2}'

# ================== 单模块(根 v1)级别 target ==================

lint: ## golangci-lint + gofumpt 格式化(本模块)
	@ echo "▶️ golangci-lint run"
	golangci-lint run $(LINT_TARGETS)
	gofumpt -l -w .
	@ echo "✅ golangci-lint run"

tool: lint ## 别名:同 lint(后向兼容)

check: ## govulncheck + gosec 安全扫描(本模块)
	govulncheck ./...
	gosec ./...

vet: ## go vet(本模块)
	GOWORK=off go vet ./...

test: ## race 测试 + count=1(本模块)
	GOWORK=off go test -race -count=1 -timeout=5m ./...

audit: ## 多 module 发版审计(仓库级,跑一次即可)
	bash scripts/check-modules.sh

## release-check 是单模块的 tag 前必跑质量门(全局规则 4).
## audit 是仓库级 advisory,不在此处包含 — 由顶层 release-check-all / release 单独跑.
release-check: vet test lint check
	@ echo "✅ release-check 全部通过"

tag: release-check ## 升 patch 版本并打 tag(自动跑 release-check 质量门)
	@current=$$(grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' version.go | head -n1 | tr -d 'v'); \
	if [ -z "$$current" ]; then echo "version not found in version.go"; exit 1; fi; \
	maj=$$(echo $$current | cut -d. -f1); \
	min=$$(echo $$current | cut -d. -f2); \
	patch=$$(echo $$current | cut -d. -f3); \
	newpatch=$$(expr $$patch + 1); \
	new="v$$maj.$$min.$$newpatch"; \
	printf "Bump: v%s -> %s\n" "$$current" "$$new"; \
	sed -E -i.bak 's/(const Version = ")([^"]+)(")/\1'"$$new"'\3/' version.go; \
	rm -f version.go.bak; \
	git add version.go; \
	git commit -m "chore(release): $$new"; \
	printf "Release: %s\n" "$$new"; \
	git push gtkit HEAD; \
	git tag -a "$$new" -m "release $$new"; \
	printf "Tag: %s\n" "$$new"; \
	git push gtkit "$$new"; \
	printf "Done\n"

gittag: ## 显示根模块最新 tag
	git tag --sort=-version:refname | grep -v '^v2/' | head -1

delcommit: ## 删除最近一次提交,但保留修改内容
	git reset --soft HEAD~1

# ================== 多模块(跨 v1 + v2 + v2/gin)自动化 target ==================

all-vet: ## 三模块 go vet
	bash scripts/run-all.sh vet

all-test: ## 三模块 race 测试
	bash scripts/run-all.sh test

all-lint: ## 三模块 golangci-lint + gofumpt
	bash scripts/run-all.sh lint

all-check: ## 三模块 govulncheck + gosec
	bash scripts/run-all.sh check

release-check-all: ## 三模块全套质量门(vet + race + lint + vuln + sec)+ advisory audit
	bash scripts/run-all.sh release-check
	@ echo ""
	@ echo "📋 发版审计(advisory):"
	@ bash scripts/check-modules.sh || true

release: ## 一键智能发版(audit 判定 + 按依赖顺序发所有需要的模块)
	bash scripts/release.sh
