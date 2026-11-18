# SBX

sing-box 节点管理面板。一条命令装到服务器上，节点、流量、在线 IP、限速都在网页里管。

后端是 Go 写的静态单文件，前端打包在里面，服务器不需要 Python 或别的运行时。

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

不想直接跑管道，就先下下来核对：

```bash
curl -fsSLO https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/sbx.sh
curl -fsSLO https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/sbx.sh.sha256
sha256sum -c sbx.sh.sha256 && sudo bash sbx.sh
```

需要 root，普通用户加 `sudo`。

装完运行 `sbx` 进管理菜单，面板地址用 `sbx --panel-url` 查看。

安装过程：检查系统和 init → 安装 nftables（不可用就停下）→ 下载并校验 sbx-core → 安装 sing-box → 写计数规则 → 启动 `sbx-firewall`、`sing-box`、`sbx-panel` 三个服务。

nftables 是必须的，没有就装不了，也不会退回到 iptables。

### 支持

| 系统 | 版本 |
|---|---|
| Debian | 11 / 12 |
| Ubuntu | 20.04 / 22.04 / 24.04 |
| RHEL / Rocky / Alma | 8 / 9 |
| Fedora | 38+ |
| Alpine | 3.18+ |

架构：x86_64、arm64、armv7、386、s390x、riscv64 都有二进制，armv6 没验证过。

## 面板

浏览器打开 `http://服务器地址:8080`，用安装时生成的令牌登录，之后七天不用再输。令牌在安装结束时打印过，忘了可以看 `/etc/sbx/panel.json`。

| 页签 | 内容 |
|---|---|
| 首页 | 节点卡片、累计和今日流量、实时速率、在线 IP |
| 每日 | 所有节点近 180 天的流量曲线 |
| 节点 | 单个节点的趋势，以及每天访问过哪些域名 |
| 配置 | 新建、编辑、删除节点，查看分享链接 |

数据每 2 秒刷新一次，在线 IP 通过 SSE 推送。

## 节点

支持六种协议：VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5/v6、SOCKS5。Snell 需要 sing-box 1.14 以上，创建时会自动升级内核。

每个节点可以单独设置：

- **启用 / 暂停**：暂停只拦这个节点的端口，不删配置，也不重启 sing-box
- **在线 IP 上限**：达到上限后只挡新 IP，已经连上的不受影响
- **限速**：按节点设置 Mbps，上传和下载分开
- **只允许访问特定服务**：白名单，只放行勾选的服务，其它全部拒绝
- **限制来源省份**：只让指定省份的 IP 连进来，勾一个就是单选，勾多个就是多选

后两项在「配置 → 编辑节点」里打开。

白名单用到的规则集第一次会从上游下载，缓存在 `/etc/sing-box/rulesets/`，之后不再联网。服务器连不上外网的话，可以把 Surge 格式的 `.list` 文件手动放进这个目录。

省份限制只管 IPv4。开了之后这个节点的 IPv6 连接会被直接拒绝，因为查不到省份就不放行。客户端会走 IPv6 的话，就用 IPv4 连，或者别开这个限制。

## 常用命令

```bash
sbx                          # 管理菜单
sbx --update                 # 升级
sbx --update --force         # 强制重装
sbx --show                   # 今日和累计流量
sbx --panel-url              # 面板地址
sbx --apply-firewall         # 重建计数规则
sbx --clear-firewall         # 清除计数规则
sbx --uninstall              # 卸载
sbx --version                # 版本
```

```bash
sbx-core show                # 命令行看流量
sbx-core daily 30            # 最近 30 天
sbx-core selftest            # 计数器自检
sbx-core reset node:2        # 清空某个节点的统计
sbx-core node list           # 节点列表
sbx-core node edit 2 --provinces=广东,广西
sbx-core node edit 2 --allow=Google,TikTok
sbx-core node links          # 分享链接
```

## 文件位置

```text
/usr/local/bin/sbx           菜单入口
/usr/local/bin/sbx-core      后端（前端内嵌）
/etc/sbx/panel.json          面板配置
/etc/sbx/nodes.json          节点数据（0600）
/etc/sbx/traffic.db          流量库
/etc/sbx/policy.nft          暂停 / IP / 限速 / 省份规则
/etc/sbx/nft.conf            流量计数规则
/etc/sing-box/config.json    sing-box 配置
```

面板默认监听 `0.0.0.0:8080`。要放到公网，建议前面套一层 HTTPS 反代，并在 `panel.json` 里设 `secure_cookie=true`；只给本机用就把 `listen` 改成 `127.0.0.1`。

## 升级与卸载

`sbx --update` 会保留节点配置和流量记录，二进制原子替换，失败自动回滚。

卸载时如果 `/etc/sing-box` 不是空的，会先打包到 `/var/backups/sbx-sing-box-<时间>.tar.gz`，打包失败就保留目录不删。`/etc/sbx` 里的节点凭据和流量库会一起删掉。

## 流量日报

可以每天定时把流量推送到飞书群。管理菜单 →「系统设置与运维」→「流量日报配置」，填 Webhook 就能用，不配也不影响统计。

## 许可

专有软件，保留所有权利。见 [LICENSE](LICENSE)。
