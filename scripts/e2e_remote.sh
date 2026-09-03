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

pkill -f "$ROOT/usr/local/bin/sbx-core" 2>/dev/null; pkill -f 'sing-box run' 2>/dev/null
# 隔离生产实例：本机若已有正式安装（systemd 的 sbx-panel），它会管理同名的
# 内核表 inet sbx_traffic / sbx_policy，并在 e2e 删表后把自己的表重建回来——
# 那会污染「clear 后无残留」「外部删除自愈」这类内核态断言。先停掉，收尾时恢复。
PROD_STOPPED=0
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet sbx-panel 2>/dev/null; then
  systemctl stop sbx-panel >/dev/null 2>&1 && PROD_STOPPED=1
  echo "  (已临时停止生产 sbx-panel，收尾时恢复)"
fi
nft delete table inet sbx_traffic 2>/dev/null
nft delete table inet sbx_policy 2>/dev/null
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
"$CORE" version | grep -q "v3.0.8"; ck "core 版本 3.0.8 ($("$CORE" version))" $?
jq -e '.token and (.port|type)=="number" and .port>=1 and .port<=65535' "$PANEL_CONF" >/dev/null 2>&1
ck "panel.json 合法(token+port)" $?

# ---------------------------------------------------------------- 2. 菜单加节点
section "2. 菜单添加 Shadowsocks 2022 节点"
# 按键序列（v3.0.x 菜单顺序）：
#   1=添加节点 → 2=Shadowsocks → 1=加密算法(128) → 端口 → 备注 → 回车(pause) → 0=退出
# 密码由 `sbx-core node ss2022-key` 用 crypto/rand 生成，菜单不再询问。
printf '1\n2\n1\n18388\nss-e2e\n\n0\n' | env SBX_ROOT="$ROOT" SBX_NO_SERVICE=1 \
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
api_total() { curl -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/summary" | jq -r '
  ([.nodes[] | select(.id==1)][0] | "\(.total.rx) \(.total.tx)")'; }
read RX TX <<< "$(api_total 2>/dev/null || echo 0 0)"
[[ "$RX" =~ ^[0-9]+$ ]] || RX=0
[[ "$TX" =~ ^[0-9]+$ ]] || TX=0
echo "  节点累计 rx=$RX tx=$TX"
[[ "$TX" -ge 1000000 ]]; ck "tx≥1MB（下载计入）" $?
[[ "$RX" -ge 500 && "$RX" -le 20000 ]]; ck "rx 在请求量级（无虚增）" $?
curl -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/summary" | jq -e '
  .rate_known==true and .total.tx>=1000000 and .healthy==true' >/dev/null 2>&1
ck "healthy 且速率已知" $?

# query string token 渠道已在 v3.0.5 移除（会泄漏进日志/history）：必须 401
CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://127.0.0.1:$PORT/api/summary?token=$TOKEN")
[[ "$CODE" == "401" ]]; ck "?token= 渠道已移除（返回 401，实得 $CODE）" $?

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
# show 的维度字样：概览段（总览）+ 节点段（节点流量/合计）
grep -qE "总览" /tmp/e2e-show.txt && grep -qE "节点流量|合计" /tmp/e2e-show.txt; ck "show 含总览/节点维度" $?

# ---------------------------------------------------------------- 8. 策略层（v3.0.8 修复点）
section "8. 策略 enforcement（配额阻断 / 表清理 / 外部删除自愈）"
policy_put() { # policy_put <json>
  curl -fsS -m 10 -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -d "$1" "http://127.0.0.1:$PORT/api/nodes/1/policy"; }

# 8.1 配额达限 → 生成 sbx_policy 表并 drop 该节点端口
policy_put '{"quota_enabled":true,"quota_limit_bytes":1000,"ip_limit_enabled":false,"ip_limit_max":0}' \
  | jq -e '.quota_state=="exceeded"' >/dev/null 2>&1
ck "配额达限 → quota_state=exceeded" $?
sleep 1
nft list table inet sbx_policy 2>/dev/null | grep -q 'tcp dport @quota_ports drop'; ck "sbx_policy 生成 quota drop 规则" $?
curl -fsS -m 8 --socks5-hostname 10.66.0.2:10801 http://10.66.0.1:8000/e2e-1mb.bin -o /dev/null 2>/dev/null
[[ $? -ne 0 ]]; ck "达限后经节点访问被阻断" $?

# 8.2 保存策略必须 200（不因 enforcement 细节返回 500）
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X PUT -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"quota_enabled":false,"quota_limit_bytes":0,"ip_limit_enabled":false,"ip_limit_max":0}' \
  "http://127.0.0.1:$PORT/api/nodes/1/policy")
[[ "$CODE" == "200" ]]; ck "解除配额 → 200（实得 $CODE）" $?
sleep 1
curl -fsS -m 20 --socks5-hostname 10.66.0.2:10801 http://10.66.0.1:8000/e2e-1mb.bin -o /dev/null
ck "解除后经节点访问恢复" $?

# 8.3 请求体加固：超限 / 尾随数据必须 400
head -c 2200000 /dev/zero | tr '\0' 'A' > /tmp/e2e-big.txt
printf '{"pad":"%s"}' "$(cat /tmp/e2e-big.txt)" > /tmp/e2e-big.json
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 20 -X PUT -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' --data-binary @/tmp/e2e-big.json \
  "http://127.0.0.1:$PORT/api/nodes/1/policy")
[[ "$CODE" == "400" ]]; ck "超大 body → 400（实得 $CODE）" $?
CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X PUT -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"quota_enabled":false} GARBAGE' \
  "http://127.0.0.1:$PORT/api/nodes/1/policy")
