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
| 采集方式 | 默认每 2 秒读取一次计数器，按真实 duration_ms 计算实时速率 |
| 计数器归零 | 检测到 `当前值 < 上次值` 时全量补入累计；未知时长不进入实时速率 |
| 规则重建 | 规则集带 epoch 世代标记，换代后从新基线继续 |
| 首次启动 | 已有历史计数完整进入累计，但不制造假实时速率峰值 |
| 崩溃安全 | 增量入账 + 基线保存在同一个 SQLite 事务 |
| 跨天切点 | 固定按中国时间 UTC+8（无 tzdata 时使用固定偏移） |

需要明确的一点：计数的是 **IP 层字节数**，包含 TCP/IP 包头与协议开销，
因此数字会比客户端显示的「应用层已下载」略高（典型差异 2%~5%，取决于包大小）。
这是网络计费的通用口径 —— 服务商按流量收费也是按这个口径算的。
如果你要的是「和云厂商账单对得上」，这个口径正是对的；
如果要的是「和客户端下载进度条对得上」，任何服务端统计都做不到逐字节相同。

两个后端的细微差别：

- nftables 后端的计数链挂在 `priority 300`，在常规防火墙规则之后，**被防火墙丢弃的包不计入**
- iptables 后端的计数链插在 `INPUT/OUTPUT` 首位（必须如此，否则前面的 `-j ACCEPT` 会让计数规则永远命中不到），因此**会计入随后被丢弃的包**

## 面板

高级深色控制台 / 浅色玻璃双主题，面板只保留自用场景真正需要的信息：

- 实时速率数字（上传/下载）
- TCP 连接数、UDP 会话数
- 今日流量、累计流量
- 最近 60 天每日流量表格
- 单节点最近 60 天每日明细表格
- 节点端口、类型、今日/累计用量与实时速率

每日流量和单节点每日明细使用 **Excel 风格表格**，列为：

```text
日期 | 上传 | 下载 | 合计
```

不显示重复的底部总计行，不需要长按或悬浮查看数据。手机端使用紧凑布局并支持横向滚动，确保上传、下载、合计列完整显示。

面板已经彻底移除：

- 历史速率曲线
- 实时速率波浪线
- 柱状图
- 流量占比/百分比
- 顶部 nftables/日期/主题状态信息区
- 底部数据来源/时区/CSV区域
- 节点“按今日/按累计”排序切换

实时速率走轻量 `/api/live`，默认约每 2 秒刷新；历史概览低频刷新；请求防重入，实时数字短时缓动，节点实时列增量更新，页面后台暂停轮询。

当前发布版本为 v2.5.8。实时速率底层使用真实 `duration_ms`，严格按字节/真实耗时计算；首次启动、规则换代、计数器归零不产生假峰；samples只保留2分钟。

## 升级

以后有新功能或修 bug，不用重装，直接：

```bash
sbx --update
```

它会从 GitHub 拉最新版、校验后替换本体，**保留你所有节点和流量历史**，然后自动重启服务。已是最新版会直接跳过；想强制重装用 `sbx --update --force`。菜单里的「检查更新 / 升级」是同一个功能。

## 管理节点

菜单里可以：

- **添加节点** — 7 种协议，自动生成分享链接
- **修改节点** — 改端口、改 VLESS/Trojan/Hysteria2/TUIC/AnyTLS 的 SNI 伪装域名、改 Hysteria2 端口跳跃范围；改完自动重载并给出新分享链接，流量历史不丢
- **删除节点** — 可选是否一并清除该节点历史流量

全新安装后节点列表是空的，自己按需添加（不再自动塞一个默认节点）。

## 命令行

```
sbx                  # 管理菜单
sbx --update         # 在线升级（保留节点与流量历史）
sbx --show           # 各节点用量速览
sbx --links          # 输出分享链接
sbx --panel-url      # 输出面板地址
sbx --apply-firewall # 重建计数规则
sbx --uninstall      # 卸载
```

面板本体也可直接调用：

```
python3 /etc/sbx/panel.py show          # 各节点今日/累计
python3 /etc/sbx/panel.py daily 60      # 最近 60 天
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
sbx.sh                  自包含发布脚本（用户直接 curl 执行）
src/panel.py            采集器 + HTTP 面板
src/nodes_tool.py       节点管理、端口/SNI修改与链接生成
web/                    前端（高级深/浅主题、紧凑表格、实时刷新）
build.py                内嵌全部资源，生成根目录 sbx.sh
test_*.py/.sh            回归测试
```

发布：`python3 build.py` → 把 `dist/sbx.sh` 推到仓库，用户 curl 这个文件。

沙箱测试（不碰真实系统）：

```
SBX_ROOT=/tmp/x SBX_NO_SERVICE=1 SBX_PAYLOAD_DIR=$PWD \
SBX_SB_BIN=/path/to/sing-box bash sbx.sh
```
