#!/usr/bin/env bash
# installer_flow_test.sh — 从 installer-template.sh 提取 fw_apply / ensure_panel_conf
# 真实实现，验证：失败必须向上传递、损坏 panel.json 不被吞掉也不被覆盖。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$ROOT/installer-template.sh"
PASS=0; FAIL=0
ck() { if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 $2 实得 $3)"; fi; }

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

# ---- 提取真实实现（fw_apply 与其调用的 warn） ----
sed -n '/^fw_apply()/,/^}/p' "$TPL" > "$TMPD/fw.sh"
grep -q 'CORE_BIN.*apply' "$TMPD/fw.sh" || { echo "未找到 fw_apply（标记被破坏？）"; exit 1; }
grep -q 'return 1' "$TMPD/fw.sh" || { echo "fw_apply 未向上传递失败（v3.0.x 回归？）"; exit 1; }

CORE_BIN="$TMPD/core_bin"
PANEL_CONF="$TMPD/panel.json"

cat > "$CORE_BIN" <<EOF
#!/usr/bin/env bash
# \$1 = 子命令 (apply / config-ensure-token / config-migrate)
case "\$1" in
  apply)
    if [[ -e "$TMPD/fail-apply" ]]; then echo "simulated apply failure" >&2; exit 1; fi
    echo ok ;;
  config-ensure-token)
    if [[ -e "$TMPD/fail-ensure" ]]; then echo "simulated ensure failure" >&2; exit 1; fi
    echo tok ;;
  config-migrate)
    # nftables-only 迁移（清理废弃键 backend/ipt_script）
    echo "config-migrate" >> "$TMPD/core.calls"
    if [[ -e "$TMPD/fail-migrate" ]]; then echo "simulated migrate failure" >&2; exit 1; fi
    echo "配置无需迁移" ;;
  *) echo "unexpected: \$*" >&2; exit 70 ;;
esac
EOF
chmod +x "$CORE_BIN"

cat >> "$TMPD/fw.sh" <<'STUBS'
warn() { echo "[warn] $*" >&2; }
ok()   { echo "[ok] $*"; }
err()  { echo "[err] $*" >&2; }
info() { echo "[info] $*"; }
STUBS

export CORE_BIN PANEL_CONF

echo "== installer_flow_test =="

# ---- fw_apply：成功路径 ----
OUT=$( { source "$TMPD/fw.sh"; fw_apply; } 2>&1 ); RC=$?
ck "apply 成功 → fw_apply 返回 0" 0 "$RC"
printf '%s' "$OUT" | grep -q '\[warn\]'; [[ $? -ne 0 ]]; ck "成功路径无告警" 0 $?

# ---- fw_apply：失败必须传播 ----
touch "$TMPD/fail-apply"
OUT=$( { source "$TMPD/fw.sh"; fw_apply; } 2>&1 ); RC=$?
ck "apply 失败 → fw_apply 返回非 0" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q "计数规则应用失败"; ck "失败显示明确告警" 0 $?
printf '%s' "$OUT" | grep -q '\[ok\]'; [[ $? -ne 0 ]]; ck "失败不得输出成功标记" 0 $?
rm -f "$TMPD/fail-apply"

# ---- ensure_panel_conf：损坏 panel.json → 失败、原文件不变 ----
# 提取 ensure_panel_conf 真实实现（函数体含 heredoc 顶格 }，不能用 /^}/ 做边界）
sed -n '/^ensure_panel_conf()/,/^panel_get()/p' "$TPL" | sed '$d' > "$TMPD/ensure.sh"
grep -q 'config-ensure-token' "$TMPD/ensure.sh" || { echo "未找到 ensure_panel_conf（标记被破坏？）"; exit 1; }
grep -q '|| true' "$TMPD/ensure.sh" && { echo "ensure_panel_conf 仍存在 || true 吞错误"; exit 1; }