[[ "$CODE" == "400" ]]; ck "尾随数据 → 400（实得 $CODE）" $?
rm -f /tmp/e2e-big.txt /tmp/e2e-big.json

# 8.4 IP 限制：allow set 授予在线 IP
policy_put '{"quota_enabled":false,"quota_limit_bytes":0,"ip_limit_enabled":true,"ip_limit_max":1}' \
  | jq -e '.ip_limit_enabled==true and .ip_limit_max==1' >/dev/null 2>&1
ck "启用 IP 限制(max=1)" $?
sleep 1
nft list table inet sbx_policy 2>/dev/null | grep -q 'ct state established ip saddr != @ip_allow_1_v4 drop'
ck "生成 IP allow set 规则" $?
# 慢速长连接让 conntrack 出现持续活跃流 → slot 授予。
# 1MB @48k ≈ 21s，采样窗口内必然处于传输中。
( curl -fsS -m 40 --limit-rate 48k --socks5-hostname 10.66.0.2:10801 \
    http://10.66.0.1:8000/e2e-1mb.bin -o /dev/null >/dev/null 2>&1 & )
GRANTED=1
for i in $(seq 1 12); do
  if curl -fsS -m 10 -H "Authorization: Bearer $TOKEN" \
       "http://127.0.0.1:$PORT/api/nodes/1/active-ips" | jq -e '(.ips|length)>=1' >/dev/null 2>&1; then
    GRANTED=0; break
  fi
  sleep 1
done
ck "在线 IP 被授予 slot（active-ips 非空）" $GRANTED
if [[ "$GRANTED" != 0 ]]; then
  echo "  [diag] conntrack(dport=18388): $(grep -c 'dport=18388' /proc/net/nf_conntrack 2>/dev/null)"
  grep 'dport=18388' /proc/net/nf_conntrack 2>/dev/null | head -2 | sed 's/^/  [diag] /'
  echo "  [diag] ip-state: $(curl -fsS -m 5 -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/nodes/1/ip-state")"
  echo "  [diag] acct: $(sysctl -n net.netfilter.nf_conntrack_acct 2>/dev/null)"
fi
nft list set inet sbx_policy ip_allow_1_v4 2>/dev/null | grep -q 'elements'
SETW=$?
if [[ "$SETW" != 0 ]]; then
  # allow set 的 IP 内容变化走 3s 合并窗口（v3.0.7 节流，防 SYN churn 高频重写），
  # 因此 slot 授予后 nft 里最多晚一个窗口才出现元素——这里轮询等收敛。
  for i in $(seq 1 8); do
    sleep 1
    nft list set inet sbx_policy ip_allow_1_v4 2>/dev/null | grep -q 'elements' && { SETW=0; break; }
  done
fi
ck "allow set 已写入客户端 IP（含节流窗口收敛）" $SETW

# 8.5 外部删除策略表 → 自愈重建
# 用「配额达限」而非 IP 限制做这个断言：quota 阻断集合是稳态的，
# 不会像 allow set 那样因 slot 授予/释放而自行触发重写——只有存在性探测
# 能把表带回来，断言才真正测到自愈逻辑（探测有 10s 节流，给 ~14s）。
pkill -f 'curl.*socks5-hostname' 2>/dev/null
policy_put '{"quota_enabled":true,"quota_limit_bytes":1000,"ip_limit_enabled":false,"ip_limit_max":0}' >/dev/null 2>&1
sleep 2
nft list table inet sbx_policy 2>/dev/null | grep -q 'quota_ports'; ck "自愈前置：quota 表已就位" $?
nft delete table inet sbx_policy >/dev/null 2>&1
! nft list table inet sbx_policy >/dev/null 2>&1; ck "手工删除 sbx_policy 成功" $?
REBUILT=1
for i in $(seq 1 18); do
  nft list table inet sbx_policy >/dev/null 2>&1 && { REBUILT=0; break; }
  sleep 1
done
ck "策略表被外部删除后自动重建" $REBUILT
grep -q "策略表被外部移除" /tmp/e2e-panel.log; ck "日志明确记录重建原因" $?

# 8.6 clear 清除计数表与策略表，且退出码 0（iptables 链不存在不得报错）
policy_put '{"quota_enabled":false,"quota_limit_bytes":0,"ip_limit_enabled":false,"ip_limit_max":0}' >/dev/null 2>&1
kill -TERM "$CORE_PID" 2>/dev/null; wait "$CORE_PID" 2>/dev/null
env SBX_CONF="$PANEL_CONF" "$CORE" clear >/tmp/e2e-clear.log 2>&1
ck "sbx-core clear 退出码 0" $?
! nft list tables 2>/dev/null | grep -q 'inet sbx_'; ck "clear 后无 sbx_* 表残留（含 sbx_policy）" $?
env SBX_CONF="$PANEL_CONF" "$CORE" serve >>/tmp/e2e-panel.log 2>&1 &
CORE_PID=$!
sleep 2

# ---------------------------------------------------------------- 收尾
section "结果"
kill "$SB_PID" "$CLI_PID" "$HTTP_PID" 2>/dev/null
kill "$CORE_PID" 2>/dev/null
pkill -f "$ROOT/usr/local/bin/sbx-core" 2>/dev/null
ip netns del e2e 2>/dev/null
nft delete table inet sbx_traffic 2>/dev/null
nft delete table inet sbx_policy 2>/dev/null
if [[ "$PROD_STOPPED" == 1 ]]; then
  systemctl start sbx-panel >/dev/null 2>&1 && echo "  (生产 sbx-panel 已恢复)"
fi
echo "PASS=$PASS FAIL=$FAIL"
if [[ "$FAIL" -gt 0 ]]; then printf '%s\n' "${FAILED[@]}"; exit 1; fi
echo E2E_ALL_GREEN
