#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
gen_goldens.py — 用旧 Python 实现（reference）生成 Go 测试的金标夹具。

运行: cd 仓库根目录 && python3 tests/gen_goldens.py

产物（全部提交进仓库，Go 测试不依赖 Python 运行时）:
  internal/traffic/testdata/golden/traffic_scenarios.json   计数器序列 + 期望 DB 状态
  internal/traffic/testdata/golden/summary_live.json        build_summary/live 形状样本
  internal/nodes/testdata/link_fixtures.json                分享链接矩阵
  internal/nodes/testdata/inbound_fixtures.json             sing-box inbound 结构
  internal/firewall/testdata/gen_nft.golden                 nft 规则文本
  internal/firewall/testdata/gen_iptables.golden            iptables 规则文本

夹具由固定时钟驱动，可重复生成；CI 中重新生成并 git diff --exit-code 可检测
Go 行为与 reference 漂移。
"""

import base64
import copy
import io
import json
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRC = os.path.join(ROOT, "src")
sys.path.insert(0, SRC)

TMP = tempfile.mkdtemp(prefix="sbx-golden-")
SBX_DIR = os.path.join(TMP, "etc", "sbx")
SB_DIR = os.path.join(TMP, "etc", "sing-box")
os.makedirs(SBX_DIR, exist_ok=True)
os.makedirs(SB_DIR, exist_ok=True)
os.environ["SBX_DIR"] = SBX_DIR
os.environ["SBX_CONF"] = os.path.join(SBX_DIR, "panel.json")
os.environ["SBX_SB_CONF"] = os.path.join(SB_DIR, "config.json")

import panel  # noqa: E402
import nodes_tool  # noqa: E402


def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    mode = "wb" if isinstance(data, bytes) else "w"
    with open(path, mode) as f:
        f.write(data)


# ---------------------------------------------------------------- 流量场景
T0_MS = 1_700_000_000_000


class FakeBackend:
    name = "nft"

    def __init__(self, seq):
        self.seq = list(seq)

    def read(self):
        return self.seq.pop(0) if self.seq else {}

    def repair(self):
        return True


def db_dump(con):
    out = {}
    for table in ("daily", "totals", "samples", "counter_state", "meta"):
        rows = [dict(r) for r in con.execute("SELECT * FROM %s" % table).fetchall()]
        out[table] = sorted(rows, key=lambda r: json.dumps(r, sort_keys=True))
    return out


def run_scenario(name, seq, times_ms, interval=2):
    """seq[i] 在 times_ms[i] 毫秒时刻被读取（时钟完全固定，可复现）。"""
    db_path = os.path.join(SBX_DIR, name + ".db")
    if os.path.exists(db_path):
        os.remove(db_path)
    conf = {"backend": "nft", "interval": interval, "tz": "UTC",
            "db": db_path, "nodes_file": os.path.join(SBX_DIR, "nodes.json")}
    c = panel.Collector(conf)
    c.backend = FakeBackend(copy.deepcopy(seq))
    clock = {"ms": times_ms[0]}

    def set_clock(ms):
        clock["ms"] = ms
        ns = ms * 1_000_000

        class FakeDT(panel.datetime):
            @classmethod
            def now(cls, tz=None):
                return panel.datetime.fromtimestamp(ms / 1000.0, tz)

        panel.time.time_ns = lambda: ns
        panel.time.time = lambda: ns // 1_000_000_000
        panel.time.monotonic = lambda: ns // 1_000_000
        panel.datetime = FakeDT

    for i in range(len(seq)):
        set_clock(times_ms[i])
        c.tick()
    con = c.con
    fixture = {
        "name": name,
        "interval": interval,
        "times_ms": times_ms[:len(seq)],
        "counters": [{k: list(v) for k, v in snap.items()} for snap in seq],
        "expected_db": db_dump(con),
    }
    con.close()
    return fixture


def scenario_basic():
    seq = [
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (100, 10), "sbx_n1_o": (50, 5),
         "sbx_sys_i": (500, 50)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (150, 15), "sbx_n1_o": (75, 8),
         "sbx_sys_i": (600, 60)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (20, 2), "sbx_n1_o": (90, 9),
         "sbx_sys_i": (650, 55)},                       # n1_i 归零
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (70, 7), "sbx_n1_o": (95, 10),
         "sbx_sys_i": (700, 62)},
    ]
    t = [T0_MS, T0_MS + 2000, T0_MS + 4000, T0_MS + 6000]
    return run_scenario("basic", seq, t)


def scenario_epoch_switch():
    seq = [
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (100, 10)},
        {"sbx_epoch_2": (0, 0), "sbx_n1_i": (5, 1)},
        {"sbx_epoch_2": (0, 0), "sbx_n1_i": (11, 2)},
    ]
    t = [T0_MS, T0_MS + 2000, T0_MS + 4000]
    return run_scenario("epoch_switch", seq, t)


def scenario_duration_edges():
    # 三轮间隔分别为 300ms(过短)/8000ms(过长)/500ms(恰好下界,有效)
    seq = [
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (100, 10), "sbx_n1_o": (60, 6)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (110, 11), "sbx_n1_o": (70, 7)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (130, 13), "sbx_n1_o": (90, 9)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (140, 14), "sbx_n1_o": (100, 10)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (150, 15), "sbx_n1_o": (110, 11)},
    ]
    t = [T0_MS, T0_MS + 300, T0_MS + 8300, T0_MS + 13800, T0_MS + 14300]
    return run_scenario("duration_edges", seq, t)


def scenario_sample_second_monotonic():
    # 时钟回拨：第三轮回退到更早的墙钟，样本秒仍须单调递增
    seq = [
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (100, 10), "sbx_n1_o": (50, 5)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (150, 15), "sbx_n1_o": (75, 8)},
        {"sbx_epoch_1": (0, 0), "sbx_n1_i": (200, 20), "sbx_n1_o": (100, 10)},
    ]
    t = [T0_MS, T0_MS + 2000, T0_MS + 1500]  # 第三次时间倒流
    return run_scenario("sample_second_monotonic", seq, t)


def traffic_scenarios():
    return {
        "t0_ms": T0_MS,
        "scenarios": [
            scenario_basic(),
            scenario_epoch_switch(),
            scenario_duration_edges(),
            scenario_sample_second_monotonic(),
        ],
    }


# ---------------------------------------------------------------- summary/live
def summary_live():
    nodes_file = os.path.join(SBX_DIR, "nodes.json")
    write(nodes_file, json.dumps([
        {"id": 1, "type": "vless", "port": 443, "name": "测试节点"},
        {"id": 2, "type": "shadowsocks", "port": 8388, "name": "ss-2"},
    ], ensure_ascii=False))
    db_path = os.path.join(SBX_DIR, "summary.db")
    if os.path.exists(db_path):
        os.remove(db_path)
    conf = {"backend": "nft", "interval": 2, "tz": "UTC",
            "db": db_path, "nodes_file": nodes_file}
    now_s = T0_MS // 1000
    con = panel.db_connect(conf)
    con.execute("INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES"
                "('2023-11-14','node:1',1000,2000,10,20),"
                "('2023-11-14','node:2',300,400,3,4),"
                "('2023-11-14','system',50,60,5,6),"
                "('2023-11-15','node:1',111,222,1,2)")
    con.execute("INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES"
                "('node:1',9999,8888,99,88),('node:2',777,666,7,6),('system',55,44,5,4)")
    latest = now_s - 1
    con.execute("INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES"
                "(?,?,1000,2000,2000,1),(?,?,300,400,2000,1)",
                (latest, "node:1", latest - 2, "node:2"))
    con.commit()

    # 固定时钟，保证 now / 速率窗口完全可复现
    _ns = T0_MS * 1_000_000
    panel.time.time = lambda: T0_MS // 1000
    panel.time.time_ns = lambda: _ns

    s = panel.build_summary(conf, con, None)
    l = panel.build_live(conf, con, None)
    con.close()
    return {
        "now_fixed": now_s,
        "nodes_file": [["id", "type", "port", "name"],
                       [1, "vless", 443, "测试节点"], [2, "shadowsocks", 8388, "ss-2"]],
        "summary": json.loads(json.dumps(s)),
        "live": json.loads(json.dumps(l)),
    }


# ---------------------------------------------------------------- 分享链接
def link_fixtures():
    cases = []

    def add(name, node, host, host6="", suffix=""):
        link = nodes_tool.node_link(node, host or None, suffix)
        cases.append({"name": name, "node": node, "host": host,
                      "suffix": suffix, "expected": link})

    vless = {"id": 1, "type": "vless", "port": 443, "uuid": "u" * 16,
             "sni": "www.microsoft.com", "public_key": "pbk_X+/=",
             "short_id": "sid01", "flow": "xtls-rprx-vision", "name": "测试 VLESS"}
    add("vless_full", vless, "1.2.3.4")

    vless_noflow = dict(vless, flow="", name="")
    add("vless_noflow_noname", vless_noflow, "example.com")

    ss = {"id": 2, "type": "shadowsocks", "port": 8388,
          "method": "2022-blake3-aes-128-gcm", "password": "a+bc=/d", "name": "SS 节点"}
    add("ss_special_chars", ss, "1.2.3.4")

    trojan = {"id": 3, "type": "trojan", "port": 8443,
              "password": "p@ss w:rd/+", "sni": "www.bing.com"}
    add("trojan_special", trojan, "[2001:db8::1]")

    anytls = {"id": 4, "type": "anytls", "port": 443,
              "password": "pw~.-_", "sni": "example.com"}
    add("anytls_plain", anytls, "1.2.3.4")

    add("ipv6_bracket", vless, "2001:db8::1")          # host 未带括号应自动加
    add("label_suffix", vless, "1.2.3.4", suffix="-IPv6")

    sub = nodes_tool.subscription([vless, ss], host="1.2.3.4", host6="2001:db8::1")
    sub_nov6 = nodes_tool.subscription([vless], host="1.2.3.4", host6=None)
    return {
        "cases": cases,
        "sub_with_v6_body": base64.b64decode(sub).decode(),
        "sub_no_v6_body": base64.b64decode(sub_nov6).decode(),
    }


# ---------------------------------------------------------------- inbound
def inbound_fixtures():
    nodes_list = [
        {"id": 1, "type": "vless", "port": 443, "uuid": "u" * 16,
         "sni": "www.microsoft.com", "private_key": "PK1", "short_id": "sid",
         "flow": "xtls-rprx-vision", "name": "v1"},
        {"id": 2, "type": "shadowsocks", "port": 8388,
         "method": "2022-blake3-aes-128-gcm", "password": "pw2"},
        {"id": 3, "type": "trojan", "port": 8443, "password": "pw3",
         "sni": "www.bing.com", "cert": "/etc/sbx/certs/cert.pem",
         "key": "/etc/sbx/certs/key.pem"},
        {"id": 4, "type": "anytls", "port": 9443, "password": "pw4",
         "sni": "example.org", "cert": "/etc/sbx/certs/cert.pem",
         "key": "/etc/sbx/certs/key.pem"},
    ]
    built = [nodes_tool.build_inbound(n) for n in nodes_list]

    sb_conf = os.environ["SBX_SB_CONF"]
    write(sb_conf, json.dumps({
        "log": {"level": "warn"},
        "inbounds": [{"type": "direct", "tag": "user-custom-in", "listen": "127.0.0.1",
                      "listen_port": 1080},
                     {"type": "vless", "tag": "sbx-n99", "listen": "::", "listen_port": 1}],
        "outbounds": [{"type": "block", "tag": "block"}],
        "route": {"rules": [{"inbound": "user-custom-in", "outbound": "block"}]},
    }))
    merged = nodes_tool.rebuild_config(nodes_tool.load_nodes.__globals__ and nodes_list)
    return {"inbounds_built": built, "rebuild_result": json.loads(json.dumps(merged))}


# ---------------------------------------------------------------- 规则文本
def rules_text():
    nodes_list = [{"id": 1, "type": "vless", "port": 443},
                  {"id": 2, "type": "shadowsocks", "port": 8388}]
    return {
        "nft": panel.gen_nft({}, nodes_list, epoch=42),
        "iptables": panel.gen_iptables({}, nodes_list, epoch=42),
    }


def main():
    root = ROOT
    write(os.path.join(root, "internal/traffic/testdata/golden/traffic_scenarios.json"),
          json.dumps(traffic_scenarios(), indent=1, ensure_ascii=False))
    write(os.path.join(root, "internal/traffic/testdata/golden/summary_live.json"),
          json.dumps(summary_live(), indent=1, ensure_ascii=False))
    write(os.path.join(root, "internal/nodes/testdata/link_fixtures.json"),
          json.dumps(link_fixtures(), indent=1, ensure_ascii=False))
    write(os.path.join(root, "internal/nodes/testdata/inbound_fixtures.json"),
          json.dumps(inbound_fixtures(), indent=1, ensure_ascii=False))
    rt = rules_text()
    write(os.path.join(root, "internal/firewall/testdata/gen_nft.golden"), rt["nft"])
    write(os.path.join(root, "internal/firewall/testdata/gen_iptables.golden"), rt["iptables"])
    print("金标夹具已生成")


if __name__ == "__main__":
    main()
