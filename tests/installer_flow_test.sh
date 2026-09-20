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
  # err 在真实安装器里存在（输出样式段），桩此前遗漏——补上以保证被测代码
  # 走的是与线上一致的输出路径
  err() { echo "[err] $*" >&2; }
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

# ---- 依赖分级：必需（curl/tar）失败必须中止；可选（jq/openssl）失败只降级 ----
# 真机故障回归：Debian 11（已 EOL）security 源被移除 → apt 装 jq 报 404，
# 早期实现把 curl/tar/openssl/jq 混在一次 pkg_install 里，导致"装个 jq 失败
# 就整个装不下去"，与代码注释里"jq 缺失不阻断安装"的承诺自相矛盾。
#
# 隔离要求：PATH **只含桩目录**。若把 /usr/bin:/bin 放进来，宿主上真实存在的
# jq/tar 会被 command -v 找到，"缺失"场景根本走不到，测试会假通过。
stub_bin() { # stub_bin <目录名> <缺失的工具列表>
  local bin="$TMPD/$1" missing="$2" t
  rm -rf "$bin"; mkdir -p "$bin"
  for t in curl tar openssl jq nft sha256sum; do
    case " $missing " in
      *" $t "*) continue ;;
    esac
    printf '#!/bin/sh\nexit 0\n' > "$bin/$t"; chmod +x "$bin/$t"
  done
  echo "$bin"
}

run_deps_split() { # $1 = 缺失的工具（空格分隔），$2 = pkg_install 是否成功
  set +u
  local missing="$1" installok="$2"
  PATH="$(stub_bin splitbin "$missing")"
  PKG="apt"
  # 桩 pkg_install 必须**真的把文件建出来**：安装后的复检是
  # `command -v <tool>`，只返回 0 而不落盘的桩会让"装得上"场景被判成失败。
  if [[ "$installok" == yes ]]; then
    pkg_install() { local a; for a in "$@"; do printf '#!/bin/sh\nexit 0\n' > "$PATH/$a"; chmod +x "$PATH/$a"; done; return 0; }
  else
    pkg_install() { return 1; }
  fi
  ensure_conntrack_acct() { return 0; }
  info() { :; }; ok() { :; }
  warn() { echo "[warn] $*" >&2; }
  err() { echo "[err] $*" >&2; }
  die() { echo "[die] $*" >&2; exit 1; }
  source "$TMPD/deps.sh"
  install_deps
}

# 只有 jq 缺失且装不上 → 必须放行（降级），并给出原因
OUT=$( run_deps_split "jq" no 2>&1 ); RC=$?
ck "可选依赖 jq 装不上 → 仍继续安装" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"
printf '%s' "$OUT" | grep -q '可选依赖未能安装'; ck "可选依赖失败给出明确告警" 0 $?
printf '%s' "$OUT" | grep -q 'archive.debian.org'; ck "EOL 源被移除时给出 archive 提示" 0 $?

# openssl 缺失且装不上 → 仍有 sha256sum，放行
OUT=$( run_deps_split "openssl" no 2>&1 ); RC=$?
ck "可选依赖 openssl 装不上（有 sha256sum）→ 继续" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"

# 必需依赖 tar 缺失且装不上 → 必须中止
OUT=$( run_deps_split "tar" no 2>&1 ); RC=$?
ck "必需依赖 tar 装不上 → 中止" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q '必需依赖安装失败'; ck "必需依赖失败给出明确错误" 0 $?
printf '%s' "$OUT" | grep -q '404'; ck "必需依赖失败时提示 404/EOL 排查方向" 0 $?

# 必需依赖缺失但装得上 → 继续
OUT=$( run_deps_split "tar" yes 2>&1 ); RC=$?
ck "必需依赖装得上 → 继续" 0 "$([ "$RC" == 0 ] && echo 0 || echo 1)"

# 既无 sha256sum 也无 openssl → fail-closed（无法校验供应链，绝不继续安装）
run_no_sha() {
  set +u
  PATH="$(stub_bin noshabin "openssl sha256sum")"   # 刻意不给 sha256sum 与 openssl
  PKG="apt"; pkg_install() { return 0; }
  ensure_conntrack_acct() { return 0; }
  info() { :; }; ok() { :; }; warn() { echo "[warn] $*" >&2; }
  err() { echo "[err] $*" >&2; }; die() { echo "[die] $*" >&2; exit 1; }
  source "$TMPD/deps.sh"
  install_deps
}
OUT=$( run_no_sha 2>&1 ); RC=$?
ck "无 sha256sum 且无 openssl → fail-closed 中止" 1 "$([ "$RC" != 0 ] && echo 1 || echo 0)"
printf '%s' "$OUT" | grep -q '校验'; ck "缺校验工具时说明原因" 0 $?


echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
