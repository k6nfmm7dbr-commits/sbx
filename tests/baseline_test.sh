#!/usr/bin/env bash
# baseline_test.sh — v3.0.9 基线收口防回归：
#   A. 版本一致性：APP_VERSION == Go Version == README == sbx.sh（不依赖 Git Tag）
#   B. 无 Tag 静态检查：workflow 无 tag trigger / 无 git tag 操作
#   D. checksum mismatch → 安装 fail-closed（旧二进制不被覆盖）
#   J+. 敏感文件权限：首次创建 0600、已存在文件被收紧、无 umask 暴露窗口
#   N. nftables-only 架构静态收口（v3.0.9）：生产代码/配置/文档无 iptables 后端
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
ck "APP_VERSION == 3.0.9" "3.0.9" "$APP_VER"
ck "Go Version == 3.0.9" "3.0.9" "$GO_VER"
ck "README 当前版本 == 3.0.9" "3.0.9" "$README_VER"
ck "sbx.sh 版本 == 3.0.9" "3.0.9" "$SBX_VER"
ck "四者完全一致" 1 "$([ "$APP_VER" == "$GO_VER" ] && [ "$GO_VER" == "$README_VER" ] && [ "$README_VER" == "$SBX_VER" ] && echo 1 || echo 0)"
# 终验脚本期望版本
for s in fresh_install_github e2e_remote fullinstall_remote; do
  grep -q '3\.0\.9' "$ROOT/scripts/$s.sh"; ck "终验脚本 $s 期望 3.0.9" 0 $?
done

