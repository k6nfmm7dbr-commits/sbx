#!/usr/bin/env bash
# baseline_test.sh — v3.0.4 基线收口防回归：
#   A. 版本一致性：APP_VERSION == Go Version == README == sbx.sh（不依赖 Git Tag）
#   B. 无 Tag 静态检查：workflow 无 tag trigger / 无 git tag 操作
#   D. checksum mismatch → 安装 fail-closed（旧二进制不被覆盖）
#   J+. 敏感文件权限：首次创建 0600、已存在文件被收紧、无 umask 暴露窗口
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PASS=0; FAIL=0
ck() { if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 $2 实得 $3)"; fi; }
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

echo "== baseline_test =="

# ---- A. 版本一致性（无 Tag） ----
APP_VER=$(grep -m1 '^APP_VERSION=' "$ROOT/installer-template.sh" | sed -E 's/^APP_VERSION="?([^"]+)"?.*/\1/')
GO_VER=$(grep '^const Version = ' "$ROOT/internal/version/version.go" | sed 's/.*"\(.*\)".*/\1/')
README_VER=$(awk '/^## 当前版本/{f=1;next} f&&/^```text$/{g=1;next} g&&/^```$/{exit} g{print;exit}' "$ROOT/README.md" | tr -d 'v')
SBX_VER=$(grep -m1 '^APP_VERSION=' "$ROOT/sbx.sh" | sed -E 's/^APP_VERSION="?([^"]+)"?.*/\1/')
ck "APP_VERSION == 3.0.4" "3.0.4" "$APP_VER"
ck "Go Version == 3.0.4" "3.0.4" "$GO_VER"
ck "README 当前版本 == 3.0.4" "3.0.4" "$README_VER"
ck "sbx.sh 版本 == 3.0.4" "3.0.4" "$SBX_VER"
ck "四者完全一致" 1 "$([ "$APP_VER" == "$GO_VER" ] && [ "$GO_VER" == "$README_VER" ] && [ "$README_VER" == "$SBX_VER" ] && echo 1 || echo 0)"
# 终验脚本期望版本
for s in fresh_install_github e2e_remote fullinstall_remote; do
  grep -q '3\.0\.4' "$ROOT/scripts/$s.sh"; ck "终验脚本 $s 期望 3.0.4" 0 $?
done

