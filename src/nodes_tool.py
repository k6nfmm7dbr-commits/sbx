#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
nodes_tool.py — 节点增删改与分享链接生成

把 sing-box 配置的结构性修改集中在这里，避免在 shell 里拼 JSON。
所有写操作都先生成候选文件，由外层 sbx.sh 调用 `sing-box check` 校验后才落盘。

用法:
  add <type> --port N [...]     新增节点 -> 输出候选配置路径 + 节点 id
  edit <id> [--port|--sni]      修改已装节点
  remove <id>                   删除节点
  list                          列出节点
  count                         输出节点数量
  last                          输出最后一个节点 id
  info <id>                     输出节点 type/sni/port（制表符分隔）
  links [id] [--host6 IP]       输出分享链接（有 IPv6 则附 IPv6 版）
  port-used <port>              端口是否已被现有节点占用（退出码 0=占用）
  set-host <host>               设置分享链接使用的地址（IPv4/域名）
  set-host6 [host]              设置 IPv6 分享地址（留空=清除）
  get-host / get-host6          读取分享地址
  sync                          按 nodes.json 重建 inbounds（修复错位）
"""

import argparse
import base64
import json
import os
import sys
import urllib.parse

APP_DIR = os.environ.get("SBX_DIR", "/etc/sbx")
SB_CONF = os.environ.get("SBX_SB_CONF", "/etc/sing-box/config.json")
NODES_JSON = os.path.join(APP_DIR, "nodes.json")
STATE_JSON = os.path.join(APP_DIR, "state.json")
CERT_DIR = os.path.join(APP_DIR, "certs")

TAG_PREFIX = "sbx-n"

TYPES = ("vless", "shadowsocks", "trojan", "anytls")


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
    """分配节点 ID：单调递增、永不回收。

    旧实现取最小空闲 id，删除节点后新节点会复用该 id，导致数据库里
    node:<id> 的累计/每日流量被混入无关节点。这里把分配游标持久化到
    state.json（next_node_id），并兜底兼容"已有节点 id 更大"的手工数据。
    """
    st = load_state()
    try:
        base = int(st.get("next_node_id", 0))
    except (TypeError, ValueError):
        base = 0
    used_max = 0
    for n in nodes:
        try:
            used_max = max(used_max, int(n["id"]))
        except (TypeError, ValueError):
            continue
    nid = max(base, used_max) + 1
    st["next_node_id"] = nid
    write_json(STATE_JSON, st)
    return nid


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
    elif t == "shadowsocks":
        base["method"] = node["method"]
        base["password"] = node["password"]
    elif t == "trojan":
        base["users"] = [{"name": "u", "password": node["password"]}]
        base["tls"] = tls_selfsigned(node)
    elif t == "anytls":
        base["users"] = [{"name": "u", "password": node["password"]}]
        base["tls"] = tls_selfsigned(node)
    else:
        raise SystemExit("不支持的节点类型: %s" % t)
    return base


def tls_selfsigned(node):
    return {
        "enabled": True,
        "server_name": node["sni"],
        "certificate_path": node["cert"],
        "key_path": node["key"],
    }


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
    return st.get("host") or ""


def share_host6():
    st = load_state()
    return st.get("host6") or ""


def uri_host(h):
    return "[%s]" % h if (":" in h and not h.startswith("[")) else h


def node_link(node, host=None, label_suffix=""):
    host = host or share_host() or "SERVER_IP"
    h = uri_host(host)
    t = node["type"]
    name = urllib.parse.quote((node.get("name") or t) + label_suffix)
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

    if t == "shadowsocks":
        userinfo = base64.urlsafe_b64encode(
            ("%s:%s" % (node["method"], node["password"])).encode()).decode().rstrip("=")
        return "ss://%s@%s:%s#%s" % (userinfo, h, port, name)

    if t == "trojan":
        q = {"security": "tls", "sni": node["sni"], "allowInsecure": "1", "type": "tcp"}
        return "trojan://%s@%s:%s?%s#%s" % (
            urllib.parse.quote(node["password"]), h, port, urllib.parse.urlencode(q), name)

    if t == "anytls":
        q = {"sni": node["sni"], "insecure": "1"}
        return "anytls://%s@%s:%s?%s#%s" % (
            urllib.parse.quote(node["password"]), h, port, urllib.parse.urlencode(q), name)

    return ""


# ------------------------------------------------------------------ 命令
def cmd_add(args):
    nodes = load_nodes()
    nid = next_id(nodes)
    node = {"id": nid, "type": args.type, "port": int(args.port),
            "name": args.name or ("%s-%d" % (args.type, nid))}

    for key in ("uuid", "password", "method", "sni", "flow",
                "private_key", "public_key", "short_id"):
        val = getattr(args, key, None)
        if val:
            node[key] = val
    if args.type in ("trojan", "anytls"):
        node["cert"] = os.path.join(CERT_DIR, "cert.pem")
        node["key"] = os.path.join(CERT_DIR, "key.pem")

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


# 各类型允许修改 sni 的（reality + 自签证书类）
SNI_TYPES = ("vless", "trojan", "anytls")


def cmd_edit(args):
    """修改现有节点的端口 / SNI。只改指定字段，其余保持不变。"""
    nodes = load_nodes()
    target = None
    for n in nodes:
        if str(n["id"]) == str(args.id):
            target = n
            break
    if target is None:
        raise SystemExit("未找到节点 id=%s" % args.id)

    changed = []
    if args.port:
        newp = int(args.port)
        if not (1 <= newp <= 65535):
            raise SystemExit("端口需在 1-65535")
        # 不能和其它节点端口冲突
        for n in nodes:
            if str(n["id"]) == str(target["id"]):
                continue
            if int(n.get("port", 0)) == newp:
                raise SystemExit("端口 %d 已被节点 %s 使用" % (newp, n["id"]))
        target["port"] = newp
        changed.append("端口→%d" % newp)

    if args.sni:
        if target["type"] not in SNI_TYPES:
            raise SystemExit("%s 类型节点没有 SNI 可改" % target["type"])
        target["sni"] = args.sni
        changed.append("SNI→%s" % args.sni)

    if not changed:
        raise SystemExit("未指定要修改的内容（--port / --sni）")

    cfg = rebuild_config(nodes)
    cand = write_candidate(cfg)
    write_json(NODES_JSON + ".candidate", nodes)
    print(json.dumps({"id": target["id"], "changed": changed, "candidate": cand,
                      "nodes_candidate": NODES_JSON + ".candidate"}, ensure_ascii=False))
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
    print("%-4s %-16s %-14s %-8s" % ("ID", "名称", "类型", "端口"))
    for n in nodes:
        print("%-4s %-16s %-14s %-8s" % (
            n["id"], (n.get("name") or "")[:16], n["type"], n["port"]))
    return 0


def cmd_count(args):
    """输出节点数量，供外层 shell 菜单显示用。"""
    print(len(load_nodes()))
    return 0


def cmd_last(args):
    """输出最后一个节点的 id（无节点输出空行），供"新增后立即展示链接"用。"""
    nodes = load_nodes()
    print(nodes[-1]["id"] if nodes else "")
    return 0


def cmd_info(args):
    """输出单个节点的 type/sni/port（制表符分隔），供 shell 编辑菜单读取。"""
    for n in load_nodes():
        if str(n["id"]) == str(args.id):
            print("%s\t%s\t%s" % (n["type"], n.get("sni", ""), n.get("port", "")))
            return 0
    raise SystemExit("未找到节点 id=%s" % args.id)


def cmd_links(args):
    nodes = load_nodes()
    if args.id:
        nodes = [n for n in nodes if str(n["id"]) == str(args.id)]
        if not nodes:
            raise SystemExit("未找到节点 id=%s" % args.id)
    host6 = args.host6 if args.host6 is not None else share_host6()
    for n in nodes:
        print("### %s (%s, 端口 %s)" % (n.get("name"), n["type"], n["port"]))
        print(node_link(n, args.host))
        if host6:
            print("# IPv6:")
            print(node_link(n, host6, label_suffix="-IPv6"))
        print()
    return 0


def cmd_port_used(args):
    nodes = load_nodes()
    p = int(args.port)
    for n in nodes:
        if int(n.get("port", 0)) == p:
            print("used by node %s" % n["id"])
            return 0
    return 1


def cmd_set_host(args):
    st = load_state()
    st["host"] = args.host
    write_json(STATE_JSON, st)
    print(args.host)
    return 0


def cmd_set_host6(args):
    st = load_state()
    v = (args.host or "").strip()
    if v:
        st["host6"] = v
    else:
        st.pop("host6", None)      # 空值 = 清除 IPv6
    write_json(STATE_JSON, st)
    print(v)
    return 0


def cmd_get_host(args):
    print(share_host())
    return 0


def cmd_get_host6(args):
    print(share_host6())
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
    a.add_argument("--flow")
    a.add_argument("--private-key", dest="private_key")
    a.add_argument("--public-key", dest="public_key")
    a.add_argument("--short-id", dest="short_id")
    a.set_defaults(func=cmd_add)

    r = sub.add_parser("remove"); r.add_argument("id"); r.set_defaults(func=cmd_remove)
    s = sub.add_parser("sync"); s.set_defaults(func=cmd_sync)
    c = sub.add_parser("commit"); c.set_defaults(func=cmd_commit)
    rb = sub.add_parser("rollback"); rb.set_defaults(func=cmd_rollback)

    e = sub.add_parser("edit")
    e.add_argument("id")
    e.add_argument("--port")
    e.add_argument("--sni")
    e.set_defaults(func=cmd_edit)

    l = sub.add_parser("list"); l.add_argument("--json", action="store_true")
    l.set_defaults(func=cmd_list)

    cnt = sub.add_parser("count"); cnt.set_defaults(func=cmd_count)
    last = sub.add_parser("last"); last.set_defaults(func=cmd_last)
    info = sub.add_parser("info"); info.add_argument("id"); info.set_defaults(func=cmd_info)

    k = sub.add_parser("links")
    k.add_argument("id", nargs="?")
    k.add_argument("--host")
    k.add_argument("--host6", nargs="?", const="", default=None)
    k.set_defaults(func=cmd_links)

    p = sub.add_parser("port-used"); p.add_argument("port"); p.set_defaults(func=cmd_port_used)
    sh = sub.add_parser("set-host"); sh.add_argument("host"); sh.set_defaults(func=cmd_set_host)
    gh = sub.add_parser("get-host"); gh.set_defaults(func=cmd_get_host)
    sh6 = sub.add_parser("set-host6"); sh6.add_argument("host", nargs="?", default="")
    sh6.set_defaults(func=cmd_set_host6)
    gh6 = sub.add_parser("get-host6"); gh6.set_defaults(func=cmd_get_host6)

    args = ap.parse_args()
    sys.exit(args.func(args))


if __name__ == "__main__":
    main()