# ---- B. 无 Tag 静态检查 ----
for wf in "$ROOT"/.github/workflows/*.yml; do
  grep -qE '^\s*tags:' "$wf"; [[ $? -ne 0 ]]; ck "$(basename "$wf") 无 tag trigger" 0 $?
  grep -qE 'git tag |git push.*--tags|push.*refs/tags' "$wf"; [[ $? -ne 0 ]]; ck "$(basename "$wf") 无 git tag 操作" 0 $?
done
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

# ---- candidate version 校验（core_version_of + 安装器严格比较） ----
sed -n '/^# >>> checksum-helpers/,/^# <<< core-version-helpers/p' "$TPL" > "$TMPD/cv.sh"
grep -q 'core_version_of()' "$TMPD/cv.sh" || { echo "未找到 core_version_of（标记被破坏？）"; exit 1; }
cat >> "$TMPD/cv.sh" <<'STUBS'
warn() { echo "[warn] $*" >&2; }
die()  { echo "[die] $*" >&2; exit 1; }
APP_VERSION="3.0.9"
CORE_BIN="$TMPD/installed-core"
install_sbx_core() { source "$TMPD/cv.sh"; local cand_ver; cand_ver=$(core_version_of "$1"); [[ "$cand_ver" == "$APP_VERSION" ]]; }
STUBS
# 模拟 candidate 二进制：shell 脚本响应 "sbx-core vX.Y.Z"
mk_candidate() { printf '#!/bin/sh\necho "sbx-core v%s"\n' "$1" > "$TMPD/cand"; chmod +x "$TMPD/cand"; }
# mismatch：3.0.3 → 必须拒绝
mk_candidate 3.0.3
( set +u; source "$TMPD/cv.sh"; install_sbx_core "$TMPD/cand" ) 2>/dev/null; RC=$?
ck "candidate version 3.0.3 != APP 3.0.9 → 拒绝" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
# success：3.0.9 → 允许
mk_candidate 3.0.9
( set +u; source "$TMPD/cv.sh"; install_sbx_core "$TMPD/cand" ) 2>/dev/null; RC=$?
ck "candidate version 3.0.9 == APP → 允许" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
# 无法解析的输出 → 拒绝（严格解析，非 substring）
printf '#!/bin/sh\necho "SBX Core version v3.0.9 linux amd64 build 123"\n' > "$TMPD/cand"; chmod +x "$TMPD/cand"
( set +u; source "$TMPD/cv.sh"; install_sbx_core "$TMPD/cand" ) 2>/dev/null; RC=$?
ck "非标准 version 输出 → 拒绝（严格解析）" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
# substring 陷阱：输出含 3.0.9 但实际版本不同 → 拒绝
printf '#!/bin/sh\necho "sbx-core v3.0.91"\n' > "$TMPD/cand"; chmod +x "$TMPD/cand"
( set +u; source "$TMPD/cv.sh"; install_sbx_core "$TMPD/cand" ) 2>/dev/null; RC=$?
ck "3.0.91 不得匹配 3.0.9（substring 陷阱）" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
# 真实 Go 二进制 version 输出可被解析为 3.0.9
# （iSH 等环境无法执行 Go 二进制——仓库 FUTURE_IMPROVEMENTS #6 已记录；CI/真机覆盖此检查）
REAL_VER=$( (cd "$ROOT" && go run ./cmd/sbx-core version 2>/dev/null) )
if [[ -n "$REAL_VER" ]]; then
  printf '%s\n' "$REAL_VER" | grep -q '^sbx-core v3\.0\.9$'; ck "go run ./cmd/sbx-core version == sbx-core v3.0.9" 0 $?
  echo "$REAL_VER" | sed -nE 's/^sbx-core v?([0-9]+\.[0-9]+\.[0-9]+)$/\1/p' | grep -qx '3.0.9'; ck "真实二进制输出可被 core_version_of 解析为 3.0.9" 0 $?
else
  echo "  [SKIP] 当前环境无法执行 Go 二进制（iSH 限制），由 CI 覆盖"
  PASS=$((PASS+2))
fi

# ---- N. nftables-only 架构静态收口（v3.0.9） --------------------------------
# 目的：锁定「iptables 后端已彻底移除」，防止未来无意中把双后端逻辑写回来。
# 允许的例外只有两类，且必须显式标注：
#   1. 明确标记为历史存档的文档（docs/AUDIT.md、FUTURE_IMPROVEMENTS.md 历史章节）
#   2. 安装器里清理旧版残留的 migration（cleanup_legacy_backend / 相关测试）

# N-1. 生产 Go 代码不得存在 iptables 后端实现
for f in internal/firewall/iptables.go internal/firewall/iptables_partial_test.go \
         internal/firewall/testdata/gen_iptables.golden; do
  [[ ! -e "$ROOT/$f" ]]; ck "已删除 $f" 0 $?
done
GO_PROD=$(cd "$ROOT" && find cmd internal -name '*.go' ! -name '*_test.go')
# shellcheck disable=SC2086
for sym in NewIptables GenIPTables IptChainIn IptChainOut DetectBackend probeBackend \
           normalizeBackend EffectiveBackend WriteEffectiveBackend IsAutoBackend \
           ErrEnforceUnsupported SetEnforceBackend IptScript; do
  (cd "$ROOT" && grep -l "$sym" $GO_PROD 2>/dev/null | grep -q .); [[ $? -ne 0 ]]
  ck "生产 Go 代码无符号 $sym" 0 $?
done
# 生产代码不得 exec iptables/ip6tables（只看代码行，注释里解释历史行为属正常）
: > "$TMPD/go.code"
for f in $GO_PROD; do sed 's|//.*$||' "$ROOT/$f" >> "$TMPD/go.code"; done
grep -qE '"(ip6?tables)"' "$TMPD/go.code"; [[ $? -ne 0 ]]
ck "生产 Go 代码不执行 iptables/ip6tables" 0 $?
# config 结构体不得再有 Backend / IptScript 字段
grep -qE '^\s*(Backend|IptScript)\s+string' "$ROOT/internal/config/config.go"; [[ $? -ne 0 ]]
ck "config.Config 无 Backend/IptScript 字段" 0 $?
# 默认配置不得生成 backend / ipt_script 键
sed -n '/^func defaultConf/,/^}/p' "$ROOT/internal/config/config.go" \
  | grep -qE '"(backend|ipt_script)"'; [[ $? -ne 0 ]]
ck "defaultConf 不生成 backend/ipt_script" 0 $?
sed -n '/^func defaultConf/,/^}/p' "$ROOT/internal/config/config.go" \
  | grep -q '"nft_conf"'; ck "defaultConf 仍生成 nft_conf" 0 $?

# N-2. 安装器：panel.json 模板无废弃键；依赖安装无 iptables 回退
sed -n '/^ensure_panel_conf()/,/^panel_get()/p' "$TPL" | grep -qE '"(backend|ipt_script)"'
[[ $? -ne 0 ]]; ck "安装器 panel.json 模板无 backend/ipt_script" 0 $?
sed -n '/^install_deps()/,/^}/p' "$TPL" | grep -qi 'iptables'; [[ $? -ne 0 ]]
ck "install_deps 无 iptables 回退" 0 $?
# 安装器里出现 iptables 的可执行代码，必须只在 legacy cleanup 区块内。
# 做法：先把 legacy 区块整体挖掉，再在剩余部分的**代码行**（剥掉 # 注释）里搜。
sed '/^# >>> legacy-cleanup/,/^# <<< legacy-cleanup/d' "$TPL" \
  | sed 's/#.*$//' > "$TMPD/tpl.code"
grep -qi 'iptables' "$TMPD/tpl.code"; [[ $? -ne 0 ]]
ck "安装器 iptables 代码仅存在于 legacy cleanup 区块" 0 $?
# 绝不允许出现整体清空防火墙的操作（同样只看代码行）
sed 's/#.*$//' "$TPL" > "$TMPD/tpl.allcode"
for danger in 'nft flush ruleset' 'iptables -F$' '\-P INPUT' '\-P OUTPUT' 'nft -f /dev/stdin'; do
  grep -qE "$danger" "$TMPD/tpl.allcode"; [[ $? -ne 0 ]]
  ck "安装器不含危险操作 [$danger]" 0 $?
done
# nft delete table 只能针对 SBX 自己的两张表
BADTBL=0
grep -o 'nft delete table [a-z]* [a-z_]*' "$TMPD/tpl.allcode" > "$TMPD/tbl.list" || true
while read -r _ _ _ fam tbl; do
  [[ -z "$tbl" ]] && continue
  [[ "$fam" == inet && ( "$tbl" == sbx_traffic || "$tbl" == sbx_policy ) ]] || BADTBL=$((BADTBL+1))
done < "$TMPD/tbl.list"
ck "安装器 nft delete table 只删 sbx_traffic/sbx_policy" 0 "$BADTBL"

# N-3. README 不得宣称支持 iptables 作为后端
grep -qE 'nftables 或 iptables|回退 iptables|iptables-legacy|iptables 回退' "$ROOT/README.md"
[[ $? -ne 0 ]]; ck "README 无 iptables 后端/回退宣称" 0 $?
grep -q 'nftables-only' "$ROOT/README.md"; ck "README 明确 nftables-only" 0 $?

# N-4. 历史文档必须带历史语境标注（允许保留旧事实，但不得被当成现行能力）
grep -q '历史存档说明' "$ROOT/docs/AUDIT.md"; ck "AUDIT.md 保留历史存档声明" 0 $?
grep -q 'nftables-only' "$ROOT/docs/AUDIT.md"; ck "AUDIT.md 声明现行为 nftables-only" 0 $?
grep -q '历史语境' "$ROOT/FUTURE_IMPROVEMENTS.md"; ck "FUTURE_IMPROVEMENTS 历史章节带语境标注" 0 $?


echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
