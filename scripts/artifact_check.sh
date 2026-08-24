#!/usr/bin/env bash
# artifact_check.sh — 发布产物一致性与版本一致性自检（Release 前必须全部通过）
#
# 用法: bash scripts/artifact_check.sh <dist目录> [期望版本号(不带v前缀)]
#
# 检查项：
#   1. 七个架构产物全部存在
#   2. SHA256SUMS 中每个产物恰好出现一次
#   3. 实际 sha256 与 SHA256SUMS 记录一致
#   4. （提供版本号时）internal/version、installer APP_VERSION、
#      二进制 version 输出 三者与期望版本一致
set -euo pipefail

DIST="${1:-dist}"
EXPECT_TAG="${2:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$DIST"

fail() { echo "[artifact-check] FAIL: $*" >&2; exit 1; }

ARCHS=(amd64 arm64 armv7 armv6 386 s390x riscv64)

[[ -s "SHA256SUMS" ]] || fail "SHA256SUMS 缺失或为空"

for a in "${ARCHS[@]}"; do
  name="sbx-core-linux-$a"
  [[ -s "$name" ]] || fail "缺少产物 $name"
  n=$(awk -v x="$name" '$2==x{c++} END{print c+0}' SHA256SUMS)
  [[ "$n" == "1" ]] || fail "$name 在 SHA256SUMS 中出现 ${n} 次（应为 1 次）"
  exp=$(awk -v x="$name" '$2==x{print $1}' SHA256SUMS)
  got=$(sha256sum "$name" | awk '{print $1}')
  [[ "$exp" == "$got" ]] || fail "$name 校验和不匹配 (SUMS=$exp actual=$got)"
  echo "  [OK] $name"
done

# SHA256SUMS 不应包含未知条目（防串包/漏删）
while read -r sum name; do
  case "$name" in
    sbx-core-linux-amd64|sbx-core-linux-arm64|sbx-core-linux-armv7|\
    sbx-core-linux-armv6|sbx-core-linux-386|sbx-core-linux-s390x|sbx-core-linux-riscv64) ;;
    *) fail "SHA256SUMS 含意外条目: $name" ;;
  esac
done < SHA256SUMS

if [[ -n "$EXPECT_TAG" ]]; then
  tag="${EXPECT_TAG#v}"
  grep -q "^const Version = \"${tag}\"$" "$ROOT/internal/version/version.go" \
    || fail "internal/version 与期望版本 $tag 不一致"
  grep -q "^APP_VERSION=\"${tag}\"$" "$ROOT/installer-template.sh" \
    || fail "installer APP_VERSION 与期望版本 $tag 不一致"
  ./sbx-core-linux-amd64 version | grep -qx "sbx-core v${tag}" \
    || fail "二进制 version 输出与期望版本 $tag 不一致: $(./sbx-core-linux-amd64 version)"
  echo "  [OK] 版本三方一致: v$tag"
fi

echo "[artifact-check] ALL OK"