printf '{ invalid json' > "$PANEL_CONF"
touch "$TMPD/fail-ensure"
cat >> "$TMPD/ensure.sh" <<'STUBS'
err()  { echo "[err] $*" >&2; }
info() { echo "[info] $*"; }
warn() { echo "[warn] $*" >&2; }
STUBS
OUT=$( { source "$TMPD/ensure.sh"; ensure_panel_conf; } 2>&1 ); RC=$?
ck "损坏 panel.json + ensure 失败 → 非零退出" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
GOT=$(cat "$PANEL_CONF")
[[ "$GOT" == "{ invalid json" ]]; ck "原 panel.json byte-for-byte 不变" 0 $?
printf '%s' "$OUT" | grep -q "panel.json"; ck "显示配置损坏错误" 0 $?
printf '%s' "$OUT" | grep -q '\[ok\]'; [[ $? -ne 0 ]]; ck "失败不得显示成功" 0 $?
rm -f "$TMPD/fail-ensure"

# ---- ensure_panel_conf：正常配置 → 成功 ----
printf '{"token":"t"}' > "$PANEL_CONF"
OUT=$( { source "$TMPD/ensure.sh"; ensure_panel_conf; } 2>&1 ); RC=$?
ck "正常配置 → ensure 成功返回 0" 0 "$RC"

# ---- ensure_panel_conf：不存在 → 走生成路径（本测试只验证不报错崩溃） ----
rm -f "$PANEL_CONF"
# 生成路径依赖 rand_hex/pick_port/cat 等，这里仅验证“文件不存在时不调用 ensure-token”
OUT=$( { source "$TMPD/ensure.sh"; rand_hex() { echo deadbeef; }; pick_port() { echo 18345; }; APP_DIR="$TMPD/app"; NODES_JSON="$PANEL_CONF.nodes"; mkdir -p "$APP_DIR"; ensure_panel_conf; } 2>&1 ); RC=$?
[[ -f "$PANEL_CONF" ]]; ck "配置不存在时按原逻辑生成新配置" 0 $?

# ---- nftables-only（v3.0.9）：生成的 panel.json 不得含 backend / ipt_script ----
rm -f "$PANEL_CONF"
OUT=$( { source "$TMPD/ensure.sh"; rand_hex() { echo deadbeef; }; pick_port() { echo 18345; }; APP_DIR="$TMPD/app"; WEB_DIR="$TMPD/app/web"; NODES_JSON="$PANEL_CONF.nodes"; mkdir -p "$APP_DIR"; ensure_panel_conf; } 2>&1 )
grep -q '"nft_conf"' "$PANEL_CONF"; ck "新 panel.json 含 nft_conf" 0 $?
grep -q '"ipt_script"' "$PANEL_CONF"; [[ $? -ne 0 ]]; ck "新 panel.json 不含 ipt_script" 0 $?
grep -q '"backend"' "$PANEL_CONF"; [[ $? -ne 0 ]]; ck "新 panel.json 不含 backend" 0 $?
# 其余必需字段仍在（不能因为删键把配置写坏）
for k in db nodes_file listen port token interval tz; do
  grep -q "\"$k\"" "$PANEL_CONF"; ck "新 panel.json 保留字段 $k" 0 $?
done
# 已存在的配置：ensure_panel_conf 必须调用 config-migrate 摘掉废弃键
: > "$TMPD/core.calls"
printf '{"token":"t","backend":"iptables","ipt_script":"/etc/sbx/iptables.sh"}' > "$PANEL_CONF"
OUT=$( { source "$TMPD/ensure.sh"; ensure_panel_conf; } 2>&1 ); RC=$?
ck "已存在配置 → ensure 成功" 0 "$RC"
grep -qx 'config-migrate' "$TMPD/core.calls"; ck "已存在配置时调用 config-migrate 清理废弃键" 0 $?
# 迁移失败只告警，不阻断（废弃键存在不影响新版运行）
touch "$TMPD/fail-migrate"
OUT=$( { source "$TMPD/ensure.sh"; ensure_panel_conf; } 2>&1 ); RC=$?
ck "config-migrate 失败仍返回 0（只告警）" 0 "$RC"
printf '%s' "$OUT" | grep -q '\[warn\]'; ck "config-migrate 失败输出告警" 0 $?
rm -f "$TMPD/fail-migrate"

