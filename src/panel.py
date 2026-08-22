#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
sbx-panel — sing-box 节点流量统计面板（内核级精确计数）

统计口径:
  * 数据来源为 netfilter 内核计数器（nftables named counter 或 iptables 自定义链），
    统计的是节点端口在 IP 层的真实字节数与包数，不抽样、不估算。
  * 采集器按固定间隔读取计数器并做单调差分累加写入 SQLite:
      delta = cur - last            (正常)
      delta = cur                   (检测到计数器归零/重建, 即 cur < last)
    因此 sing-box 重启、面板重启、nft 规则重载都不会导致丢计或重复计数。
  * rx = 服务器从客户端收到的字节 (= 用户上传)
    tx = 服务器发往客户端的字节 (= 用户下载)

仅依赖 Python 标准库。
"""

import base64
import contextlib
import hmac
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.parse
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

APP_DIR = os.environ.get("SBX_DIR", "/etc/sbx")
CONF_PATH = os.environ.get("SBX_CONF", os.path.join(APP_DIR, "panel.json"))
NFT_TABLE = "sbx_traffic"
IPT_CHAIN_IN = "SBX_IN"
IPT_CHAIN_OUT = "SBX_OUT"
SAMPLE_KEEP_SECONDS = 120                # 仅保留2分钟，用于实时加权速率

DEFAULT_CONF = {
    "db": os.path.join(APP_DIR, "traffic.db"),
    "nodes_file": os.path.join(APP_DIR, "nodes.json"),
    "nft_conf": os.path.join(APP_DIR, "nft.conf"),
    "ipt_script": os.path.join(APP_DIR, "iptables.sh"),
    "web_root": os.path.join(APP_DIR, "web"),
    "backend": "auto",
    "listen": "0.0.0.0",
    "port": 8080,
    "token": "",
    "interval": 2,
    "tz": "Asia/Shanghai",
}


def log(*a):
    msg = " ".join(str(x) for x in a)
    sys.stderr.write("[%s] %s\n" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg))
    sys.stderr.flush()


def load_conf():
    conf = dict(DEFAULT_CONF)
    try:
        with open(CONF_PATH, "r", encoding="utf-8") as f:
            conf.update(json.load(f))
    except FileNotFoundError:
        log("配置不存在, 使用默认值:", CONF_PATH)
    except Exception as e:
        log("配置解析失败:", e)
    return conf


def load_nodes(conf):
    """nodes.json: [{"id":1,"name":"...","type":"vless","port":443,"ports":"20000-30000","link":"..."}]"""
    try:
        with open(conf["nodes_file"], "r", encoding="utf-8") as f:
            data = json.load(f)
    except Exception:
        return []
    if isinstance(data, dict):
        data = data.get("nodes", [])
    out = []
    for n in data:
        if isinstance(n, dict) and "id" in n:
            out.append(n)
    return out


# --------------------------------------------------------------------------
# 时区 / 日期
# --------------------------------------------------------------------------

_TZ_CACHE = {}

# 无 tzdata 的精简系统上的固定偏移回退表（这些区无夏令时，固定偏移完全等价）
_TZ_FALLBACK = {
    "asia/shanghai": 8, "asia/chongqing": 8, "asia/urumqi": 6, "asia/hong_kong": 8,
    "asia/macau": 8, "asia/taipei": 8, "asia/singapore": 8, "asia/tokyo": 9,
    "asia/seoul": 9, "asia/bangkok": 7, "asia/kolkata": 5.5, "asia/dubai": 4,
    "utc": 0, "gmt": 0,
}


def _tzinfo(conf):
    """返回统计用时区。默认中国时间 (UTC+8)，缺少 tzdata 时自动用固定偏移，保证跨天切点稳定。"""
    name = (conf.get("tz") or "Asia/Shanghai").strip()
    if name.lower() in ("local", "system"):
        return None
    if name in _TZ_CACHE:
        return _TZ_CACHE[name]
    tz = None
    try:
        from zoneinfo import ZoneInfo
        tz = ZoneInfo(name)
    except Exception:
        m = re.match(r"^(?:UTC|GMT)?([+-])(\d{1,2})(?::?(\d{2}))?$", name, re.I)
        if m:
            sign = 1 if m.group(1) == "+" else -1
            tz = timezone(sign * timedelta(hours=int(m.group(2)), minutes=int(m.group(3) or 0)))
        else:
            hours = _TZ_FALLBACK.get(name.lower())
            if hours is not None:
                tz = timezone(timedelta(hours=hours))
            else:
                log("时区无法解析(系统缺少 tzdata), 回退到 UTC+8:", name)
                tz = timezone(timedelta(hours=8))
    _TZ_CACHE[name] = tz
    return tz


def now_local(conf):
    tz = _tzinfo(conf)
    if tz is None:
        return datetime.now()
    return datetime.now(tz)


def today_str(conf):
    return now_local(conf).strftime("%Y-%m-%d")


# --------------------------------------------------------------------------
# 数据库
# --------------------------------------------------------------------------

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    k TEXT PRIMARY KEY,
    v TEXT
);
CREATE TABLE IF NOT EXISTS counter_state (
    name       TEXT PRIMARY KEY,
    last_bytes INTEGER NOT NULL DEFAULT 0,
    last_pkts  INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS daily (
    day     TEXT NOT NULL,
    scope   TEXT NOT NULL,
    rx      INTEGER NOT NULL DEFAULT 0,
    tx      INTEGER NOT NULL DEFAULT 0,
    rx_pkts INTEGER NOT NULL DEFAULT 0,
    tx_pkts INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, scope)
);
CREATE TABLE IF NOT EXISTS totals (
    scope   TEXT PRIMARY KEY,
    rx      INTEGER NOT NULL DEFAULT 0,
    tx      INTEGER NOT NULL DEFAULT 0,
    rx_pkts INTEGER NOT NULL DEFAULT 0,
    tx_pkts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS samples (
    ts          INTEGER NOT NULL,
    scope       TEXT NOT NULL,
    rx          INTEGER NOT NULL DEFAULT 0,
    tx          INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    valid       INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (ts, scope)
);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);
"""