# ---- B. 无 Tag 静态检查 ----
for wf in "$ROOT"/.github/workflows/*.yml; do
  grep -qE '^\s*tags:' "$wf"; [[ $? -ne 0 ]]; ck "$(basename "$wf") 无 tag trigger" 0 $?
  grep -qE 'git tag |git push.*--tags|push.*refs/tags' "$wf"; [[ $? -ne 0 ]]; ck "$(basename "$wf") 无 git tag 操作" 0 $?
done
grep -q 'git tag' "$ROOT/build.py" 2>/dev/null; [[ $? -ne 0 ]]; ck "build.py 无 tag 操作" 0 $?
grep -rn 'git tag\|git describe' "$ROOT"/scripts/*.sh 2>/dev/null | grep -v '^\s*#' | grep -q .; [[ $? -ne 0 ]]; ck "scripts 无 git tag/describe 依赖" 0 $?

# ---- D. checksum mismatch → fail-closed ----
TPL="$ROOT/installer-template.sh"
sed -n '/^# >>> checksum-helpers/,/^# <<< checksum-helpers/p' "$TPL" > "$TMPD/helpers.sh"
grep -q 'verify_core_checksum()' "$TMPD/helpers.sh" || { echo "未找到 checksum helpers"; exit 1; }
cat >> "$TMPD/helpers.sh" <<'STUBS'
die() { echo "[die] $*" >&2; exit 1; }
STUBS
# 构造假 SHA256SUMS：正确格式但校验和与夹具不匹配
printf '%s  sbx-core-linux-amd64\n' "0000000000000000000000000000000000000000000000000000000000000000" > "$TMPD/SHA256SUMS"
printf 'fake-binary' > "$TMPD/dl"
OUT=$( { set +u; source "$TMPD/helpers.sh"; verify_core_checksum "$TMPD/dl" "$TMPD/SHA256SUMS" "sbx-core-linux-amd64"; } 2>&1 ); RC=$?
ck "checksum mismatch → 非零退出" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
# 正确校验和 → 通过
GOOD=$(sha256sum "$TMPD/dl" | awk '{print $1}')
printf '%s  sbx-core-linux-amd64\n' "$GOOD" > "$TMPD/SHA256SUMS"
OUT=$( { set +u; source "$TMPD/helpers.sh"; verify_core_checksum "$TMPD/dl" "$TMPD/SHA256SUMS" "sbx-core-linux-amd64"; } 2>&1 ); RC=$?
ck "checksum 匹配 → 通过" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
# 缺失架构条目 → 拒绝
printf '%s  sbx-core-linux-arm64\n' "$GOOD" > "$TMPD/SHA256SUMS"
OUT=$( { set +u; source "$TMPD/helpers.sh"; verify_core_checksum "$TMPD/dl" "$TMPD/SHA256SUMS" "sbx-core-linux-amd64"; } 2>&1 ); RC=$?
ck "缺失架构条目 → 拒绝" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"

# ---- J+. 敏感文件权限（提取真实实现） ----
NEXT_FN=$(awk '/^prepare_dirs\(\)/{f=1;next} f&&/^[a-z_]+\(\)\s*\{/{print $1;exit}' "$TPL" | tr -d ' {')
[[ -n "$NEXT_FN" ]] || NEXT_FN="ensure_sb_config"
sed -n "/^prepare_dirs()/,/^${NEXT_FN}()/p" "$TPL" | sed '$d' > "$TMPD/pd.sh"
cat >> "$TMPD/pd.sh" <<'STUBS'
APP_DIR="$TMPD/app"; WEB_DIR="$TMPD/app/web"; SB_DIR="$TMPD/app/singbox"; CERT_DIR="$TMPD/app/certs"
NODES_JSON="$APP_DIR/nodes.json"; STATE_JSON="$APP_DIR/state.json"
STUBS
# 已存在但 0644 的旧文件 → 被收紧为 0600
mkdir -p "$TMPD/app"
printf '[{"id":1,"password":"secret"}]' > "$TMPD/app/nodes.json"; chmod 644 "$TMPD/app/nodes.json"
printf '{}' > "$TMPD/app/state.json"; chmod 644 "$TMPD/app/state.json"
( set +u; source "$TMPD/pd.sh"; prepare_dirs )
ck "已存在 0644 nodes.json → 收紧 600" "600" "$(stat -c %a "$TMPD/app/nodes.json")"
ck "已存在 0644 state.json → 收紧 600" "600" "$(stat -c %a "$TMPD/app/state.json")"
# 首次创建（umask 000 极端环境）
rm -rf "$TMPD/app"; mkdir -p "$TMPD/app"
( set +u; umask 000; source "$TMPD/pd.sh"; prepare_dirs )
ck "首次创建(umask 000) nodes.json == 600" "600" "$(stat -c %a "$TMPD/app/nodes.json")"
ck "首次创建(umask 000) state.json == 600" "600" "$(stat -c %a "$TMPD/app/state.json")"

# ensure_sb_config：首次创建 SB_CONF 为 0600（含 Reality 私钥的文件）
sed -n "/^ensure_sb_config()/,/^ensure_panel_conf()/p" "$TPL" | sed '$d' > "$TMPD/sbc.sh"
cat >> "$TMPD/sbc.sh" <<'STUBS'
SB_CONF="$TMPD/app/sing-box/config.json"
ok() { :; }
STUBS
mkdir -p "$TMPD/app/sing-box"
( set +u; umask 000; source "$TMPD/sbc.sh"; ensure_sb_config )
ck "sing-box config.json 首次创建 == 600" "600" "$(stat -c %a "$TMPD/app/sing-box/config.json")"

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
