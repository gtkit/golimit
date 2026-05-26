.PHONY: lint check test vet audit release-check tag gittag help

LINT_TARGETS ?= ./...

help: ## 显示可用 target
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

lint: ## golangci-lint + gofumpt 格式化
	@ echo "▶️ golangci-lint run"
	golangci-lint run $(LINT_TARGETS)
	gofumpt -l -w .
	@ echo "✅ golangci-lint run"

check: ## govulncheck + gosec 安全扫描
	govulncheck ./...
	gosec ./...

vet: ## go vet
	GOWORK=off go vet ./...

test: ## race 测试 + count=1(禁缓存)
	GOWORK=off go test -race -count=1 -timeout=5m ./...

audit: ## 多 module 发版审计
	bash ../../scripts/check-modules.sh

## release-check 是 tag 前的必跑质量门(全局规则 4).
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
	git commit -m "chore(release): v2/gin $$new"; \
	printf "Release: %s\n" "$$new"; \
	git push gtkit HEAD; \
	git tag -a "v2/gin/$${new}" -m "release v2/gin/$$new"; \
	printf "Tag: v2/gin/%s\n" "$$new"; \
	git push gtkit "v2/gin/$${new}"; \
	printf "Done\n"

gittag: ## 显示 v2/gin 最新 tag
	git tag --sort=-version:refname | grep '^v2/gin/' | head -1
