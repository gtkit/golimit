#!/usr/bin/env bash
# scripts/check-modules.sh — 多 module 仓库发版审计
#
# 用途:本仓库内有两个独立 Go module,按全局规则 4-PRE 要求,
# 每次发版前必须自动检测每个模块"是否需要发版",避免漏发子模块。
#
#   - 根模块             github.com/gtkit/golimit          tag: vX.Y.Z(裸 tag)
#   - v2 核心            github.com/gtkit/golimit/v2       tag: v2/vX.Y.Z
#
# 用法:
#   bash scripts/check-modules.sh
#
# 退出码:0 = 全部模块均处于"已发版状态";1 = 至少一个模块需要发版。

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# 工作区脏检测:未提交的改动 tag 之前必须先 commit,否则发版会丢更新.
if ! git diff --quiet HEAD -- 2>/dev/null || [[ -n "$(git status --porcelain)" ]]; then
    echo "⚠ 工作区存在未提交变更,审计仅覆盖已 commit 的内容。"
    echo "   未提交的文件(节选):"
    git status --porcelain | head -20 | sed 's/^/   /'
    echo ""
fi

# module 路径 → tag 前缀映射.
declare -a MODULES=(
    ".:v1"      # 根模块,tag 形如 v1.X.Y
    "v2:v2"  # v2 模块,tag 形如 v2.X.Y(无前缀;/v2 后缀与子目录名 v2 重合,Go 剥离前缀)
)

NEED_RELEASE=0

for entry in "${MODULES[@]}"; do
    dir="${entry%%:*}"
    tag_prefix="${entry##*:}"

    # 找该模块最新 tag.
    latest_tag=$(git tag --list "${tag_prefix}.*.*" --sort=-version:refname | head -n1 || true)

    if [[ -z "$latest_tag" ]]; then
        echo "[$dir] ⚠ 未找到匹配前缀 '${tag_prefix}' 的 tag — 该模块从未发版"
        NEED_RELEASE=1
        continue
    fi

    # 检查该 tag 之后该路径是否有变更.各模块互斥统计:
    #   - 根模块:排除 v2/ + scripts/ + .github/ + *.md
    #   - v2 核心:仅 v2/ 下
    case "$dir" in
        .)
            changed=$(git diff --name-only "${latest_tag}..HEAD" -- \
                ':(exclude)v2' ':(exclude)scripts' ':(exclude).github' ':(exclude)*.md' \
                | grep -E '\.go$|^go\.(mod|sum)$' || true)
            ;;
        v2)
            changed=$(git diff --name-only "${latest_tag}..HEAD" -- "v2" \
                | grep -E '\.go$|/go\.(mod|sum)$' || true)
            ;;
    esac

    if [[ -z "$changed" ]]; then
        echo "[$dir] ✅ 自 ${latest_tag} 以来无代码变更"
    else
        echo "[$dir] 🔴 自 ${latest_tag} 以来有以下变更,需要发版:"
        echo "$changed" | sed 's/^/   - /'
        NEED_RELEASE=1
    fi
done

if [[ $NEED_RELEASE -eq 1 ]]; then
    echo ""
    echo "⚠ 至少一个模块需要发版。请按各 Makefile 的 tag target 流程逐个发布。"
    exit 1
fi

echo ""
echo "✅ 全部模块均已 release 至最新代码"
