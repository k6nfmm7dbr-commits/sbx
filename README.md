# SBX

sing-box 节点管理面板。一条命令装到服务器，节点、流量、在线 IP、限速都在网页里管。

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

需要 root 和 nftables（nftables-only，不会退回 iptables）。Debian 11+ / Ubuntu 20.04+ / RHEL 8+ / Fedora 38+ / Alpine 3.18+，x86_64、arm64 等 7 种架构。

装完敲 `sbx` 进菜单，`sbx --panel-url` 看面板地址。登录令牌在 `/etc/sbx/panel.json`。

## 节点

六种协议：VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5/v6、SOCKS5。

每个节点能单独开关：暂停、在线 IP 上限、限速、白名单（只允许访问特定服务）、来源省份。后两项在「配置 → 编辑节点」里。省份限制只管 IPv4，开了之后该节点的 IPv6 会被拒绝。

## 命令

```bash
sbx                  # 管理菜单
sbx --update         # 升级，保留配置和流量
sbx --show           # 今日和累计流量
sbx --uninstall      # 卸载
sbx-core show        # 命令行看流量
sbx-core daily 30    # 最近 30 天
```

## 文件

```text
/usr/local/bin/sbx-core      后端
/etc/sbx/panel.json          面板配置
/etc/sbx/nodes.json          节点数据
/etc/sbx/traffic.db          流量库
/etc/sing-box/config.json    sing-box 配置
```

卸载时 `/etc/sing-box` 非空会先打包到 `/var/backups/`，`/etc/sbx` 直接删。

面板默认监听 `0.0.0.0:8080`。`panel.json` 里 `listen`(默认 `0.0.0.0`) 改成 `127.0.0.1` 就只给本机用；放公网请套 HTTPS 反代并设 `secure_cookie=true`。

## 当前版本

```text
v3.0.30
```

## 许可

专有软件，保留所有权利。
