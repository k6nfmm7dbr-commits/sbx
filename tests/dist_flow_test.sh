#!/usr/bin/env bash
# dist_flow_test.sh — 验证安装器二进制分发链与敏感文件权限：
#   1. 安装器构造的下载地址指向 dist 分支（不依赖 Git Tag / 版本 Release）
#   2. 架构映射 uname -m -> 文件名与 dist 分支实际产物一一对应
#   3. prepare_dirs 首次创建 nodes.json / state.json 权限为 0600
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$ROOT/installer-template.sh"
PASS=0; FAIL=0
ck() { if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 $2 实得 $3)"; fi; }

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

echo "== dist_flow_test =="

# ---- 1. 下载基址：必须是 dist 分支 raw 地址，且不得含版本化 release 路径 ----
BASE_LINE=$(grep -m1 '^RAW_BASE=' "$TPL")
ck "安装器定义 RAW_BASE 下载基址" 0 "$([ -n "$BASE_LINE" ] && echo 0 || echo 1)"
printf '%s\n' "$BASE_LINE" | grep -q '/dist'; B=$?; [[ $B -ne 0 ]] && B=1 || B=0
ck "下载基址指向 dist 分支" 0 "$B"
# 只检查本仓库的下载路径；sing-box 上游的 releases/latest API 属其官方发布渠道，不算残留
grep 'github.com/k6nfmm7dbr-commits/sbx/releases' "$TPL" | grep -vc '^\s*#' | grep -qx '0'; ck "本仓库无任何 Release 版本化下载路径残留" 0 $?
printf '%s\n' "$BASE_LINE" | grep -q 'v\${APP_VERSION}'; [[ $? -ne 0 ]]; ck "下载 URL 不拼接版本 Tag" 0 $?

DL_URL=$(printf '%s\n' "$BASE_LINE" | sed 's/^RAW_BASE="\${SBX_RAW_BASE:-\(.*\)}"/\1/')
ck "下载基址为 raw.githubusercontent 固定地址" 0 "$(printf '%s' "$DL_URL" | grep -q '^https://raw.githubusercontent.com/' && echo 0 || echo 1)"

# ---- 2. 架构映射与 dist 分支产物清单一致（本地仓库清单为准） ----
# 安装器 sb_arch 的映射
MAPPED=$(sed -n '/^sb_arch()/,/^}/p' "$TPL" | grep -o 'sbx-core-linux' | head -1)
sed -n '/^sb_arch()/,/^}/p' "$TPL" > "$TMPD/arch.sh"
# 从映射函数提取 case 分支 -> 架构名
ARCHS=$(sed -n '/^sb_arch()/,/^}/p' "$TPL" | grep -oE '(amd64|arm64|armv7|armv6|386|s390x|riscv64)' | sort -u)
ck "安装器支持 7 个架构" 7 "$(echo "$ARCHS" | wc -l | tr -d ' ')"
for a in $ARCHS; do
  [[ -f "$ROOT/scripts/dist-manifest.txt" ]] && EXPECTED="$ROOT/scripts/dist-manifest.txt" || EXPECTED=""
  if [[ -n "$EXPECTED" ]]; then
    grep -qx "sbx-core-linux-$a" "$EXPECTED"; ck "产物 sbx-core-linux-$a 在分发清单中" 0 $?
  else
    # 无清单文件时跳过（清单由 CI 生成）
    :
  fi
done
[[ -f "$ROOT/scripts/dist-manifest.txt" ]]; ck "分发清单 dist-manifest.txt 存在" 0 $?
[[ -f "$ROOT/scripts/dist-manifest.txt" ]] && { grep -q '^sbx-core-linux-amd64$' "$ROOT/scripts/dist-manifest.txt"; ck "清单含 amd64 产物" 0 $?; grep -q '^sbx-core-linux-arm64$' "$ROOT/scripts/dist-manifest.txt"; ck "清单含 arm64 产物" 0 $?; grep -q '^SHA256SUMS$' "$ROOT/scripts/dist-manifest.txt"; ck "清单含 SHA256SUMS" 0 $?; }

# ---- 3. 安装器下载函数确实使用 RAW_BASE + 架构名 + SHA256SUMS ----
grep -q '\$RAW_BASE/\${name}' "$TPL"; ck "binary 下载使用 RAW_BASE/架构文件名" 0 $?
grep -q '\$RAW_BASE/SHA256SUMS' "$TPL"; ck "校验文件下载使用 RAW_BASE/SHA256SUMS" 0 $?

