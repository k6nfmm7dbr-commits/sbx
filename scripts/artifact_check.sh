#!/usr/bin/env bash
# artifact_check.sh — 发布产物一致性与版本一致性自检（Release 前必须全部通过）
#
# 用法: bash scripts/artifact_check.sh <dist目录> [已废弃: 第二参数被忽略]
#
# 检查项：
#   1. 七个架构产物全部存在
#   2. SHA256SUMS 中每个产物恰好出现一次
#   3. 实际 sha256 与 SHA256SUMS 记录一致
#   4. installer APP_VERSION / Go internal/version / 二进制 version 三方一致
#      （本项目不依赖 Git Tag，版本一致性不涉及任何 Git Tag）
set -euo pipefail

DIST="${1:-dist}"
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

# 版本一致性检查（无 Tag 模式，无条件执行）：
# installer APP_VERSION == Go internal/version == 二进制 version 输出。
# 本项目不依赖 Git Tag，因此不存在也不检查任何 Git Tag。
GO_VER="$(grep '^const Version = ' "$ROOT/internal/version/version.go" | sed 's/.*"\(.*\)".*/\1/')"
APP_VER="$(grep -m1 '^APP_VERSION=' "$ROOT/installer-template.sh" | sed -E 's/^APP_VERSION="?([^"]+)"?.*/\1/')"
BIN_VER="$(./sbx-core-linux-amd64 version | sed 's/^sbx-core v//')"

[[ "$GO_VER" == "$APP_VER" ]] || fail "internal/version($GO_VER) 与 installer APP_VERSION($APP_VER) 不一致"
[[ "$GO_VER" == "$BIN_VER" ]] || fail "internal/version($GO_VER) 与二进制 version 输出($BIN_VER) 不一致"
echo "  [OK] 版本三方一致: v$GO_VER（无 Git Tag）"

echo "[artifact-check] ALL OK"
