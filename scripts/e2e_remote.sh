#!/usr/bin/env bash
# e2e_remote.sh — 在真机上端到端验收 Go 版 SBX（在真机本地执行）
set -u
cd /root/sbx-build

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
"$CORE" version | grep -q "v3.0.4"; ck "core 版本 3.0.4 ($("$CORE" version))" $?
python3 -c "import json;d=json.load(open('$PANEL_CONF'));assert d['token'] and 1<=int(d['port'])<=65535"
ck "panel.json 合法(token+port)" $?

# ---------------------------------------------------------------- 2. 菜单加节点
section "2. 菜单添加 Shadowsocks 2022 节点"
E2E_PW="$(openssl rand -base64 16 | tr -d '\n')"
printf '1\n2\n18388\nss-e2e\n%s\n\n0\n0\n' "$E2E_PW" | env SBX_ROOT="$ROOT" SBX_NO_SERVICE=1 \
  NO_COLOR=1 bash "$ROOT/usr/local/bin/sbx" >/tmp/e2e-menu.log 2>&1
ck "菜单流程退出码 0" $?
python3 - <<PY
import json
nodes=json.load(open("$NODES_JSON"))
assert len(nodes)==1, nodes
n=nodes[0]
assert n["type"]=="shadowsocks" and int(n["port"])==18388 and n["name"]=="ss-e2e", n
PY
ck "nodes.json 记录正确" $?
python3 - <<PY
import json
cfg=json.load(open("$SB_DIR/config.json"))
inb=[i for i in cfg["inbounds"] if i.get("tag","").startswith("sbx-n")]
assert len(inb)==1 and inb[0]["listen_port"]==18388 and inb[0]["method"]=="2022-blake3-aes-128-gcm", inb
assert any(o["tag"]=="direct" for o in cfg["outbounds"])
PY
ck "sing-box 配置生成正确" $?
"$SB_BIN" check -c "$SB_DIR/config.json" >/dev/null 2>&1; ck "sing-box check 通过" $?
grep -q "ss://" /tmp/e2e-menu.log; ck "分享链接已输出" $?

