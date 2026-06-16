#!/usr/bin/env bash
# scripts/release.sh — 智能多 module 一键发版.
#
# 工作流程:
#   1. 工作区必须 clean(git status --porcelain 为空)
#   2. 跑 audit 判定哪些 module 需要发版
#   3. 按依赖顺序发版:.(v1) → v2(core)
#   4. 对每个待发模块:
#      - cd 进去跑 `make tag`,内部已包含 release-check 全套质量门
#      - tag 模板会自动 bump version.go(patch) + commit + tag + push
#   5. 任何一步失败立即 abort,不留半发版状态.
#
# 限制(简化设计):
#   - 只支持 patch bump(vX.Y.Z → vX.Y.Z+1).
#     需要 minor / major bump 请手动改 version.go 后单跑 `make tag`.
#   - 假设 git remote 名是 `gtkit`(各 module 的 Makefile tag 流程依赖此).
#
# 用法:
#   bash scripts/release.sh         # 自动判定 + 发版
#   bash scripts/release.sh --dry   # 只打印计划,不实际执行

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

DRY_RUN=0
if [[ "${1:-}" == "--dry" || "${1:-}" == "--dry-run" ]]; then
    DRY_RUN=1
    echo "🟡 DRY RUN 模式:只打印计划,不实际执行"
    echo ""
fi

run_cmd() {
    if [[ $DRY_RUN -eq 1 ]]; then
        printf "    [dry] %s\n" "$*"
    else
        eval "$@"
    fi
}

# ================== Step 1: 工作区 clean 检查 ==================
if [[ -n "$(git status --porcelain)" ]]; then
    if [[ $DRY_RUN -eq 1 ]]; then
        echo "⚠ 工作区存在未提交变更(dry-run 仍继续预演,实际发版前必须先 commit):"
        git status --short | sed 's/^/    /'
        echo ""
    else
        echo "🔴 工作区存在未提交变更,请先 commit 再发版:"
        git status --short | sed 's/^/    /'
        exit 1
    fi
fi

# ================== Step 2: 跑 audit 找需要发版的模块 ==================
echo "▶️ 跑 audit 判定..."
echo "----------------------------------------"
# audit 退出码 1 = 有模块需要发版(预期); 0 = 全部已是最新.
audit_output=$(bash scripts/check-modules.sh || true)
echo "$audit_output"
echo "----------------------------------------"
echo ""

# 解析 audit 输出:[<dir>] 🔴 ...  或  [<dir>] ⚠ ...
declare -a TO_RELEASE=()
while IFS= read -r line; do
    if [[ "$line" =~ ^\[(.+)\][[:space:]]+(🔴|⚠) ]]; then
        TO_RELEASE+=("${BASH_REMATCH[1]}")
    fi
done <<< "$audit_output"

if [[ ${#TO_RELEASE[@]} -eq 0 ]]; then
    echo "✅ 没有 module 需要发版,退出"
    exit 0
fi

# ================== Step 3: 按依赖顺序排列待发模块 ==================
# 依赖链:v1(根) 与 v2 互相独立.
declare -a ORDERED=()
for m in . v2; do
    for r in "${TO_RELEASE[@]}"; do
        if [[ "$r" == "$m" ]]; then
            ORDERED+=("$m")
            break
        fi
    done
done

echo "📦 计划发版顺序: ${ORDERED[*]}"
echo ""

if [[ $DRY_RUN -eq 1 ]]; then
    for m in "${ORDERED[@]}"; do
        printf "  - %s: cd %s && make tag\n" "$m" "$m"
    done
    echo ""
    echo "🟡 DRY RUN 结束,未做任何实际操作"
    exit 0
fi

# ================== Step 4: 逐个发版 ==================
for m in "${ORDERED[@]}"; do
    echo ""
    echo "🚀 开始发版: $m"
    echo "========================================"

    if ! (cd "$m" && make tag); then
        echo ""
        echo "🔴 $m 发版失败,abort.已发版的不会被回滚."
        if [[ ${#ORDERED[@]} -gt 1 ]]; then
            echo "    待发但未发: $(echo "${ORDERED[@]:$(($(echo "${ORDERED[@]}" | tr ' ' '\n' | grep -n "^$m$" | cut -d: -f1) - 1))}")"
        fi
        exit 1
    fi

    echo ""
    echo "✅ $m 发版成功"
done

echo ""
echo "========================================"
echo "✅ 全部发版完成"
echo "   发版顺序: ${ORDERED[*]}"
echo "========================================"
