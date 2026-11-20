# SBX

SBX 是 [sing-box](https://github.com/SagerNet/sing-box) 的节点管理和流量统计面板。

## 安装

需要 Linux、nftables（nftables-only，不回退到 iptables）和 root 权限。支持 systemd 与 OpenRC。下载命令使用 Bash、Git、curl 和 `sha256sum`；已是 root 时可省略 `sudo`。

在空目录中执行。脚本和校验和取自[公开分发仓](https://github.com/k6nfmm7dbr-commits/sbx/tree/dist)的同一个提交，校验通过才执行：

```bash
repo=k6nfmm7dbr-commits/sbx
rev=$(git ls-remote "https://github.com/$repo.git" refs/heads/dist | cut -f1)
[[ "$rev" =~ ^[0-9a-f]{40}$ ]] &&
  base="https://raw.githubusercontent.com/$repo/$rev" &&
  curl -fsSLO "$base/sbx.sh" &&
  curl -fsSLO "$base/sbx.sh.sha256" &&
  sha256sum -c sbx.sh.sha256 &&
  sudo env SBX_DIST_SHA="$rev" bash sbx.sh
```

校验和来自同一分发仓，不是独立签名。

## 面板

`sudo sbx` 打开管理菜单，`sudo sbx --panel-url` 查看面板地址。登录令牌在 `/etc/sbx/panel.json`，请勿分享。

新装默认监听 `0.0.0.0`，安装器自动选端口。不要直接把 HTTP 面板暴露到公网；需要远程访问时，建议将面板的 `listen` 改为 `127.0.0.1`，用 HTTPS 反向代理，并在 `panel.json` 中设 `"secure_cookie": true`。修改后重启面板服务。

## 升级

使用新版升级器时，运行 `sudo sbx --update`。升级从同一个分发提交下载并校验脚本和后端，保留节点与流量历史，期间面板和 sing-box 会短暂中断。

**旧版一次性迁移：** 如果 `/etc/sbx/sbx.sh` 中没有 `DIST_PROTOCOL="sbx-dist-v1"`，不要用旧版 `sbx --update`。先备份 `/etc/sbx` 和 `/etc/sing-box`，再按安装步骤下载和校验，把最后的 `bash sbx.sh` 改为 `bash sbx.sh --update`，保留 `SBX_DIST_SHA`。不要手工覆盖已安装脚本。迁移成功后，后续升级使用 `sudo sbx --update`。

如果升级中断或提示无法确认回滚，保留 `/etc/sbx/.upgrade-transaction`。先核对其中的 `generation`、`paths`、`services` 和 `snapshot`，再决定恢复或继续；不要删除备份后直接重试。备份含凭据，不要公开。排查版本时看升级输出中的分发提交，不能只看 `v3.x`。

## 卸载

运行 `sudo sbx --uninstall` 并确认。`/etc/sbx` 会被删除，包括节点配置和流量历史；如需保留，请先自行备份。非空的 `/etc/sing-box` 会先打包到 `/var/backups/`，备份失败则保留原目录。

## 文档与许可

变更见 [更新日志](CHANGELOG.md)。SBX 为专有软件，见 [LICENSE](LICENSE)。省级 IPv4 数据来自 [ip2region](https://github.com/lionsoul2014/ip2region)，归属及许可见 [第三方资料说明](https://github.com/k6nfmm7dbr-commits/sbx/blob/main/THIRD_PARTY_NOTICES.md)。
