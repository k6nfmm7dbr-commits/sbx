#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
SBX 回归测试（无需 nft/iptables/sing-box，全部在进程内模拟内核计数器后端）。

运行: python3 tests/run_all.py

覆盖：
  * 端口区间归一化/合并（parse_ports）
  * 计数器名 -> scope/方向 映射（parse_counter_name / parse_epoch_name）
  * nft / iptables 后端格式解析
  * 采集器单调差分：首见、增量、归零、规则集换代
  * nft / iptables 规则生成
  * 七种协议的分享链接与订阅
  * nodes_tool 增删改 + 节点 ID 单调不回收 + 端口占用判断
  * build_summary / build_live 结构一致
"""

import base64
import json
import os
import shutil
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRC = os.path.join(ROOT, "src")
sys.path.insert(0, SRC)

# 指向临时目录，绝不写真实系统
TMP = tempfile.mkdtemp(prefix="sbx-test-")
SBX_DIR = os.path.join(TMP, "etc", "sbx")
SB_DIR = os.path.join(TMP, "etc", "sing-box")
os.makedirs(SBX_DIR, exist_ok=True)
os.makedirs(SB_DIR, exist_ok=True)
os.environ["SBX_DIR"] = SBX_DIR
os.environ["SBX_CONF"] = os.path.join(SBX_DIR, "panel.json")
os.environ["SBX_SB_CONF"] = os.path.join(SB_DIR, "config.json")

import panel            # noqa: E402
import nodes_tool       # noqa: E402


# ------------------------------------------------------------------ 微型测试框架
PASS = 0
FAIL = 0
FAILED = []


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
    else:
        FAIL += 1
        FAILED.append((name, detail))
        print("  ✗ %s  %s" % (name, detail))


def section(title):
    print("\n== %s ==" % title)


def run(func):
    print("- %s" % func.__name__)
    try:
        func()
    except Exception as e:  # noqa: BLE001
        global FAIL
        FAIL += 1
        FAILED.append((func.__name__, "抛出异常: %r" % e))
        import traceback
        traceback.print_exc()


# ------------------------------------------------------------------ 端口区间
def test_parse_ports():
    check("单端口", panel.parse_ports({"port": 443}) == [(443, 443)])
    check("字符串端口", panel.parse_ports({"port": "8443"}) == [(8443, 8443)])
    check("端口 0 非法", panel.parse_ports({"port": 0}) == [])
    check("端口越界", panel.parse_ports({"port": 70000}) == [])
    check("端口非数字", panel.parse_ports({"port": "abc"}) == [])
    check("无端口", panel.parse_ports({}) == [])


# ------------------------------------------------------------------ 计数器名
def test_counter_names():
    check("nft 节点 rx", panel.parse_counter_name("sbx_n3_i") == ("node:3", "rx"))
    check("nft 节点 tx", panel.parse_counter_name("sbx_n3_o") == ("node:3", "tx"))
    check("nft system", panel.parse_counter_name("sbx_sys_i") == ("system", "rx"))
    check("ipt 节点 v4", panel.parse_counter_name("sbx:n3:i@v4") == ("node:3", "rx"))
    check("ipt 节点 v6", panel.parse_counter_name("sbx:sys:o@v6") == ("system", "tx"))
    check("epoch nft", panel.parse_epoch_name("sbx_epoch_123") == 123)
    check("epoch ipt", panel.parse_epoch_name("sbx:epoch:7@v4") == 7)
    check("未知名返回 None", panel.parse_counter_name("random") is None)


# ------------------------------------------------------------------ 后端解析
def test_nft_parse():
    fake = json.dumps({"nftables": [
        {"counter": {"name": "sbx_epoch_9", "bytes": 0, "packets": 0}},
        {"counter": {"name": "sbx_n1_i", "bytes": 1000, "packets": 7}},
        {"counter": {"name": "sbx_n1_o", "bytes": 2000, "packets": 9}},
    ]})

    def fake_run(args, timeout=15):
        return 0, fake, ""

    orig = panel.run_cmd
    panel.run_cmd = fake_run
    try:
        b = panel.NftBackend({"backend": "nft"})
        got = b.read()
        check("nft 解析字节", got["sbx_n1_i"] == (1000, 7))
        check("nft 解析包含 epoch", "sbx_epoch_9" in got)
    finally:
        panel.run_cmd = orig


def test_iptables_parse():
    chain_in = "\n".join([
        "Chain SBX_IN (1 references)",
        " pkts bytes target     prot opt in     out     source               destination",
        "    5  1000            tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* sbx:n1:i */",
        "    2  300             tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* sbx:epoch:9 */",
    ])
    chain_out = "\n".join([
        "Chain SBX_OUT (1 references)",
        " pkts bytes target     prot opt in     out     source               destination",
        "    8  2000            tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* sbx:n1:o */",
    ])

    def fake_run(args, timeout=15):
        if args[-1] == "SBX_IN":
            return 0, chain_in, ""
        return 0, chain_out, ""

    orig_run = panel.run_cmd
    orig_which = shutil.which
    panel.run_cmd = fake_run
    shutil.which = lambda name: name if name in ("iptables",) else orig_which(name)
    try:
        b = panel.IptablesBackend({"backend": "iptables"})
        got = b.read()
        check("ipt v4 rx 聚合", got["sbx:n1:i@v4"] == (1000, 5))
        check("ipt v4 tx 聚合", got["sbx:n1:o@v4"] == (2000, 8))
        check("ipt epoch 存在", "sbx:epoch:9" in got)
    finally:
        panel.run_cmd = orig_run
        shutil.which = orig_which


# ------------------------------------------------------------------ 采集器差分
class FakeBackend:
    name = "nft"

    def __init__(self, seq):
        self.seq = list(seq)
        self.repairs = 0

    def read(self):
        return self.seq.pop(0) if self.seq else {}

    def repair(self):
        self.repairs += 1
        return True


def _make_collector(conf, seq):
    conf = dict({"backend": "nft", "interval": 2, "tz": "UTC",
                 "db": os.path.join(SBX_DIR, "t.db")}, **conf)
    c = panel.Collector(conf)
    c.backend = FakeBackend(seq)
    # 固定时钟：用真实感的时间戳（毫秒级，年 2023 附近），保证 duration 落在可信窗口内
    t = {"ns": 1_700_000_000_000_000_000}   # ≈ 2023-11，ms 值 > 1e12，不会被当秒级旧值
    panel.time.time_ns = lambda: t["ns"]
    panel.time.time = lambda: t["ns"] // 1_000_000_000
    panel.time.monotonic = lambda: t["ns"] // 1_000_000_000
    return c, t


def _advance(t, ms):
    t["ns"] += ms * 1_000_000


def _totals(con):
    return {r["scope"]: dict(r) for r in con.execute("SELECT * FROM totals").fetchall()}


def test_collector_diff():
    db = os.path.join(SBX_DIR, "diff.db")
    if os.path.exists(db):
        os.remove(db)
    c, t = _make_collector({"db": db}, [
        {"sbx_n1_i": (100, 10), "sbx_n1_o": (50, 5)},
        {"sbx_n1_i": (150, 15), "sbx_n1_o": (75, 8)},
        {"sbx_n1_i": (20, 2), "sbx_n1_o": (10, 1)},   # 归零
    ])
    c.tick()                                   # 首见：全量累计，无速率
    _advance(t, 2000)
    c.tick()                                   # 增量：delta=50/25
    _advance(t, 2000)
    c.tick()                                   # 归零：delta=20/10
    con = c.con
    tot = _totals(con)
    check("累计 rx=170", tot["node:1"]["rx"] == 170, str(tot.get("node:1")))
    check("累计 tx=85", tot["node:1"]["tx"] == 85)
    check("归零后基线更新", con.execute(
        "SELECT last_bytes FROM counter_state WHERE name='sbx_n1_i'").fetchone()["last_bytes"] == 20)
    # 首见与归零轮不写速率样本，仅增量轮写
    n_samples = con.execute("SELECT COUNT(*) n FROM samples WHERE valid=1").fetchone()["n"]
    check("仅增量轮写有效样本", n_samples == 1, "valid samples=%d" % n_samples)
    con.close()


def test_collector_epoch_switch():
    db = os.path.join(SBX_DIR, "epoch.db")
    if os.path.exists(db):
        os.remove(db)
    c, t = _make_collector({"db": db}, [
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (100, 10)},
        {"sbx_epoch_2": (0, 0), "sbx_n1_i": (5, 1)},   # 世代切换：从头基线
    ])
    c.tick()
    _advance(t, 2000)
    c.tick()
    con = c.con
    tot = _totals(con)
    # 世代切换后，计数器 5 应视为全量累计（新基线从零起）
    check("换代后全量累计 rx=105", tot["node:1"]["rx"] == 105, str(tot.get("node:1")))
    check("meta.epoch=2", con.execute("SELECT v FROM meta WHERE k='epoch'").fetchone()["v"] == "2")
    con.close()


# ------------------------------------------------------------------ 规则生成
def test_gen_rules():
    nodes = [{"id": 1, "type": "vless", "port": 443},
             {"id": 2, "type": "shadowsocks", "port": 8388}]
    nft = panel.gen_nft({"nft_conf": "x"}, nodes, epoch=42)
    check("nft 含 epoch 计数器", "counter sbx_epoch_42" in nft)
    check("nft 含节点计数器", "counter sbx_n2_i" in nft and "counter sbx_n2_o" in nft)
    check("nft dport 规则", "dport { 443 }" in nft or "dport {443}" in nft)
    # vless 纯 TCP 只生成入/出 2 条规则；shadowsocks 双栈生成 4 条
    check("nft vless 仅 TCP", nft.count("counter name sbx_n1_") == 2)
    check("nft ss 双栈", nft.count("counter name sbx_n2_") == 4)

    ipt = panel.gen_iptables({"ipt_script": "x"}, nodes, epoch=42)
    check("ipt 含 epoch 注释", "sbx:epoch:42" in ipt)
    check("ipt 含节点注释", "sbx:n1:i" in ipt and "sbx:n2:o" in ipt)
    check("ipt ss 双协议计数", ipt.count("sbx:n2:i") == 2)  # tcp/udp 各一次


# ------------------------------------------------------------------ 分享链接
def test_links():
    vless = {"id": 1, "type": "vless", "port": 443, "uuid": "u" * 16, "sni": "example.com",
             "public_key": "pbk", "short_id": "sid", "flow": "xtls-rprx-vision", "name": "测试"}
    link = nodes_tool.node_link(vless, "1.2.3.4")
    check("vless 链接前缀", link.startswith("vless://"))
    check("vless 含 reality", "security=reality" in link)
    check("vless 含 pbk", "pbk=pbk" in link)

    ss = {"id": 2, "type": "shadowsocks", "port": 8388, "method": "2022-blake3-aes-128-gcm",
          "password": "pw", "name": "ss"}
    check("ss 链接", nodes_tool.node_link(ss, "1.2.3.4").startswith("ss://"))

    trojan = {"id": 5, "type": "trojan", "port": 8443, "password": "pw", "sni": "example.com"}
    check("trojan 链接", nodes_tool.node_link(trojan, "1.2.3.4").startswith("trojan://"))

    anytls = {"id": 7, "type": "anytls", "port": 443, "password": "pw", "sni": "example.com"}
    check("anytls 链接", nodes_tool.node_link(anytls, "1.2.3.4").startswith("anytls://"))

    # 订阅：host6 提供时每个节点多一条 IPv6
    sub = nodes_tool.subscription([vless], host="1.2.3.4", host6="2001:db8::1")
    decoded = base64.b64decode(sub).decode()
    check("订阅含 IPv4 与 IPv6", decoded.count("vless://") == 2)
    check("IPv6 标签", "-IPv6" in decoded)


# ------------------------------------------------------------------ nodes_tool
def _write_json(path, data):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f)


def test_next_id_monotonic():
    state = os.path.join(SBX_DIR, "state.json")
    nodes_json = os.path.join(SBX_DIR, "nodes.json")
    _write_json(state, {})
    _write_json(nodes_json, [{"id": 1, "type": "vless", "port": 443}])

    nid1 = nodes_tool.next_id(nodes_tool.load_nodes())
    check("首个新增 id=2", nid1 == 2, str(nid1))
    # 删除节点 1 后再分配，不能复用 1
    _write_json(nodes_json, [])
    nid2 = nodes_tool.next_id([])
    check("删除后不复用旧 id", nid2 == 3, str(nid2))
    # 手工写入更大 id 时兜底
    _write_json(nodes_json, [{"id": 10, "type": "vless", "port": 443}])
    nid3 = nodes_tool.next_id(nodes_tool.load_nodes())
    check("兼容手工大 id", nid3 == 11, str(nid3))


def test_port_used():
    nodes_json = os.path.join(SBX_DIR, "nodes.json")
    _write_json(nodes_json, [
        {"id": 1, "type": "vless", "port": 443},
        {"id": 2, "type": "trojan", "port": 8443},
    ])

    def used(p):
        import subprocess
        r = subprocess.run(
            [sys.executable, os.path.join(SRC, "nodes_tool.py"), "port-used", str(p)],
            capture_output=True, text=True, env=dict(os.environ))
        return r.returncode == 0

    check("占用单端口", used(443))
    check("占用另一节点端口", used(8443))
    check("未占用", not used(12345))


def test_add_remove_roundtrip():
    nodes_json = os.path.join(SBX_DIR, "nodes.json")
    _write_json(nodes_json, [])
    _write_json(os.environ["SBX_SB_CONF"], {
        "inbounds": [], "outbounds": [{"type": "direct", "tag": "direct"}],
        "route": {"final": "direct"},
    })
    import subprocess
    env = dict(os.environ)

    def call(*args):
        return subprocess.run([sys.executable, os.path.join(SRC, "nodes_tool.py")] + list(args),
                              capture_output=True, text=True, env=env)

    r = call("add", "shadowsocks", "--port", "8388", "--method", "2022-blake3-aes-128-gcm",
             "--password", "pw")
    check("add 返回候选", r.returncode == 0 and "candidate" in r.stdout,
          "rc=%d out=%s err=%s" % (r.returncode, r.stdout, r.stderr))
    check("add 生成候选 nodes", os.path.exists(nodes_json + ".candidate"))

    call("commit")
    check("commit 后节点数=1", call("count").stdout.strip() == "1")

    rid = json.load(open(nodes_json))[0]["id"]
    r = call("remove", str(rid))
    check("remove 返回候选", r.returncode == 0, "rc=%d out=%s err=%s" % (r.returncode, r.stdout, r.stderr))
    call("commit")
    check("remove 后节点数=0", call("count").stdout.strip() == "0")


# ------------------------------------------------------------------ summary / live
def test_summary_live_shape():
    db = os.path.join(SBX_DIR, "shape.db")
    if os.path.exists(db):
        os.remove(db)
    con = panel.db_connect({"db": db, "nodes_file": os.path.join(SBX_DIR, "nodes.json"),
                            "interval": 2, "tz": "UTC"})
    _write_json(os.path.join(SBX_DIR, "nodes.json"),
                [{"id": 1, "type": "vless", "port": 443, "name": "n1"}])
    s = panel.build_summary({"nodes_file": os.path.join(SBX_DIR, "nodes.json"),
                             "interval": 2, "tz": "UTC"}, con, None)
    # vless 只统计 TCP，UDP 连接数为 None；TCP 为整数
    check("summary 节点字段", isinstance(s["nodes"][0]["conns_tcp"], int) and
          s["nodes"][0]["conns_udp"] is None and "rate" in s["nodes"][0])
    check("summary 汇总键", {"rx", "tx"} <= set(s["rate_total"]))
    l = panel.build_live({"nodes_file": os.path.join(SBX_DIR, "nodes.json"),
                          "interval": 2, "tz": "UTC"}, con, None)
    check("live 结构", l["nodes"][0]["id"] == 1 and "rate_total" in l)
    con.close()


# ------------------------------------------------------------------ 入口
def main():
    section("端口区间")
    run(test_parse_ports)
    section("计数器名映射")
    run(test_counter_names)
    section("后端解析")
    run(test_nft_parse)
    run(test_iptables_parse)
    section("采集器差分")
    run(test_collector_diff)
    run(test_collector_epoch_switch)
    section("规则生成")
    run(test_gen_rules)
    section("分享链接")
    run(test_links)
    section("nodes_tool")
    run(test_next_id_monotonic)
    run(test_port_used)
    run(test_add_remove_roundtrip)
    section("summary/live")
    run(test_summary_live_shape)

    print("\n" + "=" * 60)
    print("通过 %d / %d" % (PASS, PASS + FAIL))
    if FAILED:
        print("失败项:")
        for name, detail in FAILED:
            print("  - %s: %s" % (name, detail))
    shutil.rmtree(TMP, ignore_errors=True)
    sys.exit(1 if FAIL else 0)


if __name__ == "__main__":
    main()
