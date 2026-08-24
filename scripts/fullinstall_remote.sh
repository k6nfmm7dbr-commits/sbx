#!/usr/bin/env bash
# fullinstall_remote.sh — 完整真实安装/卸载验收（systemd）
set -u
PASS=0; FAIL=0; FAILED=()
ck() { if [[ "$2" == 0 ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); FAILED+=("$1"); echo "  [FAIL] $1"; fi; }
section() { echo; echo "== $1 =="; }

export SBX_CORE_BIN=/root/sbx-build/dist/sbx-core-linux-amd64
export SBX_SB_BIN=/tmp/sb-test/sing-box
pkill -9 -f 'sbx-[c]ore serve' 2>/dev/null
pkill -9 -f 'sing-[b]ox' 2>/dev/null

section "1. 完整安装（真实路径 + systemd）"
env NO_COLOR=1 bash /root/sbx-build/sbx.sh </dev/null >/tmp/full-install.log 2>&1
ck "安装退出码 0" $?
grep -q "安装完成" /tmp/full-install.log; ck "输出含「安装完成」" $?

section "2. systemd 单元"
for u in sing-box sbx-firewall sbx-panel; do
  [[ -f /etc/systemd/system/$u.service ]]; ck "单元文件存在: $u" $?
done
sleep 2
systemctl is-active --quiet sing-box; ck "sing-box active" $?
systemctl is-active --quiet sbx-panel; ck "sbx-panel active" $?
systemctl is-enabled --quiet sbx-panel >/dev/null 2>&1 || systemctl is-enabled --quiet sbx-panel; ck "sbx-panel 开机自启" $?
FP=$(python3 -c "import json;print(json.load(open('/etc/sbx/panel.json'))['port'])")
ss -Hlnt | grep -q ":$FP "; ck "面板端口($FP)监听中" $?

section "3. 命令行冒烟"
PORT=$(python3 -c "import json;print(json.load(open('/etc/sbx/panel.json'))['port'])")
TOKEN=$(python3 -c "import json;print(json.load(open('/etc/sbx/panel.json'))['token'])")
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"ok":true'; ck "面板 API 健康" $?
sbx --version | grep -q "3.0.0"; ck "sbx --version" $?
sbx --show >/dev/null 2>&1; ck "sbx --show" $?
sbx --links >/dev/null 2>&1; ck "sbx --links" $?
[[ "$(sbx-core node count)" == "0" ]] || [[ "$(sbx-core node count)" == "" ]]; ck "node count(全新无节点)" $?
journalctl -u sbx-panel -n 5 --no-pager | grep -q "面板已启动"; ck "journald 有启动日志" $?

section "4. 计数规则内核态"
nft list table inet sbx_traffic >/dev/null 2>&1; ck "nft 表存在" $?
nft -j list counters table inet sbx_traffic | grep -q sbx_sys_i; ck "系统计数器存在" $?

section "5. 升级演练(--apply-update 幂等)"
env NO_COLOR=1 bash /root/sbx-build/sbx.sh --apply-update >/tmp/full-update.log 2>&1
ck "apply-update 退出码 0" $?
systemctl is-active --quiet sbx-panel; ck "升级后面板仍 active" $?

section "6. 干净卸载"
echo y | env NO_COLOR=1 bash /root/sbx-build/sbx.sh --uninstall >/tmp/full-uninstall.log 2>&1
ck "卸载退出码 0" $?
[[ ! -d /etc/sbx ]]; ck "/etc/sbx 已删除" $?
[[ ! -f /usr/local/bin/sbx-core ]]; ck "sbx-core 已删除" $?
[[ ! -f /etc/systemd/system/sbx-panel.service ]]; ck "单元已删除" $?
CLEAN=1
for i in $(seq 1 10); do
  nft list table inet sbx_traffic >/dev/null 2>&1 || { CLEAN=0; break; }
  nft delete table inet sbx_traffic 2>/dev/null || true
  sleep 0.5
done
ck "nft 表已清除" $CLEAN

section "结果"
echo "PASS=$PASS FAIL=$FAIL"
[[ "$FAIL" -gt 0 ]] && { printf '%s\n' "${FAILED[@]}"; exit 1; }
echo FULLINSTALL_ALL_GREEN
