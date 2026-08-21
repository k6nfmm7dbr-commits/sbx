#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
nodes_tool.py — 节点增删改与分享链接生成

把 sing-box 配置的结构性修改集中在这里，避免在 shell 里拼 JSON。
所有写操作都先生成候选文件，由外层 sbx.sh 调用 `sing-box check` 校验后才落盘。

用法:
  add <type> --port N [...]     新增节点 -> 输出候选配置路径 + 节点 id
  remove <id>                   删除节点
  list                          列出节点
  links [id]                    输出分享链接
  port-used <port>              端口是否已被现有节点占用（退出码 0=占用）
  set-host <host>               设置分享链接使用的地址
  get-host                      读取分享链接地址
  sync                          按 nodes.json 重建 inbounds（修复错位）
"""

import argparse
import base64
import json
import os
import re
import sys
import urllib.parse

APP_DIR = os.environ.get("SBX_DIR", "/etc/sbx")
SB_CONF = os.environ.get("SBX_SB_CONF", "/etc/sing-box/config.json")
NODES_JSON = os.path.join(APP_DIR, "nodes.json")
STATE_JSON = os.path.join(APP_DIR, "state.json")
CERT_DIR = os.path.join(APP_DIR, "certs")

TAG_PREFIX = "sbx-n"

TYPES = ("vless", "vmess", "shadowsocks", "trojan", "hysteria2", "tuic", "anytls")


# ------------------------------------------------------------------ 读写
def read_json(path, default):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def write_json(path, data):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")
    os.replace(tmp, path)


def load_nodes():
    d = read_json(NODES_JSON, [])
    return d if isinstance(d, list) else []


def load_state():
    d = read_json(STATE_JSON, {})
    return d if isinstance(d, dict) else {}


def next_id(nodes):
    used = {int(n["id"]) for n in nodes if str(n.get("id", "")).isdigit()}
    i = 1
    while i in used:
        i += 1
    return i


def tag_of(node_id):
    return "%s%s" % (TAG_PREFIX, node_id)


# ------------------------------------------------------------------ inbound 构造
def build_inbound(node):
    """由 nodes.json 中的一条记录生成 sing-box inbound。单一数据源，避免两边不一致。"""
    t = node["type"]
    tag = tag_of(node["id"])
    port = int(node["port"])
    base = {"type": t, "tag": tag, "listen": "::", "listen_port": port}

    if t == "vless":
        user = {"name": "u", "uuid": node["uuid"]}
        if node.get("flow"):
            user["flow"] = node["flow"]
        base["users"] = [user]
        base["tls"] = {
            "enabled": True,
            "server_name": node["sni"],
            "reality": {
                "enabled": True,
                "handshake": {"server": node["sni"], "server_port": 443},
                "private_key": node["private_key"],
                "short_id": [node["short_id"]],
            },
        }
    elif t == "vmess":
        base["users"] = [{"name": "u", "uuid": node["uuid"], "alterId": 0}]
        if node.get("path"):
            base["transport"] = {"type": "ws", "path": node["path"]}
    elif t == "shadowsocks":
        base["method"] = node["method"]
        base["password"] = node["password"]
    elif t == "trojan":
        base["users"] = [{"name": "u", "password": node["password"]}]
        base["tls"] = tls_selfsigned(node)
    elif t == "hysteria2":
        base["users"] = [{"name": "u", "password": node["password"]}]
        base["tls"] = tls_selfsigned(node, alpn=["h3"])
        if node.get("ports"):
            base["listen_port"] = port
    elif t == "tuic":
        base["users"] = [{"name": "u", "uuid": node["uuid"], "password": node["password"]}]
        base["congestion_control"] = "bbr"
        base["tls"] = tls_selfsigned(node, alpn=["h3"])
    elif t == "anytls":
        base["users"] = [{"name": "u", "password": node["password"]}]
        base["tls"] = tls_selfsigned(node)
    else:
        raise SystemExit("不支持的节点类型: %s" % t)
    return base


def tls_selfsigned(node, alpn=None):
    tls = {
        "enabled": True,
        "server_name": node["sni"],
        "certificate_path": node["cert"],
        "key_path": node["key"],
    }
    if alpn:
        tls["alpn"] = alpn
    return tls


def rebuild_config(nodes, src=SB_CONF):
    """保留用户其它字段，只重建由 sbx 管理的 inbounds。"""
    cfg = read_json(src, None)
    if cfg is None:
        raise SystemExit("读取 sing-box 配置失败: %s" % src)
    others = [i for i in cfg.get("inbounds", [])
              if not str(i.get("tag", "")).startswith(TAG_PREFIX)]
    cfg["inbounds"] = others + [build_inbound(n) for n in nodes]
    cfg.setdefault("outbounds", [{"type": "direct", "tag": "direct"}])
    cfg.setdefault("route", {}).setdefault("final", "direct")
    return cfg


def write_candidate(cfg):
    path = SB_CONF + ".candidate"
    write_json(path, cfg)
    return path


# ------------------------------------------------------------------ 分享链接
def share_host():
    st = load_state()
    h = st.get("host") or ""
    return h


def uri_host(h):
    return "[%s]" % h if (":" in h and not h.startswith("[")) else h


def node_link(node, host=None):
    host = host or share_host() or "SERVER_IP"
    h = uri_host(host)
    t = node["type"]
    name = urllib.parse.quote(node.get("name") or t)
    port = node["port"]

    if t == "vless":
        q = {
            "encryption": "none", "security": "reality", "type": "tcp",
            "sni": node["sni"], "fp": "chrome",
            "pbk": node["public_key"], "sid": node["short_id"],
        }
        if node.get("flow"):
            q["flow"] = node["flow"]
        return "vless://%s@%s:%s?%s#%s" % (
            node["uuid"], h, port, urllib.parse.urlencode(q), name)

    if t == "vmess":
        obj = {
            "v": "2", "ps": node.get("name") or "vmess", "add": host, "port": str(port),
            "id": node["uuid"], "aid": "0", "scy": "auto",
            "net": "ws" if node.get("path") else "tcp",
            "type": "none", "host": "", "path": node.get("path", ""), "tls": "",
        }
        raw = json.dumps(obj, ensure_ascii=False, separators=(",", ":")).encode()
        return "vmess://" + base64.b64encode(raw).decode()

    if t == "shadowsocks":
        userinfo = base64.urlsafe_b64encode(
            ("%s:%s" % (node["method"], node["password"])).encode()).decode().rstrip("=")
        return "ss://%s@%s:%s#%s" % (userinfo, h, port, name)

    if t == "trojan":
        q = {"security": "tls", "sni": node["sni"], "allowInsecure": "1", "type": "tcp"}
        return "trojan://%s@%s:%s?%s#%s" % (
            urllib.parse.quote(node["password"]), h, port, urllib.parse.urlencode(q), name)

    if t == "hysteria2":
        q = {"sni": node["sni"], "insecure": "1"}
        if node.get("ports"):
            q["mport"] = node["ports"]
        return "hysteria2://%s@%s:%s?%s#%s" % (
            urllib.parse.quote(node["password"]), h, port, urllib.parse.urlencode(q), name)

    if t == "tuic":
        q = {"sni": node["sni"], "alpn": "h3", "congestion_control": "bbr", "allow_insecure": "1"}
        return "tuic://%s:%s@%s:%s?%s#%s" % (
            node["uuid"], urllib.parse.quote(node["password"]), h, port,
            urllib.parse.urlencode(q), name)

    if t == "anytls":
        q = {"sni": node["sni"], "insecure": "1"}
        return "anytls://%s@%s:%s?%s#%s" % (
            urllib.parse.quote(node["password"]), h, port, urllib.parse.urlencode(q), name)

    return ""


def subscription(nodes, host=None):
    links = [node_link(n, host) for n in nodes]
    body = "\n".join(l for l in links if l)
    return base64.b64encode(body.encode()).decode()


# ------------------------------------------------------------------ 命令
def cmd_add(args):
    nodes = load_nodes()
    nid = next_id(nodes)
    node = {"id": nid, "type": args.type, "port": int(args.port),
            "name": args.name or ("%s-%d" % (args.type, nid))}

    for key in ("uuid", "password", "method", "sni", "path", "flow",
                "private_key", "public_key", "short_id", "ports", "net"):
        val = getattr(args, key, None)
        if val:
            node[key] = val
    if args.type in ("trojan", "hysteria2", "tuic", "anytls"):
        node["cert"] = os.path.join(CERT_DIR, "cert.pem")
        node["key"] = os.path.join(CERT_DIR, "key.pem")
    if args.type in ("hysteria2", "tuic"):
        node["net"] = "udp"

    nodes.append(node)
    cfg = rebuild_config(nodes)
    cand = write_candidate(cfg)
    # nodes.json 由外层在校验通过后调用 commit 写入
    write_json(NODES_JSON + ".candidate", nodes)
    print(json.dumps({"id": nid, "candidate": cand,
                      "nodes_candidate": NODES_JSON + ".candidate"}, ensure_ascii=False))
    return 0


def cmd_commit(args):
    """校验通过后：把候选 nodes.json 提升为正式文件"""
    cand = NODES_JSON + ".candidate"
    if os.path.exists(cand):
        os.replace(cand, NODES_JSON)
    print("ok")
    return 0


def cmd_rollback(args):
    for p in (NODES_JSON + ".candidate", SB_CONF + ".candidate"):
        if os.path.exists(p):
            os.remove(p)
    print("ok")
    return 0


def cmd_remove(args):
    nodes = load_nodes()
    keep = [n for n in nodes if str(n["id"]) != str(args.id)]
    if len(keep) == len(nodes):
        raise SystemExit("未找到节点 id=%s" % args.id)
    cfg = rebuild_config(keep)
    cand = write_candidate(cfg)
    write_json(NODES_JSON + ".candidate", keep)
    print(json.dumps({"candidate": cand, "nodes_candidate": NODES_JSON + ".candidate"},
                     ensure_ascii=False))
    return 0


def cmd_sync(args):
    nodes = load_nodes()
    cfg = rebuild_config(nodes)
    cand = write_candidate(cfg)
    write_json(NODES_JSON + ".candidate", nodes)
    print(json.dumps({"candidate": cand, "nodes_candidate": NODES_JSON + ".candidate"},
                     ensure_ascii=False))
    return 0


def cmd_list(args):
    nodes = load_nodes()
    if args.json:
        print(json.dumps(nodes, ensure_ascii=False, indent=2))
        return 0
    if not nodes:
        print("(暂无节点)")
        return 0
    print("%-4s %-14s %-14s %-8s %s" % ("ID", "名称", "类型", "端口", "端口范围"))
    for n in nodes:
        print("%-4s %-14s %-14s %-8s %s" % (
            n["id"], (n.get("name") or "")[:14], n["type"], n["port"], n.get("ports", "")))
    return 0


def cmd_links(args):
    nodes = load_nodes()
    if args.id:
        nodes = [n for n in nodes if str(n["id"]) == str(args.id)]
        if not nodes:
            raise SystemExit("未找到节点 id=%s" % args.id)
    if args.sub:
        print(subscription(nodes, args.host))
        return 0
    for n in nodes:
        print("### %s (%s, 端口 %s)" % (n.get("name"), n["type"], n["port"]))
        print(node_link(n, args.host))
        print()
    return 0


def cmd_port_used(args):
    nodes = load_nodes()
    p = int(args.port)
    for n in nodes:
        if int(n["port"]) == p:
            print("used by node %s" % n["id"])
            return 0
        rng = str(n.get("ports") or "")
        for part in re.split(r"[,\s]+", rng):
            m = re.match(r"^(\d+)[-:](\d+)$", part)
            if m and int(m.group(1)) <= p <= int(m.group(2)):
                print("in range of node %s" % n["id"])
                return 0
            if part.isdigit() and int(part) == p:
                print("used by node %s" % n["id"])
                return 0
    return 1


def cmd_set_host(args):
    st = load_state()
    st["host"] = args.host
    write_json(STATE_JSON, st)
    print(args.host)
    return 0


def cmd_get_host(args):
    print(share_host())
    return 0


def main():
    ap = argparse.ArgumentParser(prog="nodes_tool.py", add_help=True)
    sub = ap.add_subparsers(dest="cmd", required=True)

    a = sub.add_parser("add")
    a.add_argument("type", choices=TYPES)
    a.add_argument("--port", required=True)
    a.add_argument("--name")
    a.add_argument("--uuid")
    a.add_argument("--password")
    a.add_argument("--method")
    a.add_argument("--sni")
    a.add_argument("--path")
    a.add_argument("--flow")
    a.add_argument("--private-key", dest="private_key")
    a.add_argument("--public-key", dest="public_key")
    a.add_argument("--short-id", dest="short_id")
    a.add_argument("--ports")
    a.add_argument("--net")
    a.set_defaults(func=cmd_add)

    r = sub.add_parser("remove"); r.add_argument("id"); r.set_defaults(func=cmd_remove)
    s = sub.add_parser("sync"); s.set_defaults(func=cmd_sync)
    c = sub.add_parser("commit"); c.set_defaults(func=cmd_commit)
    rb = sub.add_parser("rollback"); rb.set_defaults(func=cmd_rollback)

    l = sub.add_parser("list"); l.add_argument("--json", action="store_true")
    l.set_defaults(func=cmd_list)

    k = sub.add_parser("links")
    k.add_argument("id", nargs="?")
    k.add_argument("--host")
    k.add_argument("--sub", action="store_true")
    k.set_defaults(func=cmd_links)

    p = sub.add_parser("port-used"); p.add_argument("port"); p.set_defaults(func=cmd_port_used)
    sh = sub.add_parser("set-host"); sh.add_argument("host"); sh.set_defaults(func=cmd_set_host)
    gh = sub.add_parser("get-host"); gh.set_defaults(func=cmd_get_host)

    args = ap.parse_args()
    sys.exit(args.func(args))


if __name__ == "__main__":
    main()
