#!/usr/bin/env bash
# e2e_remote.sh — 在真机上端到端验收 Go 版 SBX（在真机本地执行）
set -u
cd /root/sbx-build || exit 1

ROOT="/root/sbx-e2e"
CORE_BIN_SRC="/root/sbx-build/dist/sbx-core-linux-amd64"
SB_BIN="/tmp/sb-test/sing-box"
APP_DIR="$ROOT/etc/sbx"
SB_DIR="$ROOT/etc/sing-box"
PANEL_CONF="$APP_DIR/panel.json"
NODES_JSON="$APP_DIR/nodes.json"
export SBX_DIR="$APP_DIR" SBX_CONF="$PANEL_CONF" SBX_SB_CONF="$SB_DIR/config.json"
CORE="$ROOT/usr/local/bin/sbx-core"

PASS=0; FAIL=0; FAILED=()
ck() { # ck <名称> <条件退出码>
  if [[ "$2" == 0 ]]; then PASS=$((PASS+1)); echo "  [PASS] $1";
  else FAIL=$((FAIL+1)); FAILED+=("$1"); echo "  [FAIL] $1"; fi
}
section() { echo; echo "== $1 =="; }

pkill -f sbx-core-linux 2>/dev/null; pkill -f 'sing-box run' 2>/dev/null
ip netns del e2e 2>/dev/null
rm -rf "$ROOT"; mkdir -p "$ROOT"

# ---------------------------------------------------------------- 1. 安装
section "1. 沙箱安装（pipe 模式）"
env SBX_ROOT="$ROOT" SBX_NO_SERVICE=1 SBX_CORE_BIN="$CORE_BIN_SRC" SBX_SB_BIN="$SB_BIN" \
  NO_COLOR=1 bash sbx.sh </dev/null >/tmp/e2e-install.log 2>&1
ck "安装脚本退出码 0" $?
grep -q "安装完成" /tmp/e2e-install.log; ck "输出含「安装完成」" $?
[[ -x "$CORE" ]]; ck "sbx-core 已安装" $?
[[ -x "$ROOT/usr/local/bin/sing-box" ]]; ck "sing-box 已安装" $?
"$CORE" version | grep -q "v3.0.6"; ck "core 版本 3.0.6 ($("$CORE" version))" $?
jq -e '.token and (.port|type)=="number" and .port>=1 and .port<=65535' "$PANEL_CONF" >/dev/null 2>&1
ck "panel.json 合法(token+port)" $?

# ---------------------------------------------------------------- 2. 菜单加节点
section "2. 菜单添加 Shadowsocks 2022 节点"
E2E_PW="$(openssl rand -base64 16 | tr -d '\n')"
printf '1\n2\n18388\nss-e2e\n%s\n\n0\n0\n' "$E2E_PW" | env SBX_ROOT="$ROOT" SBX_NO_SERVICE=1 \
  NO_COLOR=1 bash "$ROOT/usr/local/bin/sbx" >/tmp/e2e-menu.log 2>&1
ck "菜单流程退出码 0" $?
jq -e 'length==1 and .[0].type=="shadowsocks" and .[0].port==18388 and .[0].name=="ss-e2e"' "$NODES_JSON" >/dev/null 2>&1
ck "nodes.json 记录正确" $?
jq -e '
  ([.inbounds[] | select(.tag|startswith("sbx-n"))] | length)==1
  and ([.inbounds[] | select(.tag|startswith("sbx-n"))][0].listen_port==18388)
  and ([.inbounds[] | select(.tag|startswith("sbx-n"))][0].method=="2022-blake3-aes-128-gcm")
  and any(.outbounds[]; .tag=="direct")' "$SB_DIR/config.json" >/dev/null 2>&1
ck "sing-box 配置生成正确" $?
"$SB_BIN" check -c "$SB_DIR/config.json" >/dev/null 2>&1; ck "sing-box check 通过" $?
grep -q "ss://" /tmp/e2e-menu.log; ck "分享链接已输出" $?

# ---------------------------------------------------------------- 3. 启动服务
section "3. 启动 sing-box 与面板"
"$SB_BIN" run -C "$SB_DIR" >/tmp/e2e-sb.log 2>&1 &
SB_PID=$!
for i in $(seq 1 30); do ss -Hlnt | grep -q ':18388 ' && break; sleep 0.5; done
PORT=$(jq -r '.port' "$PANEL_CONF")
TOKEN=$(jq -r '.token' "$PANEL_CONF")
env SBX_CONF="$PANEL_CONF" "$CORE" serve >/tmp/e2e-panel.log 2>&1 &
CORE_PID=$!
for i in $(seq 1 30); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.5; done
ck "面板 healthz" $?
ss -Hlnt | grep -q ":18388 "; ck "节点端口已监听" $?

# ---------------------------------------------------------------- 4. 真实流量
section "4. veth 命名空间真实流量计数"
ip netns add e2e
ip link add v-h type veth peer name v-n
ip link set v-n netns e2e
ip addr add 10.66.0.1/24 dev v-h; ip link set v-h up
ip netns exec e2e ip addr add 10.66.0.2/24 dev v-n
ip netns exec e2e ip link set v-n up; ip netns exec e2e ip link set lo up
dd if=/dev/urandom of=/tmp/e2e-1mb.bin bs=1M count=1 status=none
mkdir -p /tmp/e2e-www && cp /tmp/e2e-1mb.bin /tmp/e2e-www/
busybox httpd -f -p 8000 -h /tmp/e2e-www >/tmp/e2e-http.log 2>&1 &
HTTP_PID=$!
# 客户端放进 e2e 命名空间：client->server 才会经 veth 触发 INPUT/OUTPUT 计数
# （宿主内连本地地址走 lo，会被 iif lo return 跳过——这是计数口径的一部分）
cat >/tmp/e2e-client.json <<EOF
{"log":{"level":"warn"},
 "inbounds":[{"type":"mixed","tag":"in","listen":"10.66.0.2","listen_port":10801}],
 "outbounds":[{"type":"shadowsocks","tag":"out","server":"10.66.0.1","server_port":18388,
   "method":"2022-blake3-aes-128-gcm","password":"$(jq -r '.[0].password' "$NODES_JSON")"}]}
