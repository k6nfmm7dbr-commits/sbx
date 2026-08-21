#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
build.py — 生成可直接 curl 执行的单文件发布版 sbx.sh

把 panel.py / nodes_tool.py / web 资源以 base64 内嵌进脚本，
替换掉开发用的 write_payload()，使 `bash <(curl -fsSL ...)` 无需额外下载。
"""

import base64
import gzip
import hashlib
import io
import os
import sys

ROOT = os.path.dirname(os.path.abspath(__file__))
TEMPLATE = os.path.join(ROOT, "installer-template.sh")
OUT = os.path.join(ROOT, "sbx.sh")

FILES = [
    ("src/panel.py", "$PANEL_PY", "0755"),
    ("src/nodes_tool.py", "$APP_DIR/nodes_tool.py", "0755"),
    ("web/index.html", "$WEB_DIR/index.html", "0644"),
    ("web/login.html", "$WEB_DIR/login.html", "0644"),
    ("web/app.js", "$WEB_DIR/app.js", "0644"),
    ("web/style.css", "$WEB_DIR/style.css", "0644"),
]

MARKER_START = "# ---------------------------------------------------------------- 内嵌资源"


def b64_gz(path):
    with open(path, "rb") as f:
        raw = f.read()
    buf = io.BytesIO()
    # mtime=0 让输出可重复构建
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0) as gz:
        gz.write(raw)
    return base64.b64encode(buf.getvalue()).decode(), hashlib.sha256(raw).hexdigest(), len(raw)


def wrap(s, width=200):
    return "\n".join(s[i:i + width] for i in range(0, len(s), width))


def main():
    with open(TEMPLATE, "r", encoding="utf-8") as f:
        tpl = f.read()

    idx = tpl.find(MARKER_START)
    if idx < 0:
        sys.exit("未找到内嵌资源标记")
    head = tpl[:idx]

    parts = [MARKER_START,
             "# 由 build.py 自动生成，请勿手工编辑以下内容",
             "",
             "_sbx_unpack() {  # _sbx_unpack <dest> <mode> <<< base64-gzip",
             '  local dest="$1" mode="$2"',
             '  base64 -d | gzip -d > "$dest"',
             '  chmod "$mode" "$dest"',
             "}",
             "",
             "write_payload() {",
             '  install -d -m 0755 "$APP_DIR" "$WEB_DIR"']

    total = 0
    for rel, dest, mode in FILES:
        path = os.path.join(ROOT, rel)
        data, sha, size = b64_gz(path)
        total += size
        var = "_SBX_" + rel.replace("/", "_").replace(".", "_").upper()
        parts.append("")
        parts.append("  # %s (%d bytes, sha256 %s)" % (rel, size, sha[:16]))
        parts.append('  _sbx_unpack "%s" %s <<\'%s\'' % (dest, mode, var))
        parts.append(wrap(data))
        parts.append(var)

    parts.append("}")
    parts.append("")
    parts.append('main "$@"')
    parts.append("")

    body = head + "\n".join(parts)
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as f:
        f.write(body)
    os.chmod(OUT, 0o755)

    print("已生成 %s" % OUT)
    print("  内嵌 %d 个文件, 原始共 %d 字节, 脚本大小 %d 字节"
          % (len(FILES), total, len(body.encode())))


if __name__ == "__main__":
    main()
