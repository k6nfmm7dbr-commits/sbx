```bash
(
  set -euo pipefail
  umask 077
  tmp=$(mktemp -d)
  trap 'rm -rf -- "$tmp"' EXIT
  cd "$tmp"
  repo=k6nfmm7dbr-commits/sbx
  ref=$(git ls-remote --exit-code "https://github.com/$repo.git" refs/heads/dist)
  rev=${ref%%$'\t'*}
  [[ "$rev" =~ ^[0-9a-f]{40}$ ]]
  base="https://raw.githubusercontent.com/$repo/$rev"
  curl -q --globoff --proto '=https' -fsS -o sbx.sh "$base/sbx.sh"
  curl -q --globoff --proto '=https' -fsS -o sbx.sh.sha256 "$base/sbx.sh.sha256"
  mapfile -t sums < sbx.sh.sha256
  [[ ${#sums[@]} -eq 1 ]]
  read -r hash name extra <<< "${sums[0]}"
  [[ "$hash" =~ ^[0-9a-fA-F]{64}$ && "$name" == sbx.sh && -z "$extra" ]]
  printf '%s  sbx.sh\n' "${hash,,}" | sha256sum -c -
  sudo env SBX_DIST_SHA="$rev" SBX_SCRIPT_SHA256="$hash" bash sbx.sh
)
```

`SBX_DIST_SHA` 固定下载提交；`SBX_SCRIPT_SHA256` 校验正在执行的本地脚本。后一项只接受普通文件，不用于管道或进程替换。校验和来自同一分发仓，不是独立签名。

面板默认监听 `0.0.0.0`，使用明文 HTTP。不要直接对公网放行面板端口；远程管理请绑定回环地址后使用 SSH 隧道或 HTTPS 反向代理。`secure_cookie` 不会给 HTTP 加密。