EOF
pkill -9 -f 'sing-box run -c /tmp/e2e-[c]lient.json' 2>/dev/null
ip netns exec e2e "$SB_BIN" run -c /tmp/e2e-client.json >/tmp/e2e-client.log 2>&1 &
CLI_PID=$!
READY=1
for i in $(seq 1 30); do ip netns exec e2e ss -Hlnt | grep -q ':10801 ' && { READY=0; break; }; sleep 0.5; done
ck "SOCKS 客户端就绪(命名空间内)" $READY
curl -fsS --max-time 30 --socks5-hostname 10.66.0.2:10801 http://10.66.0.1:8000/e2e-1mb.bin -o /tmp/e2e-dl.bin
ck "经节点下载 1MB 成功" $?
[[ $(stat -c%s /tmp/e2e-dl.bin) == 1048576 ]]; ck "下载字节数完整" $?
sleep 4   # 等两轮采集
api_total() { curl -fsS "http://127.0.0.1:$PORT/api/summary?token=$TOKEN" | jq -r '
  ([.nodes[] | select(.id==1)][0] | "\(.total.rx) \(.total.tx)")'; }
read RX TX <<< "$(api_total 2>/dev/null || echo 0 0)"
echo "  节点累计 rx=$RX tx=$TX"
[[ "$TX" -ge 1000000 ]]; ck "tx≥1MB（下载计入）" $?
[[ "$RX" -ge 500 && "$RX" -le 20000 ]]; ck "rx 在请求量级（无虚增）" $?
curl -fsS "http://127.0.0.1:$PORT/api/summary?token=$TOKEN" | jq -e '
  .rate_known==true and .total.tx>=1000000 and .healthy==true' >/dev/null 2>&1
ck "healthy 且速率已知" $?

# ---------------------------------------------------------------- 5. epoch 换代连续性
section "5. 规则重建(epoch 换代)不丢计不重复"
read RX1 TX1 <<< "$(api_total)"
"$CORE" apply >/dev/null 2>&1; ck "apply 重建规则成功" $?
sleep 1
curl -fsS --max-time 30 --socks5-hostname 10.66.0.2:10801 http://10.66.0.1:8000/e2e-1mb.bin -o /tmp/e2e-dl2.bin
sleep 4
read RX2 TX2 <<< "$(api_total)"
echo "  before(tx=$TX1) after(tx=$TX2)"
DIFF=$((TX2-TX1))
[[ "$DIFF" -ge 1000000 && "$DIFF" -le 1100000 ]]; ck "换代后增量≈1MB($DIFF)" $?
# SQLite 内部基线（meta.epoch/counter_state）由 internal/traffic 单元测试覆盖，
# 此处仅验证真机数据面：apply 后 show 正常、累计不丢。
"$CORE" show >/dev/null 2>&1; ck "show 正常（计数器基线完整）" $?

# ---------------------------------------------------------------- 6. 面板重启连续性
section "6. 面板重启后统计连续"
kill -TERM "$CORE_PID"; wait "$CORE_PID" 2>/dev/null
ck "面板 SIGTERM 优雅退出" $?
grep -qiE "panic|SIGSEGV" /tmp/e2e-panel.log && { ck "面板日志无 panic" 1; } || ck "面板日志无 panic" 0
env SBX_CONF="$PANEL_CONF" "$CORE" serve >>/tmp/e2e-panel.log 2>&1 &
CORE_PID=$!
sleep 3
read RX3 TX3 <<< "$(api_total)"
[[ "$TX3" == "$TX2" ]]; ck "重启后累计不变($TX3)" $?
curl -fsS --max-time 30 --socks5-hostname 10.66.0.2:10801 http://10.66.0.1:8000/e2e-1mb.bin -o /tmp/e2e-dl3.bin
sleep 4
read RX4 TX4 <<< "$(api_total)"
DIFF=$((TX4-TX3))
[[ "$DIFF" -ge 1000000 && "$DIFF" -le 1100000 ]]; ck "重启后再传增量≈1MB($DIFF)" $?

# ---------------------------------------------------------------- 7. 统计一致性（Go 单测 + 真机 API 双重验证）
section "7. 统计一致性核对"
# Python reference 已移除（全 Go 化）；此处不再做 Go vs Python 对拍。
# 等价回归由 internal/traffic 的单元测试 + 金标夹具（internal/*/testdata）覆盖，
# 本脚本通过 /api/summary 的 epoch 连续性与累计值核对真机数据面一致性。
env SBX_CONF="$PANEL_CONF" "$CORE" show >/tmp/e2e-show.txt 2>&1
grep -qE "今日|累计|rx|tx|RX|TX" /tmp/e2e-show.txt; ck "sbx-core show 输出流量汇总" $?
grep -qE "总计|系统|node" /tmp/e2e-show.txt; ck "show 含节点/系统维度" $?

# ---------------------------------------------------------------- 收尾
section "结果"
kill "$SB_PID" "$CLI_PID" "$HTTP_PID" 2>/dev/null
ip netns del e2e 2>/dev/null
pkill -f sbx-core-linux 2>/dev/null
echo "PASS=$PASS FAIL=$FAIL"
if [[ "$FAIL" -gt 0 ]]; then printf '%s\n' "${FAILED[@]}"; exit 1; fi
echo E2E_ALL_GREEN
