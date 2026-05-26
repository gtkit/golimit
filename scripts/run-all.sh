#!/usr/bin/env bash
# scripts/run-all.sh — 在所有 module 上跑同一个 make target.
#
# 用法:
#   bash scripts/run-all.sh <make-target>
#
# 例如:
#   bash scripts/run-all.sh test           # 三模块都跑 race 测试
#   bash scripts/run-all.sh lint           # 三模块都跑 lint
#   bash scripts/run-all.sh release-check  # 三模块都跑 release-check 全套
#
# 行为:
#   - 遍历 . / v2 / v2/gin,逐个 cd 进去跑 make <target>
#   - 任一失败立即 exit 1
#   - 全部通过则 exit 0
#
# 设计:每个 module 的 Makefile 都暴露相同名字的 target(vet / test / lint /
# check / release-check 等),本脚本只做"循环 + 失败即停".

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

TARGET="${1:-}"
if [[ -z "$TARGET" ]]; then
    echo "用法: bash scripts/run-all.sh <make-target>"
    echo ""
    echo "可用 target(每个 module 都支持):"
    echo "  vet            go vet"
    echo "  test           go test -race -count=1"
    echo "  lint           golangci-lint + gofumpt"
    echo "  check          govulncheck + gosec"
    echo "  release-check  上述全部 + audit(等同发版前质量门)"
    exit 2
fi

MODULES=(. v2 v2/gin)
PASSED=()

for m in "${MODULES[@]}"; do
    printf "\n========== [%s] make %s ==========\n" "$m" "$TARGET"
    if ! (cd "$m" && make "$TARGET"); then
        printf "\n🔴 module '%s' 的 '%s' 失败,abort\n" "$m" "$TARGET"
        if [[ ${#PASSED[@]} -gt 0 ]]; then
            printf "    在此之前已通过: %s\n" "${PASSED[*]}"
        fi
        exit 1
    fi
    PASSED+=("$m")
done

printf "\n========================================\n"
printf "✅ 全部 %d 个 module 都跑完 '%s' 通过\n" "${#PASSED[@]}" "$TARGET"
printf "   通过的 module: %s\n" "${PASSED[*]}"
printf "========================================\n"
