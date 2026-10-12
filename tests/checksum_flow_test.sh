#!/usr/bin/env bash
# checksum_flow_test.sh — 从 installer-template.sh 提取真实的校验函数，
# 用固定夹具验证：存在/缺失/重复/错误 hash/格式异常 五类行为。
# 保证“测试的逻辑”与“发布安装器里的逻辑”永远同源。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$ROOT/installer-template.sh"
PASS=0; FAIL=0

ck() { # ck <名称> <期望rc> <实际rc>
  if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1";
  else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 rc=$2 实得 rc=$3)"; fi
}

# ---- 提取安装器内的真实实现 ----
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT
sed -n '/^# >>> checksum-helpers/,/^# <<< checksum-helpers/p' "$TPL" > "$TMPD/helpers.sh"
grep -q 'verify_core_checksum()' "$TMPD/helpers.sh" || { echo "未找到 verify_core_checksum（模板标记被破坏？）"; exit 1; }
# warn 在隔离环境下重定向到 stdout 便于断言
printf '%s\n' 'warn() { echo "[warn] $*" >&2; }' 'ok() { echo "[ok] $*"; }' > "$TMPD/prelude.sh"
cat "$TMPD/prelude.sh" "$TMPD/helpers.sh" > "$TMPD/lib.sh"
# shellcheck disable=SC1090
source "$TMPD/lib.sh"

BIN="$TMPD/fake-binary"
printf 'FAKE-CORE-BINARY-CONTENT\n' > "$BIN"

mksums() { # mksums <文件内容行...>
  printf '%s\n' "$@" > "$TMPD/SUMS"
}

GOOD_HASH=$(sha256_of "$BIN")

echo "== checksum_flow_test =="

# 1. 正确条目 → 通过
mksums "$GOOD_HASH  sbx-core-linux-amd64"
verify_core_checksum "$BIN" "$TMPD/SUMS" "sbx-core-linux-amd64" 2>/dev/null; ck "正确 hash → 成功" 0 $?

# 2. 缺失目标架构 → 失败
mksums "$GOOD_HASH  sbx-core-linux-arm64"
verify_core_checksum "$BIN" "$TMPD/SUMS" "sbx-core-linux-amd64" 2>/dev/null; ck "缺失目标条目 → 失败" 1 $?

# 3. 校验和错误 → 失败
mksums "0000000000000000000000000000000000000000000000000000000000000000  sbx-core-linux-amd64"
verify_core_checksum "$BIN" "$TMPD/SUMS" "sbx-core-linux-amd64" 2>/dev/null; ck "hash 不匹配 → 失败" 1 $?

# 4. 重复条目 → 失败
mksums "$GOOD_HASH  sbx-core-linux-amd64" "$GOOD_HASH  sbx-core-linux-amd64"
verify_core_checksum "$BIN" "$TMPD/SUMS" "sbx-core-linux-amd64" 2>/dev/null; ck "重复条目 → 失败" 1 $?

# 5. 格式异常（空 SUMS / 二进制缺失）→ 失败
: > "$TMPD/SUMS"
verify_core_checksum "$BIN" "$TMPD/SUMS" "sbx-core-linux-amd64" 2>/dev/null; ck "SUMS 为空 → 失败" 1 $?
mksums "$GOOD_HASH  sbx-core-linux-amd64"
verify_core_checksum "$TMPD/not-exist" "$TMPD/SUMS" "sbx-core-linux-amd64" 2>/dev/null; ck "二进制缺失 → 失败" 1 $?

# 6. 模拟完整安装器流程（amd64）：下载产物目录 + SHA256SUMS → 校验通过
mkdir -p "$TMPD/dist"
for a in amd64 arm64 armv7 armv6 386 s390x riscv64; do printf 'bin-%s\n' "$a" > "$TMPD/dist/sbx-core-linux-$a"; done
( cd "$TMPD/dist" && sha256sum sbx-core-linux-* > SHA256SUMS )
ALL_OK=0
for a in amd64 arm64 armv7 armv6 386 s390x riscv64; do
  verify_core_checksum "$TMPD/dist/sbx-core-linux-$a" "$TMPD/dist/SHA256SUMS" "sbx-core-linux-$a" 2>/dev/null || ALL_OK=1
done
ck "全架构模拟安装器校验 → 全部通过" 0 $ALL_OK

# 篡改一个产物后必须能发现
printf 'tampered' >> "$TMPD/dist/sbx-core-linux-386"
TAMPER=0
verify_core_checksum "$TMPD/dist/sbx-core-linux-386" "$TMPD/dist/SHA256SUMS" "sbx-core-linux-386" 2>/dev/null || TAMPER=1
ck "篡改产物 → 被检出" 1 $TAMPER

# 7. 在线升级脚本校验 helper：哈希缺失/不匹配 fail-closed，匹配才成功
SCRIPT="$TMPD/remote-sbx.sh"
printf '#!/usr/bin/env bash\nAPP_VERSION="3.0.30"\n' > "$SCRIPT"
SCRIPT_HASH=$(sha256_of "$SCRIPT")
verify_remote_script "$SCRIPT" "" "测试空锚" 2>/dev/null; ck "脚本哈希为空 → 失败" 1 $?
verify_remote_script "$SCRIPT" "$(printf 'b%.0s' {1..64})" "测试错误锚" 2>/dev/null; ck "脚本哈希不匹配 → 失败" 1 $?
verify_remote_script "$SCRIPT" "$SCRIPT_HASH" "测试正确锚" >/dev/null 2>&1; ck "脚本哈希匹配 → 成功" 0 $?
verify_remote_script "$TMPD/not-exist" "$SCRIPT_HASH" "测试缺失脚本" 2>/dev/null; ck "脚本文件缺失 → 失败" 1 $?

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
