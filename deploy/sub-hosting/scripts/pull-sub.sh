#!/usr/bin/env bash
# 本地镜像订阅（用于 GitHub Actions 够不着面板的情况，或想手动同步一份）
#
#   ./pull-sub.sh https://1.2.3.4:8080 3f2a...9c sub/
#
# 产物：sub.txt / sub-singbox.json
# 注意：这两个文件里是节点密码，别提交到公开仓库。

set -euo pipefail

BASE="${1:?用法: pull-sub.sh <面板地址> <订阅令牌> [输出目录]}"
TOKEN="${2:?缺少订阅令牌}"
OUT="${3:-.}"

BASE="${BASE%/}"
mkdir -p "$OUT"

fetch() {
  local url="$1" dest="$2" tmp
  tmp="$(mktemp "${dest}.XXXXXX")"
  # -f: HTTP 错误码直接失败，避免把 404 页面当订阅存下来
  if ! curl -fsS --max-time 30 --retry 2 "$url" -o "$tmp"; then
    rm -f "$tmp"
    echo "拉取失败: $dest" >&2
    return 1
  fi
  if [ ! -s "$tmp" ]; then
    rm -f "$tmp"
    echo "内容为空: $dest" >&2
    return 1
  fi
  mv "$tmp" "$dest"
}

fetch "$BASE/sub/$TOKEN" "$OUT/sub.txt"
echo "  链接列表 $(wc -c < "$OUT/sub.txt") 字节"

fetch "$BASE/sub/$TOKEN?target=singbox" "$OUT/sub-singbox.json"
python3 -m json.tool "$OUT/sub-singbox.json" > /dev/null
echo "  sing-box 配置 $(wc -c < "$OUT/sub-singbox.json") 字节，JSON 合法"

echo "完成: $OUT"