# ---------------------------------------------------------------- 3. 启动服务
section "3. 启动 sing-box 与面板"
"$SB_BIN" run -C "$SB_DIR" >/tmp/e2e-sb.log 2>&1 &
SB_PID=$!
for i in $(seq 1 30); do ss -Hlnt | grep -q ':18388 ' && break; sleep 0.5; done
PORT=$(python3 -c "import json;print(json.load(open('$PANEL_CONF'))['port'])")
TOKEN=$(python3 -c "import json;print(json.load(open('$PANEL_CONF'))['token'])")
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
python3 -m http.server 8000 --bind 10.66.0.1 --directory /tmp >/tmp/e2e-http.log 2>&1 &
HTTP_PID=$!
# 客户端放进 e2e 命名空间：client->server 才会经 veth 触发 INPUT/OUTPUT 计数
# （宿主内连本地地址走 lo，会被 iif lo return 跳过——这是计数口径的一部分）
cat >/tmp/e2e-client.json <<EOF
{"log":{"level":"warn"},
 "inbounds":[{"type":"mixed","tag":"in","listen":"10.66.0.2","listen_port":10801}],
 "outbounds":[{"type":"shadowsocks","tag":"out","server":"10.66.0.1","server_port":18388,
   "method":"2022-blake3-aes-128-gcm","password":"$(python3 -c "import json;print(json.load(open('$NODES_JSON'))[0]['password'])")"}]}
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
api_total() { curl -fsS "http://127.0.0.1:$PORT/api/summary?token=$TOKEN" | python3 -c "
import json,sys
d=json.load(sys.stdin)
n=[x for x in d['nodes'] if x['id']==1][0]
print(n['total']['rx'], n['total']['tx'])"; }
read RX TX <<< "$(api_total 2>/dev/null || echo 0 0)"
echo "  节点累计 rx=$RX tx=$TX"
[[ "$TX" -ge 1000000 ]]; ck "tx≥1MB（下载计入）" $?
[[ "$RX" -ge 500 && "$RX" -le 20000 ]]; ck "rx 在请求量级（无虚增）" $?
curl -fsS "http://127.0.0.1:$PORT/api/summary?token=$TOKEN" | python3 -c "
import json,sys
d=json.load(sys.stdin)
assert d['rate_known'] is True, d
assert d['total']['tx']>=1000000
sys.exit(0 if d['healthy'] else 1)"; ck "healthy 且速率已知" $?

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
python3 -c "
import sqlite3
con=sqlite3.connect('$APP_DIR/traffic.db')
ep=con.execute(\"select v from meta where k='epoch'\").fetchone()[0]
cs=con.execute('select count(*) from counter_state').fetchone()[0]
assert cs>0 and ep!='', (ep,cs)
print('  epoch=',ep,'counter_state rows=',cs)"; ck "meta.epoch 与基线正常" $?

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

# ---------------------------------------------------------------- 7. 与 Python reference 对拍
section "7. Go vs Python reference 输出对拍"
kill -TERM "$CORE_PID" 2>/dev/null; wait "$CORE_PID" 2>/dev/null
mkdir -p /tmp/e2e-parity/etc/sbx /tmp/e2e-parity/etc/sing-box
cp "$APP_DIR/traffic.db" /tmp/e2e-parity/etc/sbx/traffic.db
cp "$NODES_JSON" /tmp/e2e-parity/etc/sbx/nodes.json
cp "$SB_DIR/config.json" /tmp/e2e-parity/etc/sing-box/config.json
cat >/tmp/e2e-parity/go.json <<EOF
{"db":"/tmp/e2e-parity/etc/sbx/traffic.db",
 "nodes_file":"/tmp/e2e-parity/etc/sbx/nodes.json","tz":"Asia/Shanghai","interval":2}
EOF
cat >/tmp/e2e-parity/py.py <<'EOF'
import json, sys, os
sys.path.insert(0, "/root/sbx-build/src")
os.environ["SBX_CONF"]="/tmp/e2e-parity/py-conf.json"
conf={"db":"/tmp/e2e-parity/etc/sbx/traffic.db",
      "nodes_file":"/tmp/e2e-parity/etc/sbx/nodes.json",
      "tz":"Asia/Shanghai","interval":2,"backend":"nft"}
json.dump(conf, open("/tmp/e2e-parity/py-conf.json","w"))
import panel
con=panel.db_connect(conf)
s=panel.build_summary(conf, con, None); l=panel.build_live(conf, con, None)
keep=lambda d:{k:d[k] for k in ("day","tz","interval","backend")}|{
  "nodes":[{k:n[k] for k in ("id","name","type","today","total")} for n in d["nodes"]],
  "today":d["today"],"total":d["total"],
  "system_today":d["system_today"],"system_total":d["system_total"]}
print(json.dumps({"summary":keep(s),"live_keep":{"nodes":[{k:n[k] for k in ("id",)} for n in l["nodes"]]}}))
EOF
python3 /tmp/e2e-parity/py.py >/tmp/e2e-parity/py.json 2>/tmp/e2e-parity/py.err || { cat /tmp/e2e-parity/py.err; }
env SBX_CONF=/tmp/e2e-parity/go.json "$CORE" once >/dev/null 2>&1
# 用 Go 的 build_summary 直接对拍：通过临时驱动程序
cat >/tmp/e2e-parity/godrive.go <<'EOF'
package main

import (
    "encoding/json"
    "fmt"
    "os"

    "github.com/k6nfmm7dbr-commits/sbx/internal/config"
    "github.com/k6nfmm7dbr-commits/sbx/internal/database"
    "github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

func main() {
    raw := map[string]any{}
    b,_ := os.ReadFile(os.Getenv("SBX_CONF"))
    json.Unmarshal(b, &raw)
    _ = raw
    cfg := config.Load()
    cfg.Backend = "nft"
    db, err := database.Open(cfg.DB)
    if err != nil { panic(err) }
    defer db.Close()
    s, err := traffic.BuildSummary(cfg, db.DB, nil)
    if err != nil { panic(err) }
    type nodeOut struct {
        ID any `json:"id"`
        Today traffic.Counters `json:"today"`
        Total traffic.Counters `json:"total"`
    }
    out := struct {
        Day string `json:"day"`
        TZ string `json:"tz"`
        Interval int `json:"interval"`
        Backend string `json:"backend"`
        Nodes []nodeOut `json:"nodes"`
        Today traffic.Counters `json:"today"`
        Total traffic.Counters `json:"total"`
        SystemToday traffic.Counters `json:"system_today"`
        SystemTotal traffic.Counters `json:"system_total"`
    }{Day:s.Day,TZ:s.TZ,Interval:s.Interval,Backend:s.Backend,
       Today:s.Today,Total:s.Total,SystemToday:s.SystemToday,SystemTotal:s.SystemTotal}
    for _, n := range s.Nodes {
        out.Nodes = append(out.Nodes, nodeOut{ID:n.ID,Today:n.Today,Total:n.Total})
    }
    j,_ := json.Marshal(out)
    fmt.Println(string(j))
}
EOF
mkdir -p /root/sbx-build/cmd/e2edrive && cp /tmp/e2e-parity/godrive.go /root/sbx-build/cmd/e2edrive/main.go
(cd /root/sbx-build && go run ./cmd/e2edrive) >/tmp/e2e-parity/go.json.out 2>/dev/null
rm -rf /root/sbx-build/cmd/e2edrive
python3 - <<'PY'
import json
py=json.load(open("/tmp/e2e-parity/py.json"))["summary"]
go=json.load(open("/tmp/e2e-parity/go.json.out"))
for k in ("day","tz","interval"):
    assert py[k]==go[k], (k,py[k],go[k])
assert py["today"]==go["today"], ("today",py["today"],go["today"])
assert py["total"]==go["total"], ("total",py["total"],go["total"])
assert py["system_today"]==go["system_today"]
assert py["system_total"]==go["system_total"]
pm={str(n["id"]):n for n in py["nodes"]}
gm={str(n["id"]):n for n in go["nodes"]}
assert pm.keys()==gm.keys()
for i in pm:
    assert pm[i]["today"]==gm[i]["today"], (i,pm[i]["today"],gm[i]["today"])
    assert pm[i]["total"]==gm[i]["total"], (i,pm[i]["total"],gm[i]["total"])
print("  Python/Go summary 完全一致")
PY
ck "build_summary 数值逐字段一致" $?

# ---------------------------------------------------------------- 收尾
section "结果"
kill "$SB_PID" "$CLI_PID" "$HTTP_PID" 2>/dev/null
ip netns del e2e 2>/dev/null
pkill -f sbx-core-linux 2>/dev/null
echo "PASS=$PASS FAIL=$FAIL"
if [[ "$FAIL" -gt 0 ]]; then printf '%s\n' "${FAILED[@]}"; exit 1; fi
echo E2E_ALL_GREEN
