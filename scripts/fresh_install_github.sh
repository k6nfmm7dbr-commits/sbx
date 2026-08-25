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

/usr/local/bin/sbx-core version | grep -q "v3.0.4"; ck "core 版本 v3.0.4" $?
systemctl is-active --quiet sbx-panel; ck "面板服务 active" $?
systemctl is-active --quiet sing-box; ck "sing-box active" $?
PORT=$(python3 -c "import json;print(json.load(open('/etc/sbx/panel.json'))['port'])")
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"ok":true'; ck "API 健康" $?

echo "== 菜单添加节点（真实用户路径） =="
printf '1\n2\n28388\ngh-e2e\n\n\n0\n0\n' | env NO_COLOR=1 bash /usr/local/bin/sbx >/tmp/gh-menu.log 2>&1
ck "菜单加节点退出码 0" $?
python3 -c "
import json
n=json.load(open('/etc/sbx/nodes.json'))
assert len(n)==1 and int(n[0]['port'])==28388, n"; ck "节点落库正确" $?

sleep 3
curl -fsS --max-time 10 "http://127.0.0.1:$PORT/api/live?token=$(python3 -c "import json;print(json.load(open('/etc/sbx/panel.json'))['token'])")" | python3 -c "
import json,sys
d=json.load(sys.stdin)
assert d['healthy'] is True, d
assert any(x['id']==1 for x in d['nodes']), d
print('live ok, nodes:', [x['id'] for x in d['nodes']], 'healthy:', d['healthy'])"; ck "/api/live healthy 且含节点" $?

echo "== 清理卸载 =="
echo y | env NO_COLOR=1 bash /usr/local/bin/sbx --uninstall >/tmp/gh-uninstall.log 2>&1
ck "卸载退出码 0" $?
[[ ! -d /etc/sbx ]]; ck "目录清除" $?

echo "== 结果 =="
echo "PASS=$PASS FAIL=$FAIL"
[[ "$FAIL" -gt 0 ]] && { printf '%s\n' "${FAILED[@]}"; exit 1; }
echo GH_FRESH_INSTALL_ALL_GREEN
