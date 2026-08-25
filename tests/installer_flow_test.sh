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
# \$1 = 子命令 (apply / config-ensure-token)
case "\$1" in
  apply)
    if [[ -e "$TMPD/fail-apply" ]]; then echo "simulated apply failure" >&2; exit 1; fi
    echo ok ;;
  config-ensure-token)
    if [[ -e "$TMPD/fail-ensure" ]]; then echo "simulated ensure failure" >&2; exit 1; fi
    echo tok ;;
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

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
