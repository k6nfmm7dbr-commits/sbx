#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
build.py — 生成发布版 sbx.sh

v3.0.0 起 Go 后端（sbx-core）与前端资源不再内嵌进脚本：
  * 前端由 sbx-core 通过 //go:embed 内嵌（升级二进制即升级 UI）
  * sbx-core 二进制由 GitHub Releases 按 CPU 架构分发，安装器校验 SHA256
因此发布脚本就是 installer-template.sh 本体，build.py 只做拷贝与提示。
"""

import hashlib
import os
import shutil

ROOT = os.path.dirname(os.path.abspath(__file__))
TEMPLATE = os.path.join(ROOT, "installer-template.sh")
OUT = os.path.join(ROOT, "sbx.sh")


def main():
    shutil.copyfile(TEMPLATE, OUT)
    os.chmod(OUT, 0o755)
    size = os.path.getsize(OUT)
    with open(OUT, "rb") as f:
        sha = hashlib.sha256(f.read()).hexdigest()
    print("已生成 %s (%d bytes, sha256 %s...)" % (OUT, size, sha[:16]))
    print("提示: sbx-core 二进制请用 scripts/build-release.sh 构建并上传 GitHub Releases")


if __name__ == "__main__":
    main()