# ---- 3b. rolling 分发：不得因「已装版本号==APP_VERSION」跳过下载（防 stale 二进制）----
sed -n '/^install_sbx_core()/,/^}/p' "$TPL" > "$TMPD/isc.sh"
grep -q 'core_version_of "\$CORE_BIN"' "$TMPD/isc.sh"; [[ $? -ne 0 ]]; ck "install_sbx_core 不再按已装版本号跳过下载" 0 $?
grep -q '代码内容' "$TMPD/isc.sh"; ck "保留按内容判断的说明注释" 0 $?
# 内容比较：先取 SHA256SUMS 与本地二进制 sha256 对比，一致才跳过
grep -q 'sha256_of "\$CORE_BIN"' "$TMPD/isc.sh"; ck "install_sbx_core 按二进制内容(sha256)判断是否跳过" 0 $?
grep -q 'expect_sum' "$TMPD/isc.sh"; ck "install_sbx_core 提取 SHA256SUMS 期望哈希" 0 $?
grep -q 'CORE_REPLACED=1' "$TMPD/isc.sh"; ck "install_sbx_core 替换成功后置 CORE_REPLACED 信号" 0 $?

# ---- 3c. do_update 脚本相同时也检查二进制内容（不只靠脚本/版本号）----
sed -n '/^do_update()/,/^apply_update()/p' "$TPL" | sed '$d' > "$TMPD/du.sh"
grep -q '检查后端 sbx-core 是否同步' "$TMPD/du.sh"; ck "do_update 脚本相同时也检查二进制" 0 $?
grep -q 'install_sbx_core' "$TMPD/du.sh"; ck "do_update 脚本相同分支调用 install_sbx_core" 0 $?
grep -q 'CORE_REPLACED' "$TMPD/du.sh"; ck "do_update 依据 CORE_REPLACED 判断是否更新了后端" 0 $?

# ---- 4. prepare_dirs：0600 权限创建（提取真实实现） ----
# 提取 prepare_dirs 到下一个函数（函数体无 heredoc，但统一用边界法）
NEXT_FN=$(awk '/^prepare_dirs\(\)/{f=1;next} f&&/^[a-z_]+\(\)\s*\{/{print $1;exit}' "$TPL" | tr -d ' {')
[[ -n "$NEXT_FN" ]] || NEXT_FN="ensure_sb_config"
sed -n "/^prepare_dirs()/,/^${NEXT_FN}()/p" "$TPL" | sed '$d' > "$TMPD/pd.sh"
grep -q 'install -m 0600' "$TMPD/pd.sh"; ck "prepare_dirs 使用 install -m 0600 预创建" 0 $?
grep -q 'NODES_JSON' "$TMPD/pd.sh" && grep -q 'STATE_JSON' "$TMPD/pd.sh"; ck "两个敏感文件均走 0600 路径" 0 $?

# 实际执行验证权限
APPD="$TMPD/app/sbx"; SBDD="$TMPD/app/sing-box"; WEBD="$TMPD/app/web"; CERTD="$TMPD/app/certs"
NODES_JSON="$APPD/nodes.json"; STATE_JSON="$APPD/state.json"
export NODES_JSON STATE_JSON
cat >> "$TMPD/pd.sh" <<STUBS
APP_DIR="$APPD"; WEB_DIR="$WEBD"; SB_DIR="$SBDD"; CERT_DIR="$CERTD"
STUBS
( set +u; source "$TMPD/pd.sh"; prepare_dirs )
[[ -f "$NODES_JSON" && "$(cat "$NODES_JSON")" == "[]" ]]; ck "nodes.json 初始内容为 []" 0 $?
[[ -f "$STATE_JSON" && "$(cat "$STATE_JSON")" == "{}" ]]; ck "state.json 初始内容为 {}" 0 $?
[[ "$(stat -c %a "$NODES_JSON" 2>/dev/null)" == "600" ]]; ck "nodes.json 权限为 0600" 0 $?
[[ "$(stat -c %a "$STATE_JSON" 2>/dev/null)" == "600" ]]; ck "state.json 权限为 0600" 0 $?

# umask 0777 极端环境下仍为 0600
rm -f "$NODES_JSON" "$STATE_JSON"
( set +u; umask 0777; source "$TMPD/pd.sh"; prepare_dirs )
[[ "$(stat -c %a "$NODES_JSON" 2>/dev/null)" == "600" ]]; ck "umask 0777 下 nodes.json 仍为 0600" 0 $?
[[ "$(stat -c %a "$STATE_JSON" 2>/dev/null)" == "600" ]]; ck "umask 0777 下 state.json 仍为 0600" 0 $?

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]] || exit 1
