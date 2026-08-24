#!/usr/bin/env bash
# commit_flow_test.sh — 从 installer-template.sh 提取真实的 commit_node 流程，
# 用桩依赖验证回滚一致性。覆盖场景：
#   1. 全部成功（提交成功、重启成功、备份清理）
#   2. config 提交成功但 nodes 提交失败 → 必须整体回滚且退出非 0
#   3. sing-box check 失败 → 不触碰任何正式文件
#   4. restart 持续失败 → 用仍存在的 .bak 真正回滚，退出非 0（v3.0.3 防回归）
#   5. restart 瞬时失败 → 回滚后二次重启成功，旧配置恢复运行，仍退出非 0
#   6. config 备份创建失败 → 立即中止：正式文件不变、不提交、不重启
#   7. nodes 备份创建失败 → 行为同上
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$ROOT/installer-template.sh"
PASS=0; FAIL=0
ck() { if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 $2 实得 $3)"; fi; }

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

# ---- 提取真实实现 ----
sed -n '/^# >>> commit-node-flow/,/^# <<< commit-node-flow/p' "$TPL" > "$TMPD/flow.sh"
grep -q 'commit_node()' "$TMPD/flow.sh" || { echo "未找到 commit_node（标记被破坏？）"; exit 1; }
grep -q 'restore_old' "$TMPD/flow.sh" || { echo "commit_node 缺少 restore_old（v3.0.3 回滚逻辑被破坏？）"; exit 1; }

SB_CONF="$TMPD/sing-box/config.json"
NODES_JSON="$TMPD/sbx/nodes.json"
mkdir -p "$(dirname "$SB_CONF")" "$(dirname "$NODES_JSON")"

FAKE_BIN="$TMPD/bin"; mkdir -p "$FAKE_BIN"
FAIL_FLAG="$TMPD/fail-node-commit"
RESTART_FLAG="$TMPD/fail-restart"
RESTART_MODE="$TMPD/restart-mode"      # once=仅第一次失败; always=持续失败
RESTART_LOG="$TMPD/restarts.log"

cat > "$FAKE_BIN/core_node" <<EOF
#!/usr/bin/env bash
# 真实调用形如: core_node node <sub>
[[ "\$1" == "node" ]] || { echo "unexpected first arg: \$1" >&2; exit 70; }
sub="\$2"
case "\$sub" in
  commit)
    if [[ -e "$FAIL_FLAG" ]]; then echo "simulated nodes commit failure" >&2; exit 9; fi
    [[ -f "$NODES_JSON.candidate" ]] && mv -f "$NODES_JSON.candidate" "$NODES_JSON"
    echo ok ;;
  rollback)
    rm -f "$NODES_JSON.candidate" "$SB_CONF.candidate" 2>/dev/null
    echo ok ;;
  *) echo "unexpected sub: \$sub" >&2; exit 70 ;;
esac
EOF
printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKE_BIN/sing-box"

# cp 拦截桩：模拟“原文件存在但备份创建失败”（对 root 权限测试环境也可靠）。
# 运行时读取 CP_BLOCK_SRC/DST（导出后生效），为空时不拦截任何调用。
cat > "$FAKE_BIN/cp" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${CP_BLOCK_SRC:-}" && "$1" == "-f" && "$2" == "${CP_BLOCK_SRC:-x}" && "$3" == "${CP_BLOCK_DST:-y}" ]]; then
  echo "simulated backup creation failure" >&2
  exit 1