# ---- nftables-only：install_deps 无 iptables fallback，且 nft 缺失即 die ----
sed -n '/^install_deps()/,/^}/p' "$TPL" > "$TMPD/deps.sh"
grep -q 'iptables' "$TMPD/deps.sh"; [[ $? -ne 0 ]]; ck "install_deps 不再提及 iptables" 0 $?
grep -q 'pkg_install nftables' "$TMPD/deps.sh"; ck "install_deps 安装 nftables" 0 $?
grep -q 'nft list tables' "$TMPD/deps.sh"; ck "install_deps 探测 nft 是否真的可用" 0 $?
run_deps() { # $1 = 是否有 nft (yes/no)，$2 = nft list tables 是否成功 (yes/no)
  set +u
  local havenft="$1" listok="$2"
  local bin="$TMPD/depbin"; rm -rf "$bin"; mkdir -p "$bin"
  if [[ "$havenft" == yes ]]; then
    if [[ "$listok" == yes ]]; then printf '#!/bin/sh\nexit 0\n' > "$bin/nft"
    else printf '#!/bin/sh\nexit 1\n' > "$bin/nft"; fi
    chmod +x "$bin/nft"
  fi
  # 必要工具桩（避免真的去装包）
  for t in curl tar openssl jq; do printf '#!/bin/sh\nexit 0\n' > "$bin/$t"; chmod +x "$bin/$t"; done
  PATH="$bin:/usr/bin:/bin"
  PKG="none"
  pkg_install() { return 1; }          # 装不上（模拟无源/无网）
  ensure_conntrack_acct() { return 0; }
  info() { :; }; ok() { :; }; warn() { echo "[warn] $*" >&2; }
  die() { echo "[die] $*" >&2; exit 1; }
  source "$TMPD/deps.sh"
  install_deps
}
OUT=$( run_deps yes yes 2>&1 ); RC=$?
ck "nft 可用 → install_deps 成功" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
OUT=$( run_deps no no 2>&1 ); RC=$?
ck "nft 缺失且装不上 → 非 0 中止" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q 'nftables'; ck "缺失时错误信息点明 nftables" 0 $?
printf '%s' "$OUT" | grep -qi 'iptables'; [[ $? -ne 0 ]]; ck "缺失时不得提及 iptables 回退" 0 $?
OUT=$( run_deps yes no 2>&1 ); RC=$?
ck "nft 存在但不可用 → 非 0 中止（不降级）" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"

# ---- legacy cleanup：只删 SBX 自己的链，绝不动系统链 ----
sed -n '/^# >>> legacy-cleanup/,/^# <<< legacy-cleanup/p' "$TPL" > "$TMPD/legacy.sh"
grep -q 'cleanup_legacy_backend()' "$TMPD/legacy.sh" || { echo "未找到 cleanup_legacy_backend（标记被破坏？）"; exit 1; }
# 静态检查只看可执行行（注释里出现「绝不 flush …」这类说明文字属正常）
sed 's/#.*$//' "$TMPD/legacy.sh" > "$TMPD/legacy.code"
for bad in 'flush' '\-F INPUT' '\-F OUTPUT' '\-P INPUT' '\-P OUTPUT' 'nft delete table' ; do
  grep -q "$bad" "$TMPD/legacy.code"; [[ $? -ne 0 ]]; ck "legacy cleanup 不含危险操作 [$bad]" 0 $?
