#!/usr/bin/env bash
# commit_flow_test.sh — 从 installer-template.sh 提取真实的 commit_node 流程，
# 用桩依赖验证：config 提交成功而 nodes 提交失败时必须整体回滚且退出非 0。
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

SB_CONF="$TMPD/sing-box/config.json"
NODES_JSON="$TMPD/sbx/nodes.json"
mkdir -p "$(dirname "$SB_CONF")" "$(dirname "$NODES_JSON")"

FAKE_BIN="$TMPD/bin"; mkdir -p "$FAKE_BIN"
FAIL_FLAG="$TMPD/fail-node-commit"

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
chmod +x "$FAKE_BIN"/*

cat >> "$TMPD/flow.sh" <<'STUBS'

warn() { echo "[warn] $*" >&2; }
ok()   { echo "[ok] $*"; }
info() { echo "[info] $*"; }
die()  { echo "[die] $*" >&2; exit 1; }
RESTARTS=0
sb_restart() { RESTARTS=$((RESTARTS+1)); return 0; }
fw_apply() { :; }
panel_running() { return 1; }
svc_do() { :; }
STUBS

export SB_CONF NODES_JSON
export SB_BIN="$FAKE_BIN/sing-box"
export CORE_BIN="$FAKE_BIN/core_node"

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

echo "== commit_flow_test =="

# ---- 场景一：全部成功 ----
seed_old; prep_candidate; rm -f "$FAIL_FLAG"
ERR1=$( run_flow 2>&1 >/dev/null ); RC1=$?
ck "成功流程退出码 0" 0 "$([ "$RC1" == 0 ] && echo 0 || echo 1)"
[[ -z "$(printf '%s' "$ERR1" | grep '\[warn\]')" ]]; ck "成功路径无告警输出" 0 $?
expect_file "$SB_CONF" "$NEW_CONF";   ck "config 已提交为候选内容" 0 $?
expect_file "$NODES_JSON" "$NEW_NODES"; ck "nodes 已提交为候选内容" 0 $?
[[ ! -f "$SB_CONF.bak" && ! -f "$NODES_JSON.bak" ]]; ck "备份文件已清理" 0 $?

# ---- 场景二：config 提交成功但 nodes 提交失败 → 必须回滚 ----
seed_old; prep_candidate; touch "$FAIL_FLAG"
ERR2=$( run_flow 2>&1 >/dev/null ); RC2=$?
ck "nodes 提交失败 → 命令退出码非 0" 1 "$([ "$RC2" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "config 回滚到修改前内容" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "nodes 保持修改前内容" 0 $?
[[ ! -f "$NODES_JSON.candidate" ]]; ck "失败后不残留候选文件" 0 $?
printf '%s' "$ERR2" | grep -q "回滚";           ck "向用户显示明确回滚错误" 0 $?
printf '%s' "$ERR2" | grep -q "simulated nodes commit failure"; ck "底层错误被透出给用户" 0 $?

# ---- 场景三：sing-box check 失败 → 不触碰任何正式文件 ----
seed_old; prep_candidate
printf '#!/usr/bin/env bash\nexit 1\n' > "$FAKE_BIN/sing-box"; chmod +x "$FAKE_BIN/sing-box"
ERR3=$( run_flow 2>&1 >/dev/null ); RC3=$?
ck "check 失败 → 非零退出" 1 "$([ "$RC3" != 0 ] && echo 1 || echo 0)"
expect_file "$SB_CONF" "$OLD_CONF";   ck "check 失败 config 未动" 0 $?
expect_file "$NODES_JSON" "$OLD_NODES"; ck "check 失败 nodes 未动" 0 $?

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