def db_connect(conf):
    path = conf["db"]
    os.makedirs(os.path.dirname(path), exist_ok=True)
    con = sqlite3.connect(path, timeout=30, isolation_level=None)
    con.row_factory = sqlite3.Row
    # WAL 让读写并发（面板查询 + 采集写入）互不阻塞；
    # 某些文件系统（如挂载的用户目录）不支持 WAL，静默退回默认日志模式。
    with contextlib.suppress(sqlite3.OperationalError, sqlite3.DatabaseError):
        con.execute("PRAGMA journal_mode=WAL")
    con.execute("PRAGMA busy_timeout=30000")
    con.executescript(SCHEMA)
    # 无损迁移旧数据库：旧 samples 没有 duration_ms / valid。
    cols = {r["name"] for r in con.execute("PRAGMA table_info(samples)").fetchall()}
    if "duration_ms" not in cols:
        con.execute("ALTER TABLE samples ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0")
    if "valid" not in cols:
        con.execute("ALTER TABLE samples ADD COLUMN valid INTEGER NOT NULL DEFAULT 1")
    # 升级清理：删除旧版本遗留的历史速率专用表。
    con.execute("DROP TABLE IF EXISTS rate_samples")
    # 升级前的旧样本没有真实耗时，不能用于实时速率计算；daily/totals 不受影响。
    con.execute("UPDATE samples SET valid=0 WHERE duration_ms<=0")
    return con


# --------------------------------------------------------------------------
# 计数器读取后端
# --------------------------------------------------------------------------

def run_cmd(args, timeout=15):
    try:
        p = subprocess.run(args, capture_output=True, timeout=timeout)
        return p.returncode, p.stdout.decode("utf-8", "replace"), p.stderr.decode("utf-8", "replace")
    except FileNotFoundError:
        return 127, "", "command not found: %s" % args[0]
    except subprocess.TimeoutExpired:
        return 124, "", "timeout"


def detect_backend(conf):
    b = (conf.get("backend") or "auto").lower()
    if b in ("nft", "nftables"):
        return "nft"
    if b in ("ipt", "iptables"):
        return "iptables"
    if shutil.which("nft"):
        rc, _, _ = run_cmd(["nft", "list", "tables"])
        if rc == 0:
            return "nft"
    if shutil.which("iptables"):
        return "iptables"
    return "nft"


class NftBackend:
    name = "nft"

    def __init__(self, conf):
        self.conf = conf

    def read(self):
        """返回 {counter_name: (bytes, pkts)}；表不存在时抛 LookupError"""
        rc, out, err = run_cmd(["nft", "-j", "list", "counters", "table", "inet", NFT_TABLE])
        if rc != 0:
            if "No such file or directory" in err or "does not exist" in err or rc == 1:
                raise LookupError(err.strip() or "nft table missing")
            raise RuntimeError("nft 读取失败: %s" % err.strip())
        try:
            doc = json.loads(out)
        except Exception as e:
            raise RuntimeError("nft JSON 解析失败: %s" % e)
        res = {}
        for item in doc.get("nftables", []):
            c = item.get("counter")
            if not c:
                continue
            res[c.get("name", "")] = (int(c.get("bytes", 0)), int(c.get("packets", 0)))
        if not res:
            raise LookupError("nft table has no counters")
        return res

    def repair(self):
        path = self.conf.get("nft_conf")
        if path and os.path.exists(path):
            rc, _, err = run_cmd(["nft", "-f", path])
            log("重建 nft 计数器表:", "ok" if rc == 0 else err.strip())
            return rc == 0
        return False


class IptablesBackend:
    name = "iptables"
    LINE = re.compile(r"^\s*(\d+)\s+(\d+)\s+.*?/\*\s*(sbx:[A-Za-z0-9_:.-]+)\s*\*/")

    def __init__(self, conf):
        self.conf = conf

    def _read_one(self, binary, family):
        res = {}
        found = False
        for chain in (IPT_CHAIN_IN, IPT_CHAIN_OUT):
            rc, out, err = run_cmd([binary, "-w", "-nvxL", chain])
            if rc != 0:
                continue
            found = True
            for line in out.splitlines():
                m = self.LINE.match(line)
                if not m:
                    continue
                pkts, byts, tag = int(m.group(1)), int(m.group(2)), m.group(3)
                # epoch 标记只需存在即可，不参与按 family 聚合
                if tag.startswith("sbx:epoch:"):
                    res[tag] = (0, 0)
                    continue
                key = "%s@%s" % (tag, family)
                prev = res.get(key, (0, 0))
                res[key] = (prev[0] + byts, prev[1] + pkts)
        if not found:
            raise LookupError("%s chains missing" % binary)
        return res

    def read(self):
        res = {}
        ok = False
        for binary, fam in (("iptables", "v4"), ("ip6tables", "v6")):
            if not shutil.which(binary):
                continue
            try:
                res.update(self._read_one(binary, fam))
                ok = True
            except LookupError:
                continue
        if not ok:
            raise LookupError("iptables 计数链不存在")
        return res

    def repair(self):
        path = self.conf.get("ipt_script")
        if path and os.path.exists(path):
            rc, _, err = run_cmd(["sh", path, "apply"])
            log("重建 iptables 计数链:", "ok" if rc == 0 else err.strip())
            return rc == 0
        return False


def make_backend(conf):
    return NftBackend(conf) if detect_backend(conf) == "nft" else IptablesBackend(conf)


# --------------------------------------------------------------------------
# 计数器名 -> (scope, direction) 映射
# --------------------------------------------------------------------------

def parse_counter_name(name):
    """
    nft:      sbx_n<id>_i / sbx_n<id>_o / sbx_sys_i / sbx_sys_o
    iptables: sbx:n<id>:i@v4 / sbx:sys:o@v6
    返回 (scope, 'rx'|'tx') 或 None
    """
    base = name.split("@", 1)[0]
    m = re.match(r"^sbx[_:](n(\d+)|sys)[_:]([io])$", base)
    if not m:
        return None
    scope = "system" if m.group(1) == "sys" else "node:%s" % m.group(2)
    return scope, ("rx" if m.group(3) == "i" else "tx")