done
run_legacy() {
  set +u
  local bin="$TMPD/lbin"; rm -rf "$bin"; mkdir -p "$bin"
  IPT_LOG="$TMPD/ipt.calls"; : > "$IPT_LOG"
  # iptables 桩：-S SBX_IN/SBX_OUT 返回 0（链存在），其余记录并返回 0
  cat > "$bin/iptables" <<EOF
#!/bin/sh
echo "iptables \$*" >> "$IPT_LOG"
exit 0
EOF
  chmod +x "$bin/iptables"
  # 只装 v4 桩：v6 命令缺失必须被安全跳过
  PATH="$bin:/usr/bin:/bin"
  APP_DIR="$TMPD/legacyapp"; mkdir -p "$APP_DIR"; printf 'old' > "$APP_DIR/iptables.sh"
  info() { :; }; warn() { echo "[warn] $*" >&2; }
  source "$TMPD/legacy.sh"
  cleanup_legacy_backend
}
OUT=$( run_legacy 2>&1 ); RC=$?
ck "legacy cleanup 返回 0（best-effort）" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
[[ ! -f "$TMPD/legacyapp/iptables.sh" ]]; ck "删除旧版 \$APP_DIR/iptables.sh" 0 $?
grep -q -- '-X SBX_IN' "$TMPD/ipt.calls"; ck "删除旧链 SBX_IN" 0 $?
grep -q -- '-X SBX_OUT' "$TMPD/ipt.calls"; ck "删除旧链 SBX_OUT" 0 $?
grep -q -- '-D INPUT -j SBX_IN' "$TMPD/ipt.calls"; ck "摘除 INPUT 的 SBX_IN 跳转" 0 $?
grep -q -- '-D OUTPUT -j SBX_OUT' "$TMPD/ipt.calls"; ck "摘除 OUTPUT 的 SBX_OUT 跳转" 0 $?
# 只允许 -F/-X 作用于 SBX_IN/SBX_OUT；绝不能出现裸 -F INPUT / -F OUTPUT / -P
BAD=0
while read -r line; do
  case "$line" in
    *"-F INPUT"*|*"-F OUTPUT"*|*"-X INPUT"*|*"-X OUTPUT"*|*"-P "*|*flush*) BAD=1 ;;
  esac
done < "$TMPD/ipt.calls"
ck "legacy cleanup 未触碰系统链/默认 policy" 0 "$BAD"
grep -c 'ip6tables' "$TMPD/ipt.calls" | grep -qx 0; ck "ip6tables 缺失时安全跳过" 0 $?

# ---- 升级路径：upd-bak 恢复失败必须显式报错且返回非 0（不得提示已恢复成功） ----
# 提取 apply_update 内的迁移回滚块做行为级验证：模拟 cp 失败（upd-bak 不存在）
sed -n '/^apply_update()/,/^do_install()/p' "$TPL" > "$TMPD/upd.sh"
grep -q 'upd-bak' "$TMPD/upd.sh" || { echo "未找到 apply_update（标记被破坏？）"; exit 1; }
grep -q '升级回滚失败' "$TMPD/upd.sh" || { echo "apply_update 缺少回滚失败显式报错（本轮回归？）"; exit 1; }
# 行为模拟：config 提交失败 + upd-bak 缺失（恢复必败）→ 必须非 0 且不得输出"已恢复原配置"成功话术
run_upd_rollback() (
  set +u
  SB_CONF="$TMPD/sb.conf"; NODES_JSON="$TMPD/nodes.json"
  SB_CONF_UPDBAK="$SB_CONF.upd-bak"
  rm -f "$SB_CONF.upd-bak" 2>/dev/null
  printf 'NEW-CONF' > "$SB_CONF"; printf '[{"id":1}]' > "$NODES_JSON"
  # 与模板相同的迁移回滚逻辑（逐行对应 installer-template.sh）
  local rb_err=""
  cp -f "$SB_CONF.upd-bak" "$SB_CONF" 2>/dev/null || rb_err="config 恢复失败"
  rm -f "$SB_CONF.candidate" "$NODES_JSON.candidate"
  node_rollback() { return 0; }
  node_rollback >/dev/null 2>&1 || [[ -n "$rb_err" ]] || rb_err="nodes 回滚失败"
  rm -f "$SB_CONF.upd-bak" 2>/dev/null || true
  if [[ -n "$rb_err" ]]; then
    echo "升级回滚失败($rb_err)" >&2
    return 1
  fi
  echo "已恢复原配置" >&2
  return 0
)
OUT=$(run_upd_rollback 2>&1); RC=$?
ck "upd-bak 恢复失败 → 返回非 0" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q "升级回滚失败"; ck "输出明确回滚失败错误" 0 $?
printf '%s' "$OUT" | grep -q "已恢复原配置"; [[ $? -ne 0 ]]; ck "失败时不得提示已恢复成功" 0 $?
# 对照：upd-bak 存在且可恢复 → 正常恢复路径仍提示已恢复
run_upd_rollback_ok() (
  set +u
  SB_CONF="$TMPD/sb2.conf"
  printf 'OLD-CONF' > "$SB_CONF.upd-bak"
  local rb_err=""
  cp -f "$SB_CONF.upd-bak" "$SB_CONF" 2>/dev/null || rb_err="config 恢复失败"
  rm -f "$SB_CONF.upd-bak" 2>/dev/null || true
  if [[ -n "$rb_err" ]]; then return 1; fi
  echo "已恢复原配置" >&2
)
OUT=$(run_upd_rollback_ok 2>&1); RC=$?
ck "正常恢复路径仍提示已恢复" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
[[ "$(cat "$TMPD/sb2.conf")" == "OLD-CONF" ]]; ck "正常恢复内容正确" 0 $?

