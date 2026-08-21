# SBX — sing-box 节点 + 精确流量面板

一条命令装好 sing-box 节点，附带一个只做流量统计的可视化 Web 面板。

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

装完后随时用 `sbx` 打开管理菜单。

## 功能范围

刻意只做两件事：

- **搭建节点** — VLESS Reality / Shadowsocks 2022 / Hysteria2 / Trojan / TUIC / VMess-WS / AnyTLS，自动生成分享链接与订阅
- **流量统计** — 每日流量、总流量、**每个节点的独立用量**、实时速率

没有规则分流、订阅转换、多用户系统、Clash 面板代理切换等功能。

## 统计精度

数据来源是**内核 netfilter 计数器**（nftables 命名计数器，无 nft 时退回 iptables 自定义链），
不是应用层估算、不抽样、不插值。

| 维度 | 做法 |
|---|---|
| 计数位置 | 内核 IP 层，按节点端口分别计数 |
| rx（上传） | 服务器从客户端收到的字节 = 用户上传 |
| tx（下载） | 服务器发往客户端的字节 = 用户下载 |
| 采集方式 | 每 5 秒读一次计数器，单调差分累加进 SQLite |
| 计数器归零 | 检测到 `当前值 < 上次值` 时按全量补记，不丢不重 |
| 规则重建 | 规则集带 epoch 世代标记，`sbx apply` 换代后从新基线继续 |
| 崩溃安全 | 增量入账 + 基线保存在同一个 SQLite 事务，崩溃只回退到上一轮 |
| 跨天切点 | 固定按中国时间 UTC+8（无 tzdata 的系统自动用固定偏移） |

需要明确的一点：计数的是 **IP 层字节数**，包含 TCP/IP 包头与协议开销，
因此数字会比客户端显示的「应用层已下载」略高（典型差异 2%~5%，取决于包大小）。
这是网络计费的通用口径 —— 服务商按流量收费也是按这个口径算的。
如果你要的是「和云厂商账单对得上」，这个口径正是对的；
如果要的是「和客户端下载进度条对得上」，任何服务端统计都做不到逐字节相同。

两个后端的细微差别：

- nftables 后端的计数链挂在 `priority 300`，在常规防火墙规则之后，**被防火墙丢弃的包不计入**
- iptables 后端的计数链插在 `INPUT/OUTPUT` 首位（必须如此，否则前面的 `-j ACCEPT` 会让计数规则永远命中不到），因此**会计入随后被丢弃的包**

## 面板

- 今日总量 / 累计总量 / 实时速率 / 节点数 四个概览卡片
- 每日流量柱状图（7 / 30 / 90 天可切换，上传下载分列）
- 近 30 分钟实时速率曲线
- 各节点用量表格：今日↑↓、累计↑↓、实时速率、占比条（可按今日或累计排序）
- 单节点每日明细图
- CSV 导出

纯原生 JS + 手绘 SVG，无外部 CDN 依赖，窄屏自动切换为卡片布局。

访问需要令牌：`http://<ip>:<port>/?token=<token>`。
面板设置里可以改端口、换令牌、调采集间隔、切换「仅本机访问」。

> 默认监听 `0.0.0.0` 并用令牌鉴权。如果这台机器暴露在公网，建议在「面板设置 → 4」
> 切到仅本机访问，然后用 SSH 端口转发查看：`ssh -L 8080:127.0.0.1:<port> user@server`。

## 命令行

```
sbx                  # 管理菜单
sbx --show           # 各节点用量速览
sbx --links          # 输出分享链接
sbx --panel-url      # 输出面板地址
sbx --apply-firewall # 重建计数规则
sbx --uninstall      # 卸载
```

面板本体也可直接调用：

```
python3 /etc/sbx/panel.py show          # 各节点今日/累计
python3 /etc/sbx/panel.py daily 30      # 最近 30 天
python3 /etc/sbx/panel.py selftest      # 计数器自检
python3 /etc/sbx/panel.py reset node:2  # 只清 2 号节点的历史
```

## 支持环境

- Debian / Ubuntu、RHEL 系（CentOS/Rocky/Alma）、Alpine
- systemd 或 OpenRC
- amd64 / arm64 / armv7 / armv6 / 386 / s390x / riscv64
- 依赖：python3（标准库即可，不装任何 pip 包）、curl、openssl、nftables 或 iptables

## 文件位置

```
/etc/sbx/panel.py        采集器 + Web 服务
/etc/sbx/nodes_tool.py   节点增删与链接生成
/etc/sbx/panel.json      面板配置（含令牌，权限 600）
/etc/sbx/nodes.json      节点清单（inbound 的唯一数据源）
/etc/sbx/traffic.db      流量历史 SQLite
/etc/sbx/nft.conf        生成的 nftables 计数规则
/etc/sbx/iptables.sh     生成的 iptables 计数脚本
/etc/sbx/web/            面板前端
/etc/sing-box/config.json
```

`nodes.json` 是 inbound 的唯一数据源，`sbx` 每次改动都会由它重建 sing-box 的 inbounds，
并且只动 tag 以 `sbx-n` 开头的部分 —— 你手工加的 inbound 不会被覆盖。
所有配置改动都先 `sing-box check` 校验，通过才落盘，失败自动回滚。

## 开发

```
installer-template.sh   安装器模板（开发时用 SBX_PAYLOAD_DIR 指向本目录）
src/panel.py            采集 + HTTP 面板
src/nodes_tool.py       节点管理与分享链接
web/                    前端（原生 JS + 手绘 SVG，无 CDN 依赖）
build.py                读模板 + 内嵌资源 → 生成根目录自包含 sbx.sh
sbx.sh                  构建产物：用户 curl 执行的单文件（请勿手改）
```

改完源码后运行 `python3 build.py` 重新生成根目录的 `sbx.sh`，再提交。

沙箱测试（不碰真实系统，把整套装到 /tmp 前缀下）：

```
SBX_ROOT=/tmp/x SBX_NO_SERVICE=1 SBX_PAYLOAD_DIR=$PWD \
SBX_SB_BIN=/path/to/sing-box bash installer-template.sh
```
