#!/usr/bin/env bash
# hardening_flow_test.sh — 安装器加固测试（审计项「Shell 注入」）：
# 提取真实的 env-validation 区块，验证 SBX_GH_PROXY / SBX_ROOT /
# SBX_SCRIPT_SHA256 / SBX_SB_VERSION 的格式白名单。
# 测试的逻辑与发布安装器里的逻辑同源（区块提取）。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$ROOT/installer-template.sh"
PASS=0; FAIL=0

ck() { # ck <名称> <期望rc> <实际rc>
  if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1";
  else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 rc=$2 实得 rc=$3)"; fi
}

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

sed -n '/^# >>> env-validation/,/^# <<< env-validation/p' "$TPL" > "$TMPD/env.sh"
grep -q 'validate_env()' "$TMPD/env.sh" || { echo "未找到 validate_env（模板标记被破坏？）"; exit 1; }

cat > "$TMPD/prelude.sh" <<'PRE'
err() { echo "[err] $*" >&2; }
ok()  { echo "[ok] $*" >&2; }
PRE
cat "$TMPD/prelude.sh" "$TMPD/env.sh" > "$TMPD/lib.sh"

echo "== hardening_flow_test =="

# ---------------- 1. 环境变量格式白名单 ----------------
# 每个用例都在子 shell 内设置变量，避免赋值前缀在函数调用上的语义差异。
chk_env() { # chk_env <名称> <期望rc> <变量名> <值>
  local rc=0
  ( export "$3=$4"; source "$TMPD/lib.sh"; validate_env >/dev/null 2>&1 ) || rc=$?
  ck "$1" "$2" "$rc"
}

chk_env "未设置 GH_PROXY → 通过"            0 SBX_GH_PROXY ""
chk_env "合法 https 代理 → 通过"            0 SBX_GH_PROXY "https://ghfast.top/"
chk_env "合法 http 代理 → 通过"             0 SBX_GH_PROXY "http://127.0.0.1:8080"
chk_env "缺少 scheme → 拒绝"                1 SBX_GH_PROXY "ghfast.top/"
chk_env "含空格 → 拒绝"                     1 SBX_GH_PROXY "https://a b/"
chk_env "含反引号 → 拒绝"                   1 SBX_GH_PROXY 'https://a/`id`'
chk_env "含命令替换 → 拒绝"                 1 SBX_GH_PROXY 'https://a/$(id)'
chk_env "含分号 → 拒绝"                     1 SBX_GH_PROXY "https://a/;rm -rf /"

chk_env "合法沙箱前缀 → 通过"               0 SBX_ROOT "/tmp/sbx-sandbox"
chk_env "相对路径 → 拒绝"                   1 SBX_ROOT "tmp/sandbox"
chk_env "含 .. → 拒绝"                      1 SBX_ROOT "/tmp/../../etc"
chk_env "含空格 → 拒绝"                     1 SBX_ROOT "/tmp/a b"

chk_env "合法 sha256 → 通过"                0 SBX_SCRIPT_SHA256 "$(printf 'a%.0s' {1..64})"
chk_env "过短 sha256 → 拒绝"                1 SBX_SCRIPT_SHA256 "abcd"
chk_env "非十六进制 sha256 → 拒绝"          1 SBX_SCRIPT_SHA256 "$(printf 'z%.0s' {1..64})"

chk_env "合法版本号 → 通过"                 0 SBX_SB_VERSION "1.14.0"
chk_env "合法预发布版本 → 通过"             0 SBX_SB_VERSION "1.14.0-rc.1"
chk_env "非法版本号 → 拒绝"                 1 SBX_SB_VERSION "v1.14.0;rm -rf /"


echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