def parse_epoch_name(name):
    """规则集世代标记: sbx_epoch_<n> / sbx:epoch:<n> -> int 或 None"""
    base = name.split("@", 1)[0]
    m = re.match(r"^sbx[_:]epoch[_:](\d+)$", base)
    return int(m.group(1)) if m else None


def snapshot_epoch(snapshot):
    for k in snapshot:
        e = parse_epoch_name(k)
        if e is not None:
            return e
    return None


# --------------------------------------------------------------------------
# 采集器：单调差分累加
# --------------------------------------------------------------------------

class Collector(threading.Thread):
    daemon = True

    def __init__(self, conf):
        super().__init__(name="collector")
        self.conf = conf
        # 不在这里建连接：SQLite 连接绑定创建它的线程，
        # 而 run() 在子线程执行。连接改为在首次使用它的线程里惰性创建。
        self.con = None
        self.backend = make_backend(conf)
        self.stop_flag = threading.Event()
        self.last_error = ""
        self.last_ok_ts = 0
        self.last_sample_ts = 0
        self.repair_at = 0

    def _ensure_con(self):
        if self.con is None:
            self.con = db_connect(self.conf)
        return self.con

    # ---- 状态读写 ----
    def _load_state(self):
        rows = self.con.execute(
            "SELECT name,last_bytes,last_pkts,updated_at FROM counter_state"
        ).fetchall()
        out = {}
        for r in rows:
            updated = int(r["updated_at"] or 0)
            # 旧版存秒，新版存毫秒；升级时自动兼容。
            if 0 < updated < 1_000_000_000_000:
                updated *= 1000
            out[r["name"]] = (r["last_bytes"], r["last_pkts"], updated)
        return out

    def _meta_get(self, k, default=None):
        row = self.con.execute("SELECT v FROM meta WHERE k=?", (k,)).fetchone()
        return row["v"] if row else default

    def _commit_tick(self, deltas, snapshot, ts, ts_ms, epoch):
        """
        增量入账 + 速率样本 + 基线保存 + 清理，全部放在同一个事务里。
        daily/totals 记录所有确定的字节增量；samples 只记录时长可信的速率样本。
        """
        day = today_str(self.conf)
        cur = self.con
        cur.execute("BEGIN IMMEDIATE")
        try:
            for scope, d in deltas.items():
                if d["rx"] or d["tx"] or d["rx_pkts"] or d["tx_pkts"]:
                    cur.execute(
                        "INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?,?) "
                        "ON CONFLICT(day,scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx, "
                        "rx_pkts=rx_pkts+excluded.rx_pkts, tx_pkts=tx_pkts+excluded.tx_pkts",
                        (day, scope, d["rx"], d["tx"], d["rx_pkts"], d["tx_pkts"]),
                    )
                    cur.execute(
                        "INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES(?,?,?,?,?) "
                        "ON CONFLICT(scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx, "
                        "rx_pkts=rx_pkts+excluded.rx_pkts, tx_pkts=tx_pkts+excluded.tx_pkts",
                        (scope, d["rx"], d["tx"], d["rx_pkts"], d["tx_pkts"]),
                    )
                # valid=1 才进入实时速率窗口；即使字节为0也写样本，让速率回落到零。
                if d.get("valid") and d.get("duration_ms", 0) > 0:
                    cur.execute(
                        "INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(?,?,?,?,?,1) "
                        "ON CONFLICT(ts,scope) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx, "
                        "duration_ms=MAX(duration_ms,excluded.duration_ms), valid=1",
                        (ts, scope, d["rx"], d["tx"], d["duration_ms"]),
                    )
            cur.execute("DELETE FROM counter_state")
            cur.executemany(
                "INSERT INTO counter_state(name,last_bytes,last_pkts,updated_at) VALUES(?,?,?,?)",
                [(k, v[0], v[1], ts_ms) for k, v in snapshot.items()],
            )
            if epoch is not None:
                cur.execute("INSERT INTO meta(k,v) VALUES('epoch',?) "
                            "ON CONFLICT(k) DO UPDATE SET v=excluded.v", (str(epoch),))
            cur.execute("DELETE FROM samples WHERE ts < ?", (ts - SAMPLE_KEEP_SECONDS,))
            cur.execute("COMMIT")
        except Exception:
            with contextlib.suppress(Exception):
                cur.execute("ROLLBACK")
            raise

    # ---- 单轮采集 ----
    def tick(self):
        self._ensure_con()
        ts_ms = time.time_ns() // 1_000_000
        wall_ts = ts_ms // 1000
        # samples 主键是秒；保证极端调度/时钟回拨时仍单调，不覆盖上一条样本。
        ts = max(wall_ts, self.last_sample_ts + 1) if self.last_sample_ts else wall_ts
        snapshot = self.backend.read()          # 抛异常由调用方处理
        epoch = snapshot_epoch(snapshot)
        prev_epoch = self._meta_get("epoch")
        fresh_ruleset = (epoch is not None and prev_epoch is not None
                         and str(epoch) != str(prev_epoch))
        state = {} if fresh_ruleset else self._load_state()
        expected_ms = max(1, int(self.conf.get("interval", 2))) * 1000
        min_ms = max(200, expected_ms // 4)
        max_ms = expected_ms * 3 + 500
        deltas = {}
        reset_hits = 0

        for name, (byts, pkts) in snapshot.items():
            parsed = parse_counter_name(name)
            if not parsed:
                continue
            scope, direction = parsed
            prev = state.get(name)
            valid = False
            duration_ms = 0
            if prev is None:
                # 第一次看到这个计数器：字节可累计，但起点未知，不能拿来算速率。
                lb, lp, _ = 0, 0, 0
                db, dp = byts, pkts
            else:
                lb, lp, last_ms = prev
                duration_ms = max(0, ts_ms - last_ms)
                if byts < lb:
                    # 计数器归零：当前字节可补进累计，但发生时刻未知，不能制造速率尖峰。
                    db, dp = byts, pkts
                    reset_hits += 1
                else:
                    db, dp = byts - lb, max(0, pkts - lp)
                    valid = min_ms <= duration_ms <= max_ms
            slot = deltas.setdefault(scope, {
                "rx": 0, "tx": 0, "rx_pkts": 0, "tx_pkts": 0,
                "duration_ms": 0, "valid": True, "seen_rx": False, "seen_tx": False,
            })
            if direction == "rx":
                slot["rx"] += db; slot["rx_pkts"] += dp; slot["seen_rx"] = True
            else:
                slot["tx"] += db; slot["tx_pkts"] += dp; slot["seen_tx"] = True
            slot["duration_ms"] = max(slot["duration_ms"], duration_ms)
            slot["valid"] = slot["valid"] and valid

        # 一个节点必须同时拿到入/出两个方向的可信基线，才写速率样本。
        for d in deltas.values():
            d["valid"] = bool(d["valid"] and d["seen_rx"] and d["seen_tx"]
                              and d["duration_ms"] > 0)

        self._commit_tick(deltas, snapshot, ts, ts_ms, epoch)
        self.last_sample_ts = ts
        self.last_ok_ts = wall_ts
        self.last_error = ""
        if fresh_ruleset:
            log("检测到规则集世代 %s -> %s, 累计流量已衔接，速率从新基线开始" % (prev_epoch, epoch))
        if reset_hits:
            log("检测到 %d 个计数器归零, 已补入累计；为避免假峰值，本轮不写速率" % reset_hits)

    def run(self):
        interval = max(1, int(self.conf.get("interval", 2)))
        deadline = time.monotonic()
        while not self.stop_flag.is_set():
            try:
                self.tick()
            except LookupError as e:
                self.last_error = "计数器不存在: %s" % e
                now = time.time()
                if now - self.repair_at > 30:
                    self.repair_at = now
                    self.backend.repair()
            except Exception as e:
                self.last_error = str(e)
                log("采集异常:", e)
            # 绝对周期调度：执行耗时不会叠加进下一轮，长期保持 2s 节拍。
            deadline += interval
            now_mono = time.monotonic()
            if deadline <= now_mono:
                # 若系统挂起/卡顿跨过多个周期，从当前重新对齐，不补跑假样本。
                deadline = now_mono + interval
            self.stop_flag.wait(max(0, deadline - now_mono))


# --------------------------------------------------------------------------
# 查询
# --------------------------------------------------------------------------

def q_daily(con, days, scope=None):
    if scope:
        rows = con.execute(
            "SELECT day,rx,tx,rx_pkts,tx_pkts FROM daily WHERE scope=? ORDER BY day DESC LIMIT ?",
            (scope, days),
        ).fetchall()
        return [dict(r) for r in rows][::-1]
    rows = con.execute(
        "SELECT day, SUM(rx) rx, SUM(tx) tx, SUM(rx_pkts) rx_pkts, SUM(tx_pkts) tx_pkts "
        "FROM daily WHERE scope LIKE 'node:%' GROUP BY day ORDER BY day DESC LIMIT ?",
        (days,),
    ).fetchall()
    return [dict(r) for r in rows][::-1]


def q_totals(con):
    rows = con.execute("SELECT scope,rx,tx,rx_pkts,tx_pkts FROM totals").fetchall()
    return {r["scope"]: dict(r) for r in rows}


def q_rate(con, conf, window=None):
    """最近有效样本的加权平均速率（字节 / 真实采样耗时）。"""
    iv = max(1, int(conf.get("interval", 2)))
    window = window or max(8, iv * 4)
    row = con.execute("SELECT MAX(ts) AS m FROM samples WHERE valid=1 AND duration_ms>0").fetchone()
    latest = row["m"] if row and row["m"] else 0
    if not latest or int(time.time()) - latest > max(15, iv * 4):
        return {}
    rows = con.execute(
        "SELECT scope, SUM(rx) rx, SUM(tx) tx, SUM(duration_ms) dur FROM samples "
        "WHERE valid=1 AND duration_ms>0 AND ts>? AND ts<=? GROUP BY scope",
        (latest - window, latest),
    ).fetchall()
    out = {}
    for r in rows:
        sec = max(0.001, r["dur"] / 1000.0)
        out[r["scope"]] = {"rx": r["rx"] / sec, "tx": r["tx"] / sec}
    return out


def build_summary(conf, con, collector=None):
    nodes = load_nodes(conf)
    totals = q_totals(con)
    rates = q_rate(con, conf)
    conns = count_conns(nodes)
    today = today_str(conf)
    today_rows = {
        r["scope"]: dict(r)
        for r in con.execute(
            "SELECT scope,rx,tx,rx_pkts,tx_pkts FROM daily WHERE day=?", (today,)
        ).fetchall()
    }
    zero = {"rx": 0, "tx": 0, "rx_pkts": 0, "tx_pkts": 0}

    node_list = []
    agg_today = dict(zero)
    agg_total = dict(zero)
    for n in nodes:
        scope = "node:%s" % n["id"]
        t = today_rows.get(scope, zero)
        a = totals.get(scope, zero)
        r = rates.get(scope, {"rx": 0, "tx": 0})
        c = conns.get(n["id"], {"tcp": None, "udp": None})
        node_list.append({
            "id": n["id"],
            "name": n.get("name") or n.get("type") or ("node%s" % n["id"]),
            "type": n.get("type", ""),
            "port": n.get("port"),
            "ports": n.get("ports", ""),
            "link": n.get("link", ""),
            "today": {k: t.get(k, 0) for k in zero},
            "total": {k: a.get(k, 0) for k in zero},
            "rate": r,
            "conns": c["tcp"],           # 兼容旧字段
            "conns_tcp": c["tcp"],
            "conns_udp": c["udp"],
        })
        for k in zero:
            agg_today[k] += t.get(k, 0)
            agg_total[k] += a.get(k, 0)

    sys_today = today_rows.get("system", zero)
    sys_total = totals.get("system", zero)
    return {
        "now": int(time.time()),
        "day": today,
        "tz": conf.get("tz") or time.tzname[0],
        "interval": conf.get("interval", 5),
        "backend": (collector.backend.name if collector else detect_backend(conf)),
        "healthy": bool(collector and not collector.last_error),
        "error": (collector.last_error if collector else ""),
        "last_sample": (collector.last_ok_ts if collector else 0),
        "nodes": node_list,
        "today": agg_today,
        "total": agg_total,
        "system_today": {k: sys_today.get(k, 0) for k in zero},
        "system_total": {k: sys_total.get(k, 0) for k in zero},
        "rate_total": {
            "rx": sum(v["rx"] for k, v in rates.items() if k.startswith("node:")),
            "tx": sum(v["tx"] for k, v in rates.items() if k.startswith("node:")),
        },
        "rate_known": bool(rates),
        "conns_total": sum(v["tcp"] for v in conns.values() if v["tcp"]),
        "conns_udp_total": sum(v["udp"] for v in conns.values() if v["udp"]),
    }


def build_live(conf, con, collector=None):
    """
    轻量实时端点：只算当前速率 + 连接数，不碰 daily/totals，
    供前端高频（~2s）轮询，避免用重的 /api/summary 拖慢实时读数。
    """
    nodes = load_nodes(conf)
    rates = q_rate(con, conf)
    conns = count_conns(nodes)
    node_live = []
    for n in nodes:
        scope = "node:%s" % n["id"]
        r = rates.get(scope, {"rx": 0, "tx": 0})
        c = conns.get(n["id"], {"tcp": None, "udp": None})
        node_live.append({"id": n["id"], "rate": r,
                          "conns": c["tcp"], "conns_tcp": c["tcp"], "conns_udp": c["udp"]})
    rx = sum(v["rx"] for k, v in rates.items() if k.startswith("node:"))
    tx = sum(v["tx"] for k, v in rates.items() if k.startswith("node:"))
    return {
        "now": int(time.time()),
        "healthy": bool(collector and not collector.last_error),
        "error": collector.last_error if collector else "",
        "last_sample": collector.last_ok_ts if collector else 0,
        "interval": conf.get("interval", 5),
        "rate_known": bool(rates),
        "rate_total": {"rx": rx, "tx": tx},
        "conns_total": sum(v["tcp"] for v in conns.values() if v["tcp"]),
        "conns_udp_total": sum(v["udp"] for v in conns.values() if v["udp"]),
        "nodes": node_live,
    }


# --------------------------------------------------------------------------
# HTTP 服务
# --------------------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):
    server_version = "sbx-panel/1.0"
    conf = None
    collector = None

    def log_message(self, fmt, *args):
        pass

    # ---- 工具 ----
    def _send(self, code, body, ctype="application/json; charset=utf-8", extra=None):
        if isinstance(body, str):
            body = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            with contextlib.suppress(BrokenPipeError, ConnectionResetError):
                self.wfile.write(body)

    def _json(self, obj, code=200):
        self._send(code, json.dumps(obj, ensure_ascii=False, separators=(",", ":")))

    def _token(self):
        return (self.conf.get("token") or "").strip()

    def _authorized(self, qs):
        token = self._token()
        if not token:
            return True
        given = ""
        auth = self.headers.get("Authorization", "")
        if auth.startswith("Bearer "):
            given = auth[7:]
        if not given:
            given = (qs.get("token") or [""])[0]
        if not given:
            cookie = self.headers.get("Cookie", "")
            m = re.search(r"(?:^|;\s*)sbx_token=([^;]+)", cookie)
            if m:
                given = urllib.parse.unquote(m.group(1))
        return hmac.compare_digest(given, token)

    def _serve_file(self, path, ctype):
        try:
            with open(path, "rb") as f:
                data = f.read()
        except OSError:
            self._send(404, "not found", "text/plain; charset=utf-8")
            return
        self._send(200, data, ctype)

    # ---- 路由 ----
    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        route = parsed.path.rstrip("/") or "/"
        qs = urllib.parse.parse_qs(parsed.query)

        if route == "/healthz":
            self._json({"ok": True})
            return

        if route in ("/", "/index.html"):
            if not self._authorized(qs):
                self._serve_file(os.path.join(self.conf["web_root"], "login.html"),
                                 "text/html; charset=utf-8")
                return
            extra = {}
            tok = (qs.get("token") or [""])[0]
            if tok and self._token() and hmac.compare_digest(tok, self._token()):
                extra["Set-Cookie"] = ("sbx_token=%s; Path=/; Max-Age=2592000; HttpOnly; SameSite=Lax"
                                       % urllib.parse.quote(tok))
            try:
                with open(os.path.join(self.conf["web_root"], "index.html"), "rb") as f:
                    data = f.read()
            except OSError:
                self._send(500, "web assets missing", "text/plain; charset=utf-8")
                return
            self._send(200, data, "text/html; charset=utf-8", extra)
            return

        if route in ("/app.js", "/style.css"):
            ctype = "application/javascript; charset=utf-8" if route.endswith(".js") \
                else "text/css; charset=utf-8"
            self._serve_file(os.path.join(self.conf["web_root"], route.lstrip("/")), ctype)
            return

        if not route.startswith("/api/"):
            self._send(404, "not found", "text/plain; charset=utf-8")
            return

        if not self._authorized(qs):
            self._json({"error": "unauthorized"}, 401)
            return

        con = db_connect(self.conf)
        try:
            if route == "/api/summary":
                self._json(build_summary(self.conf, con, self.collector))
            elif route == "/api/live":
                self._json(build_live(self.conf, con, self.collector))
            elif route == "/api/daily":
                days = min(365, max(1, int((qs.get("days") or ["30"])[0])))
                scope = (qs.get("scope") or [""])[0] or None
                self._json({"days": q_daily(con, days, scope)})
            elif route == "/api/nodes":
                self._json({"nodes": load_nodes(self.conf)})
            elif route == "/api/export":
                rows = con.execute(
                    "SELECT day,scope,rx,tx,rx_pkts,tx_pkts FROM daily ORDER BY day,scope"
                ).fetchall()
                out = ["day,scope,rx_bytes,tx_bytes,rx_pkts,tx_pkts"]
                out += [",".join(str(r[k]) for k in r.keys()) for r in rows]
                self._send(200, "\n".join(out) + "\n", "text/csv; charset=utf-8",
                           {"Content-Disposition": "attachment; filename=sbx-traffic.csv"})
            else:
                self._json({"error": "not found"}, 404)
        except Exception as e:
            self._json({"error": str(e)}, 500)
        finally:
            con.close()


def cmd_serve(conf):
    collector = Collector(conf)
    collector.start()
    Handler.conf = conf
    Handler.collector = collector
    addr = (conf.get("listen", "0.0.0.0"), int(conf.get("port", 8080)))
    if not (conf.get("token") or "").strip() and addr[0] not in ("127.0.0.1", "::1"):
        log("警告: 面板监听在非本机地址且未设置 token, 任何人都能读取统计数据")
    try:
        httpd = ThreadingHTTPServer(addr, Handler)
    except OSError as e:
        collector.stop_flag.set()
        log("无法监听 %s:%d — %s" % (addr[0], addr[1], e))
        log("端口可能已被占用，请在面板设置中更换端口，或先停止旧进程")
        return 1
    httpd.daemon_threads = True
    log("面板已启动 http://%s:%d  后端=%s  采集间隔=%ss"
        % (addr[0], addr[1], collector.backend.name, conf.get("interval")))
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        collector.stop_flag.set()
        httpd.server_close()
    return 0


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

def human(n):
    n = float(n or 0)
    for unit in ("B", "KiB", "MiB", "GiB", "TiB", "PiB"):
        if abs(n) < 1024 or unit == "PiB":
            return "%.2f %s" % (n, unit)
        n /= 1024


def cmd_once(conf):
    c = Collector(conf)
    c.tick()
    print("采集完成")


def cmd_show(conf):
    con = db_connect(conf)
    s = build_summary(conf, con, None)
    print("后端: %s   日期: %s   时区: %s" % (s["backend"], s["day"], s["tz"]))
    print("-" * 68)
    print("%-18s %12s %12s %12s %12s" % ("节点", "今日↑", "今日↓", "累计↑", "累计↓"))
    for n in s["nodes"]:
        print("%-18s %12s %12s %12s %12s" % (
            (n["name"][:16]), human(n["today"]["rx"]), human(n["today"]["tx"]),
            human(n["total"]["rx"]), human(n["total"]["tx"])))
    print("-" * 68)
    print("%-18s %12s %12s %12s %12s" % (
        "合计", human(s["today"]["rx"]), human(s["today"]["tx"]),
        human(s["total"]["rx"]), human(s["total"]["tx"])))
    con.close()


def cmd_daily(conf, days=14):
    con = db_connect(conf)
    print("%-12s %14s %14s %14s" % ("日期", "上传", "下载", "合计"))
    for row in q_daily(con, days):
        print("%-12s %14s %14s %14s" % (
            row["day"], human(row["rx"]), human(row["tx"]), human(row["rx"] + row["tx"])))
    con.close()


def cmd_reset(conf, scope=None):
    con = db_connect(conf)
    if scope:
        con.execute("DELETE FROM daily WHERE scope=?", (scope,))
        con.execute("DELETE FROM totals WHERE scope=?", (scope,))
        con.execute("DELETE FROM samples WHERE scope=?", (scope,))
    else:
        for t in ("daily", "totals", "samples"):
            con.execute("DELETE FROM %s" % t)
    con.close()
    print("统计数据已清空" + (" (%s)" % scope if scope else ""))


# --------------------------------------------------------------------------
# 防火墙计数规则生成
# --------------------------------------------------------------------------

def parse_ports(node):
    """
    返回归一化、已排序、已合并的端口区间 [(lo, hi), ...]。
    支持 port=443, ports="20000-30000", ports="443,8443,20000-30000", ports="1000:2000"
    合并重叠/相邻区间很关键：iptables 后端一个包若命中同节点两条规则会重复计数。
    """
    spec = []
    if node.get("port"):
        spec.append(str(node["port"]))
    raw = str(node.get("ports") or "").strip()
    if raw:
        spec += [x for x in re.split(r"[,\s]+", raw) if x]
    ranges = []
    for item in spec:
        m = re.match(r"^(\d+)(?:[-:](\d+))?$", item)
        if not m:
            continue
        lo = int(m.group(1))
        hi = int(m.group(2) or lo)
        if lo > hi:
            lo, hi = hi, lo
        if 1 <= lo <= 65535 and 1 <= hi <= 65535:
            ranges.append((lo, hi))
    ranges.sort()
    merged = []
    for lo, hi in ranges:
        if merged and lo <= merged[-1][1] + 1:
            merged[-1] = (merged[-1][0], max(merged[-1][1], hi))
        else:
            merged.append((lo, hi))
    return [tuple(x) for x in merged]


def node_protocols(node):
    net = str(node.get("net") or "").lower()
    if net in ("tcp", "udp"):
        return [net]
    return ["tcp", "udp"]


# 连接数显示用的协议归属（按 inbound 实际监听的传输层）：
#   TCP 系：vless/vmess/trojan/anytls  ·  UDP 系(QUIC)：hysteria2/tuic  ·  双栈：shadowsocks
# 与 node_protocols（计流量时为稳妥默认双栈）不同——这里只影响"该显示 TCP 还是 UDP 连接数"。
CONN_PROTOS = {
    "vless": ("tcp",), "vmess": ("tcp",), "trojan": ("tcp",), "anytls": ("tcp",),
    "hysteria2": ("udp",), "tuic": ("udp",), "shadowsocks": ("tcp", "udp"),
}


def conn_protocols(node):
    t = str(node.get("type") or "").lower()
    if t in CONN_PROTOS:
        return CONN_PROTOS[t]
    # 未知类型：回落到 net 字段或双栈
    return tuple(node_protocols(node))


# --------------------------------------------------------------------------
# 连接数：读 /proc/net/{tcp,tcp6,udp,udp6} 内核 socket 表，按监听端口归属到节点
# --------------------------------------------------------------------------

TCP_PROC_FILES = ("/proc/net/tcp", "/proc/net/tcp6")
UDP_PROC_FILES = ("/proc/net/udp", "/proc/net/udp6")
TCP_ESTABLISHED = "01"     # /proc/net/tcp 里 ESTABLISHED 状态码


def _proc_local_ports(text, keep):
    """
    解析 /proc/net/{tcp,udp}(6) 文本，返回满足 keep(state, rem) 的本地端口列表。
    行格式: sl local_address rem_address st ...
      local/rem = 十六进制IP:十六进制端口
    """
    ports = []
    for line in text.splitlines()[1:]:          # 跳过表头
        parts = line.split()
        if len(parts) < 4:
            continue
        local, rem, st = parts[1], parts[2], parts[3]
        if ":" not in local:
            continue
        if not keep(st, rem):
            continue
        try:
            ports.append(int(local.rsplit(":", 1)[1], 16))
        except ValueError:
            continue
    return ports


def _rem_connected(rem):
    """远端地址非全零 = 该 socket 已与对端建立会话（TCP已连/UDP已connect）。"""
    ippart = rem.rsplit(":", 1)[0].replace("0", "")
    portpart = rem.rsplit(":", 1)[1] if ":" in rem else "0"
    return ippart != "" or portpart not in ("0", "0000")


def _count_by_port(proc_files, keep):
    """读多个 /proc 文件，聚合每个本地端口的命中数：{port: count}"""
    hits = {}
    for path in proc_files:
        try:
            with open(path, "r") as f:
                text = f.read()
        except OSError:
            continue
        for port in _proc_local_ports(text, keep):
            hits[port] = hits.get(port, 0) + 1
    return hits


def count_conns(nodes):
    """
    返回 {node_id: {"tcp": int|None, "udp": int|None}}。
    tcp：ESTABLISHED 的 TCP socket 数（仅 TCP 类协议，否则 None）。
    udp：已建立会话（远端非零）的 UDP socket 数（仅 UDP 类协议，否则 None）。
      说明：hysteria2/tuic 基于 QUIC，服务端通常单 socket 多路复用，
      因此该值反映"有对端的 UDP 会话 socket 数"，是内核可见的真实计数，
      不一定等于 QUIC 层的逻辑连接数。
    数据来自内核 /proc/net/{tcp,udp}[6]。
    """
    tcp_hits = _count_by_port(TCP_PROC_FILES, lambda st, rem: st == TCP_ESTABLISHED)
    udp_hits = _count_by_port(UDP_PROC_FILES, lambda st, rem: _rem_connected(rem))

    def sum_hits(node, hits):
        cnt = 0
        for lo, hi in parse_ports(node):
            for p, c in hits.items():
                if lo <= p <= hi:
                    cnt += c
        return cnt

    result = {}
    for n in nodes:
        protos = conn_protocols(n)
        result[n["id"]] = {
            "tcp": (sum_hits(n, tcp_hits) if "tcp" in protos else None),
            "udp": (sum_hits(n, udp_hits) if "udp" in protos else None),
        }
    return result


def count_tcp_conns(nodes):
    """向后兼容：返回 {node_id: tcp连接数 或 None}"""
    return {nid: v["tcp"] for nid, v in count_conns(nodes).items()}


def gen_nft(conf, nodes, epoch=None):
    epoch = int(epoch if epoch is not None else time.time())
    lines = ["#!/usr/sbin/nft -f",
             "# 由 sbx 自动生成，请勿手工编辑",
             "table inet %s" % NFT_TABLE,          # 幂等：先确保存在再删除，避免首次报错
             "delete table inet %s" % NFT_TABLE,
             "table inet %s {" % NFT_TABLE]
    counters = ["sbx_epoch_%d" % epoch, "sbx_sys_i", "sbx_sys_o"]
    for n in nodes:
        counters += ["sbx_n%s_i" % n["id"], "sbx_n%s_o" % n["id"]]
    for c in counters:
        lines.append("    counter %s { }" % c)

    def port_set(ranges):
        parts = [(str(lo) if lo == hi else "%d-%d" % (lo, hi)) for lo, hi in ranges]
        return "{ %s }" % ", ".join(parts)

    for chain, hook, prio, iface_kw, dir_kw, suffix in (
        ("sbx_in", "input", 300, "iifname", "dport", "i"),
        ("sbx_out", "output", 300, "oifname", "sport", "o"),
    ):
        lines.append("    chain %s {" % chain)
        lines.append("        type filter hook %s priority %d; policy accept;" % (hook, prio))
        lines.append('        %s "lo" return' % iface_kw)
        lines.append("        counter name sbx_sys_%s" % suffix)
        for n in nodes:
            ranges = parse_ports(n)
            if not ranges:
                continue
            for proto in node_protocols(n):
                lines.append("        %s %s %s counter name sbx_n%s_%s"
                             % (proto, dir_kw, port_set(ranges), n["id"], suffix))
        lines.append("    }")
    lines.append("}")
    return "\n".join(lines) + "\n"


def gen_iptables(conf, nodes, epoch=None):
    epoch = int(epoch if epoch is not None else time.time())
    sh = ["#!/bin/sh",
          "# 由 sbx 自动生成，请勿手工编辑",
          "# 用法: sh iptables.sh apply|clear",
          "IN=%s" % IPT_CHAIN_IN,
          "OUT=%s" % IPT_CHAIN_OUT,
          "",
          "clear_one() {",
          '  B="$1"',
          '  "$B" -w -D INPUT -j "$IN" 2>/dev/null',
          '  "$B" -w -D OUTPUT -j "$OUT" 2>/dev/null',
          '  "$B" -w -F "$IN" 2>/dev/null; "$B" -w -X "$IN" 2>/dev/null',
          '  "$B" -w -F "$OUT" 2>/dev/null; "$B" -w -X "$OUT" 2>/dev/null',
          "}",
          "",
          "apply_one() {",
          '  B="$1"',
          '  command -v "$B" >/dev/null 2>&1 || return 0',
          '  clear_one "$B"',
          '  "$B" -w -N "$IN"  2>/dev/null',
          '  "$B" -w -N "$OUT" 2>/dev/null',
          '  "$B" -w -I INPUT 1 -j "$IN"',
          '  "$B" -w -I OUTPUT 1 -j "$OUT"',
          '  "$B" -w -A "$IN"  -m comment --comment "sbx:epoch:%d"' % epoch,
          '  "$B" -w -A "$IN"  -i lo -j RETURN',
          '  "$B" -w -A "$OUT" -o lo -j RETURN',
          '  "$B" -w -A "$IN"  -m comment --comment "sbx:sys:i"',
          '  "$B" -w -A "$OUT" -m comment --comment "sbx:sys:o"']
    for n in nodes:
        for lo, hi in parse_ports(n):
            pspec = str(lo) if lo == hi else "%d:%d" % (lo, hi)
            for proto in node_protocols(n):
                sh.append('  "$B" -w -A "$IN"  -p %s --dport %s -m comment --comment "sbx:n%s:i"'
                          % (proto, pspec, n["id"]))
                sh.append('  "$B" -w -A "$OUT" -p %s --sport %s -m comment --comment "sbx:n%s:o"'
                          % (proto, pspec, n["id"]))
    sh += ["}",
           "",
           'case "${1:-apply}" in',
           "  apply) apply_one iptables; apply_one ip6tables ;;",
           "  clear) clear_one iptables; clear_one ip6tables ;;",
           '  *) echo "用法: $0 apply|clear" >&2; exit 2 ;;',
           "esac"]
    return "\n".join(sh) + "\n"


def cmd_rules(conf, epoch=None):
    nodes = load_nodes(conf)
    epoch = int(epoch if epoch is not None else time.time())
    os.makedirs(APP_DIR, exist_ok=True)
    with open(conf["nft_conf"], "w", encoding="utf-8") as f:
        f.write(gen_nft(conf, nodes, epoch))
    with open(conf["ipt_script"], "w", encoding="utf-8") as f:
        f.write(gen_iptables(conf, nodes, epoch))
    os.chmod(conf["ipt_script"], 0o755)
    print("已生成计数规则: %s / %s (%d 个节点, 世代 %d)"
          % (conf["nft_conf"], conf["ipt_script"], len(nodes), epoch))
    return epoch


def cmd_apply(conf):
    """
    先强制采样一次（把旧规则的计数落库），再重建规则。
    新规则带新的 epoch 标记，采集器识别到世代变化后从零基线继续，做到零丢计、零重复。
    """
    try:
        Collector(conf).tick()
    except LookupError:
        pass
    except Exception as e:
        log("重建前采样失败(可忽略):", e)
    cmd_rules(conf)
    backend = detect_backend(conf)
    if backend == "nft":
        rc, _, err = run_cmd(["nft", "-f", conf["nft_conf"]])
        if rc != 0:
            log("nft 应用失败, 尝试 iptables:", err.strip())
            backend = "iptables"
        else:
            print("nftables 计数规则已生效")
    if backend == "iptables":
        rc, _, err = run_cmd(["sh", conf["ipt_script"], "apply"])
        if rc != 0:
            log("iptables 应用失败:", err.strip())
            return 1
        print("iptables 计数规则已生效")
    return 0


def cmd_clear(conf):
    try:
        Collector(conf).tick()
    except Exception:
        pass
    if shutil.which("nft"):
        run_cmd(["nft", "delete", "table", "inet", NFT_TABLE])
    if os.path.exists(conf["ipt_script"]):
        run_cmd(["sh", conf["ipt_script"], "clear"])
    print("计数规则已移除（历史统计数据保留）")
    return 0


def cmd_selftest(conf):
    """验证内核计数器确实存在且在增长"""
    b = make_backend(conf)
    print("后端:", b.name)
    try:
        first = b.read()
    except LookupError as e:
        print("失败: 计数器不存在 (%s)，请先执行 sbx apply" % e)
        return 1
    names = sorted(k for k in first if parse_counter_name(k))
    print("识别到 %d 个计数器: %s" % (len(names), ", ".join(names) or "无"))
    if not names:
        return 1
    total0 = sum(v[0] for v in first.values())
    time.sleep(3)
    second = b.read()
    total1 = sum(v[0] for v in second.values())
    print("3 秒内计数变化: %s" % human(total1 - total0))
    print("自检通过: 计数器可读" + ("且有流量" if total1 > total0 else "（当前无流量，属正常）"))
    return 0



def main():
    conf = load_conf()
    args = sys.argv[1:]
    cmd = args[0] if args else "serve"
    if cmd == "serve":
        sys.exit(cmd_serve(conf) or 0)
    elif cmd == "once":
        cmd_once(conf)
    elif cmd == "show":
        cmd_show(conf)
    elif cmd == "daily":
        cmd_daily(conf, int(args[1]) if len(args) > 1 else 14)
    elif cmd == "reset":
        cmd_reset(conf, args[1] if len(args) > 1 else None)
    elif cmd == "rules":
        cmd_rules(conf)
    elif cmd == "apply":
        sys.exit(cmd_apply(conf))
    elif cmd == "clear":
        sys.exit(cmd_clear(conf))
    elif cmd == "selftest":
        sys.exit(cmd_selftest(conf))
    else:
        print("用法: panel.py [serve|once|show|daily [N]|rules|apply|clear|selftest|reset [scope]]")
        sys.exit(2)


if __name__ == "__main__":
    main()