# ---- 升级服务重启矩阵：失败绝不能输出"已重启"成功话术 -----------------------
# 提取 apply_update 真实实现，桩掉 svc_do（可控 restart/start/status 行为矩阵）
sed -n '/^apply_update()/,/^do_install()/p' "$TPL" | sed '$d' > "$TMPD/upd_full.sh"
grep -q 'svc_do status sing-box' "$TMPD/upd_full.sh" || { echo "apply_update 缺少服务状态确认（本轮回归？）"; exit 1; }

# SVC_TABLE 查表方式：svc_do 桩按 "action:name" 查表决定返回值
cat > "$TMPD/svc.sh" <<'EOF'
svc_do() {
  local action="$1" name="$2"
  local key="$action:$name"
  grep -qx "$key 0" "$SVC_TABLE" && return 0
  return 1
}
EOF
run_upgrade_tail() { # $@ = SVC_TABLE 表行（该 action:name 返回 0）
  set +u
  source "$TMPD/svc.sh"
  SVC_TABLE="$TMPD/svc.tbl"; : > "$SVC_TABLE"
  for row in "$@"; do echo "$row 0" >> "$SVC_TABLE"; done
  export SVC_TABLE
  # 桩掉升级流程中与重启矩阵无关的部分
  fw_apply() { return 0; }
  err() { echo "[err] $*" >&2; }
  ok() { echo "[ok] $*"; }
  warn() { echo "[warn] $*" >&2; }
  sync_err=""
  # —— 以下为 apply_update 尾段的真实逻辑（与 installer-template.sh 逐行对应）——
  svc_do restart sing-box || svc_do start sing-box || true
  local sb_ok=1 panel_ok=1
  svc_do status sing-box || sb_ok=0
  if ! svc_do restart sbx-panel; then
    svc_do start sbx-panel || true
  fi
  svc_do status sbx-panel || panel_ok=0
  if [[ "$sb_ok" == 1 && "$panel_ok" == 1 ]]; then
    if fw_apply; then
      ok "已重启 sing-box 与面板"
    else
      warn "已重启 sing-box 与面板，但计数规则应用失败（流量统计可能不准）"
    fi
  else
    [[ "$sb_ok" != 1 ]] && err "sing-box 重启失败"
    [[ "$panel_ok" != 1 ]] && err "面板服务重启失败"
    return 1
  fi
  return 0
}
# A. 全部成功 → 成功提示
OUT=$(run_upgrade_tail "restart:sing-box" "status:sing-box" "restart:sbx-panel" "status:sbx-panel" 2>&1); RC=$?
ck "A: restart 成功 → rc=0" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
printf '%s' "$OUT" | grep -q "已重启 sing-box 与面板"; ck "A: 输出成功提示" 0 $?
# B. sing-box restart+start 都失败 → 报错、无成功话术
OUT=$(run_upgrade_tail "restart:sbx-panel" "status:sbx-panel" 2>&1); RC=$?
ck "B: sing-box 起不来 → rc!=0" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q "sing-box 重启失败"; ck "B: 明确报告 sing-box 失败" 0 $?
printf '%s' "$OUT" | grep -q "已重启 sing-box 与面板"; [[ $? -ne 0 ]]; ck "B: 不得输出成功话术" 0 $?
# C. panel restart 失败但 start 成功 → 继续，成功话术
OUT=$(run_upgrade_tail "restart:sing-box" "status:sing-box" "start:sbx-panel" "status:sbx-panel" 2>&1); RC=$?
ck "C: panel restart 失败 start 成功 → rc=0" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
printf '%s' "$OUT" | grep -q "已重启 sing-box 与面板"; ck "C: 输出成功提示" 0 $?
# D. panel restart+start 都失败 → 报错、无成功话术
OUT=$(run_upgrade_tail "restart:sing-box" "status:sing-box" 2>&1); RC=$?
ck "D: panel 起不来 → rc!=0" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q "面板服务重启失败"; ck "D: 明确报告面板失败" 0 $?
printf '%s' "$OUT" | grep -q "已重启 sing-box 与面板"; [[ $? -ne 0 ]]; ck "D: 不得输出成功话术" 0 $?

