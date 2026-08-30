#!/usr/bin/env bash
# fresh_install_github.sh — 模拟全新用户从 GitHub 一键安装（发布链路终验）
set -u
PASS=0; FAIL=0; FAILED=()
ck() { if [[ "$2" == 0 ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); FAILED+=("$1"); echo "  [FAIL] $1"; fi; }

echo "== 从 raw URL 一键安装（无任何本地 override） =="
env NO_COLOR=1 bash <(curl -fsSL "https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh?v=$(date +%s)") </dev/null >/tmp/gh-install.log 2>&1
ck "一键安装退出码 0" $?
grep -q "安装完成" /tmp/gh-install.log; ck "输出含「安装完成」" $?
grep -q "sbx-core 安装完成" /tmp/gh-install.log && grep -qE "下载 sbx-core|已安装" /tmp/gh-install.log; ck "sbx-core 走 Releases 下载路径" $?

/usr/local/bin/sbx-core version | grep -q "v3.0.7"; ck "core 版本 v3.0.7" $?
systemctl is-active --quiet sbx-panel; ck "面板服务 active" $?
systemctl is-active --quiet sing-box; ck "sing-box active" $?
PORT=$(jq -r '.port' /etc/sbx/panel.json)
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"ok":true'; ck "API 健康" $?

echo "== 菜单添加节点（真实用户路径） =="
printf '1\n2\n28388\ngh-e2e\n\n\n0\n0\n' | env NO_COLOR=1 bash /usr/local/bin/sbx >/tmp/gh-menu.log 2>&1
ck "菜单加节点退出码 0" $?
jq -e 'length==1 and .[0].port==28388' /etc/sbx/nodes.json >/dev/null 2>&1
ck "节点落库正确" $?

sleep 3
TOKEN=$(jq -r '.token' /etc/sbx/panel.json)
curl -fsS --max-time 10 -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/live" | jq -e '
  .healthy==true and any(.nodes[]; .id==1)' >/dev/null 2>&1
ck "/api/live healthy 且含节点" $?

echo "== 清理卸载 =="
echo y | env NO_COLOR=1 bash /usr/local/bin/sbx --uninstall >/tmp/gh-uninstall.log 2>&1
ck "卸载退出码 0" $?
# v3.0.7：卸载须清理策略表与 sysctl 持久化文件
! nft list tables 2>/dev/null | grep -q 'inet sbx_'; ck "卸载后无 sbx_* nft 表残留" $?
[[ ! -f /etc/sysctl.d/99-sbx-conntrack.conf ]]; ck "卸载后无 sysctl 残留" $?
[[ ! -d /etc/sbx ]]; ck "目录清除" $?

echo "== 结果 =="
echo "PASS=$PASS FAIL=$FAIL"
[[ "$FAIL" -gt 0 ]] && { printf '%s\n' "${FAILED[@]}"; exit 1; }
echo GH_FRESH_INSTALL_ALL_GREEN