fi
exec /bin/cp "$@"
EOF
chmod +x "$FAKE_BIN"/*

cat >> "$TMPD/flow.sh" <<'STUBS'

warn() { echo "[warn] $*" >&2; }
ok()   { echo "[ok] $*"; }
info() { echo "[info] $*"; }
die()  { echo "[die] $*" >&2; exit 1; }
sb_restart() {
  echo restart >> "$RESTART_LOG"
  if [[ -e "$RESTART_FLAG" ]]; then
    [[ "$(cat "$RESTART_MODE" 2>/dev/null)" == "once" ]] && rm -f "$RESTART_FLAG"
    return 1
  fi
  return 0
}
fw_apply() { :; }
panel_running() { return 1; }
svc_do() { :; }
STUBS

export SB_CONF NODES_JSON
export SB_BIN="$FAKE_BIN/sing-box"
export CORE_BIN="$FAKE_BIN/core_node"
export PATH="$FAKE_BIN:$PATH"
export RESTART_FLAG RESTART_MODE RESTART_LOG

seed_old() {
  printf '{"inbounds":["OLD"]}\n'     > "$SB_CONF"
  printf '[{"id":1,"old":true}]\n'    > "$NODES_JSON"
}
prep_candidate() {
  printf '{"inbounds":["NEW"]}\n'     > "$SB_CONF.candidate"
  printf '[{"id":1,"new":true},{"id":2,"added":true}]\n' > "$NODES_JSON.candidate"
}

run_flow() { ( set +u; source "$TMPD/flow.sh"; commit_node ); }

# expect_file <实际文件> <期望内容(不含结尾换行)> —— 兼容无 /dev/fd 的环境
OLD_CONF='{"inbounds":["OLD"]}'
NEW_CONF='{"inbounds":["NEW"]}'
OLD_NODES='[{"id":1,"old":true}]'
NEW_NODES='[{"id":1,"new":true},{"id":2,"added":true}]'
expect_file() {
  printf '%s\n' "$2" > "$TMPD/.expected"
  cmp -s "$TMPD/.expected" "$1"
}
restart_count() { [[ -f "$RESTART_LOG" ]] && wc -l < "$RESTART_LOG" | tr -d ' ' || echo 0; }

echo "== commit_flow_test =="

# ---- 场景一：全部成功 ----
seed_old; prep_candidate; rm -f "$FAIL_FLAG" "$RESTART_FLAG"; : > "$RESTART_LOG"
ERR1=$( run_flow 2>&1 >/dev/null ); RC1=$?
ck "成功流程退出码 0" 0 "$([ "$RC1" == 0 ] && echo 0 || echo 1)"
[[ -z "$(printf '%s' "$ERR1" | grep '\[warn\]')" ]]; ck "成功路径无告警输出" 0 $?
expect_file "$SB_CONF" "$NEW_CONF";   ck "config 已提交为候选内容" 0 $?
expect_file "$NODES_JSON" "$NEW_NODES"; ck "nodes 已提交为候选内容" 0 $?
[[ ! -f "$SB_CONF.bak" && ! -f "$NODES_JSON.bak" ]]; ck "备份文件已清理" 0 $?
ck "成功流程恰好重启一次" 1 "$(restart_count)"

# ---- 场景二：config 提交成功但 nodes 提交失败 → 必须回滚 ----
seed_old; prep_candidate; touch "$FAIL_FLAG"; : > "$RESTART_LOG"
ERR2=$( run_flow 2>&1 >/dev/null ); RC2=$?
ck "nodes 提交失败 → 命令退出码非 0" 1 "$([ "$RC2" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "config 回滚到修改前内容" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "nodes 保持修改前内容" 0 $?
[[ ! -f "$NODES_JSON.candidate" ]]; ck "失败后不残留候选文件" 0 $?
printf '%s' "$ERR2" | grep -q "回滚";           ck "向用户显示明确回滚错误" 0 $?
printf '%s' "$ERR2" | grep -q "simulated nodes commit failure"; ck "底层错误被透出给用户" 0 $?

# ---- 场景三：sing-box check 失败 → 不触碰任何正式文件 ----
seed_old; prep_candidate; : > "$RESTART_LOG"
printf '#!/usr/bin/env bash\nexit 1\n' > "$FAKE_BIN/sing-box"; chmod +x "$FAKE_BIN/sing-box"
ERR3=$( run_flow 2>&1 >/dev/null ); RC3=$?
ck "check 失败 → 非零退出" 1 "$([ "$RC3" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "check 失败 config 未动" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "check 失败 nodes 未动" 0 $?
printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKE_BIN/sing-box"; chmod +x "$FAKE_BIN/sing-box"

# ---- 场景四：restart 持续失败 → .bak 仍在，真正回滚（v3.0.3 核心回归）----
seed_old; prep_candidate; rm -f "$FAIL_FLAG"
echo always > "$RESTART_MODE"; touch "$RESTART_FLAG"; : > "$RESTART_LOG"
ERR4=$( run_flow 2>&1 >/dev/null ); RC4=$?
ck "restart 失败 → 退出码非 0" 1 "$([ "$RC4" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "config 回滚为旧内容" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "nodes 回滚为旧内容" 0 $?
[[ ! -f "$SB_CONF.candidate" && ! -f "$NODES_JSON.candidate" ]]; ck "候选文件已清理" 0 $?
[[ -f "$SB_CONF.bak" && -f "$NODES_JSON.bak" ]]; ck "失败路径保留备份(供人工恢复)" 0 $?
ck "restart 至少尝试两次(原始+回滚后)" 1 "$([ "$(restart_count)" -ge 2 ] && echo 1 || echo 0)"
printf '%s' "$ERR4" | grep -q "启动失败"; ck "显示 restart 失败错误" 0 $?
printf '%s' "$ERR4" | grep -q "已回滚";  ck "显示回滚提示" 0 $?
printf '%s' "$ERR4" | grep -q "严重:";   ck "双失败场景显示严重告警" 0 $?

# ---- 场景五：restart 瞬时失败 → 回滚后二次重启成功 ----
seed_old; prep_candidate; rm -f "$FAIL_FLAG"
echo once > "$RESTART_MODE"; touch "$RESTART_FLAG"; : > "$RESTART_LOG"
ERR5=$( run_flow 2>&1 >/dev/null ); RC5=$?
ck "瞬时 restart 失败 → 退出码仍非 0" 1 "$([ "$RC5" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "config 回滚为旧内容" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "nodes 回滚为旧内容" 0 $?
ck "恰好重启两次且第二次成功" 2 "$(restart_count)"
[[ ! -e "$RESTART_FLAG" ]]; ck "瞬态故障已被二次重启消化" 0 $?
printf '%s' "$ERR5" | grep -q "旧配置已恢复运行"; ck "显示旧配置恢复运行提示" 0 $?
printf '%s' "$ERR5" | grep -q '\[ok\]'; [[ $? -ne 0 ]]; ck "失败流程不得输出成功标记" 0 $?

# ---- 场景六：config 备份失败 → 立即中止，正式文件不动、不重启 ----
seed_old; prep_candidate; rm -f "$FAIL_FLAG" "$RESTART_FLAG"; : > "$RESTART_LOG"
export CP_BLOCK_SRC="$SB_CONF" CP_BLOCK_DST="$SB_CONF.bak"
ERR6=$( run_flow 2>&1 >/dev/null ); RC6=$?
ck "config 备份失败 → 非零退出" 1 "$([ "$RC6" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "config 保持原内容" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "nodes 保持原内容" 0 $?
[[ -f "$SB_CONF.candidate" && -f "$NODES_JSON.candidate" ]]; ck "候选未提交(mutation 未开始)" 0 $?
ck "未执行任何 restart" 0 "$(restart_count)"
printf '%s' "$ERR6" | grep -q "备份创建失败"; ck "显示备份失败错误" 0 $?

# ---- 场景七：nodes 备份失败 → 行为相同 ----
seed_old; prep_candidate; rm -f "$FAIL_FLAG" "$RESTART_FLAG"; : > "$RESTART_LOG"
export CP_BLOCK_SRC="$NODES_JSON" CP_BLOCK_DST="$NODES_JSON.bak"
ERR7=$( run_flow 2>&1 >/dev/null ); RC7=$?
ck "nodes 备份失败 → 非零退出" 1 "$([ "$RC7" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "config 保持原内容" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "nodes 保持原内容" 0 $?
[[ -f "$SB_CONF.candidate" && -f "$NODES_JSON.candidate" ]]; ck "候选未提交(mutation 未开始)" 0 $?
ck "未执行任何 restart" 0 "$(restart_count)"
printf '%s' "$ERR7" | grep -q "备份创建失败"; ck "显示备份失败错误" 0 $?

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