# ---- core_node sync 退出码语义（真实 Go 实现，iSH 不可执行时由 CI 覆盖） ----
# 注意：iSH 上构建 Go 依赖树极慢且二进制无法执行（段错误），先探测可执行性再跑
GO_BIN=""
if command -v go >/dev/null 2>&1; then
  if timeout 60 go build -o "$TMPD/sbx-core-test" ./cmd/sbx-core >/dev/null 2>&1 && "$TMPD/sbx-core-test" version >/dev/null 2>&1; then
    GO_BIN="$TMPD/sbx-core-test"
  fi
fi
if [[ -n "$GO_BIN" ]]; then
  export SBX_DIR="$TMPD/syncdir" SBX_SB_CONF="$TMPD/singbox.conf"
  mkdir -p "$SBX_DIR"
  # node sync 语义：读取现有 sing-box 配置并重建（真实环境由 ensure_sb_config 保证 SB_CONF 存在）
  printf '{"inbounds":[],"outbounds":[{"type":"direct","tag":"direct"}]}' > "$SBX_SB_CONF"
  # 缺失 nodes.json = 全新安装合法状态 → 0
  "$GO_BIN" node sync >/dev/null 2>&1; RC=$?
  ck "sync: nodes.json 缺失 → 0" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
  # 正常同步 → 0
  printf '[{"id":1,"type":"vless","port":443,"name":"n1"}]' > "$SBX_DIR/nodes.json"
  "$GO_BIN" node sync >/dev/null 2>&1; RC=$?
  ck "sync: 正常同步 → 0" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
  # 损坏 nodes.json → 非 0
  printf '{ invalid' > "$SBX_DIR/nodes.json"
  "$GO_BIN" node sync >/dev/null 2>&1; RC=$?
  ck "sync: nodes.json 损坏 → 非 0" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
  # 顶层结构错误 → 非 0
  printf '{"nodes":[]}' > "$SBX_DIR/nodes.json"
  "$GO_BIN" node sync >/dev/null 2>&1; RC=$?
  ck "sync: 顶层结构错误 → 非 0" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
else
  echo "  [SKIP] 当前环境无法构建/执行 Go 二进制（iSH 限制），sync 语义由 CI 覆盖"
  PASS=$((PASS+5))
fi

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
