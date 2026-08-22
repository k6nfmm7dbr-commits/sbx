# SBX

sing-box 节点搭建与内核流量统计面板。自用、轻量、无多用户、无分流、无订阅转换。

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

安装后运行 `sbx` 打开命令菜单。

## 当前版本

```text
v2.8.0
```

## 节点

支持 VLESS Reality、Shadowsocks 2022、Hysteria2、Trojan、TUIC v5、VMess WebSocket、AnyTLS。

菜单支持添加、删除节点，修改端口，修改 SNI，修改 Hysteria2 端口跳跃范围，生成 IPv4/IPv6 分享链接和 Base64 订阅。

全新安装不会自动创建默认节点。节点按 `nodes.json` 添加顺序显示。

## 面板结构

底部三页导航：

- **首页**：实时速率、TCP/UDP会话、今日/累计流量、节点状态；节点状态可下拉切换节点
- **每日**：最近60天每日流量 Excel 表格
- **节点**：选择节点查看最近60天单节点每日明细

每日表格固定四列：

```text
日期 | 上传 | 下载 | 合计
```

手机端支持横向查看，四列完整显示，不需要长按或悬浮。

当前 UI 使用纯黑字体、白色卡片和粗黑边框。节点状态卡使用端口/TCP/UDP与上传/下载速率分区。

已删除功能：历史速率曲线、实时波浪线、柱状图、流量占比、管理页、管理密钥、主题切换、顶部状态区、底部来源区、节点排序切换。

## 统计

流量来自内核 netfilter 计数器：优先 nftables named counter，不支持时回退 iptables 自定义链；按节点端口计数，不抽样、不估算。

- rx：服务器从客户端收到的 IP 层字节，即上传
- tx：服务器发往客户端的 IP 层字节，即下载
- 包含 TCP/IP 包头与协议开销
- 中国时间 UTC+8 跨天
- 首次启动、规则换代、计数器归零只进累计，不制造假速率峰值
- 默认约每2秒采集，实时速率按真实 `duration_ms` 计算
- samples仅保留约2分钟

客户端通常只显示应用层有效载荷，面板数字一般会高约2%～5%；云厂商计费口径通常更接近面板。

TCP连接读取 `/proc/net/tcp[6]`，UDP会话读取 `/proc/net/udp[6]`。Hysteria2/TUIC基于QUIC多路复用，UDP socket数仅作活跃度参考。

## 在线升级

```bash
sbx --update
sbx --update --force
```

升级保留节点配置、面板端口和流量历史。升级器同时比较版本号和脚本 SHA-256。

## 常用命令

```bash
sbx
sbx --update
sbx --show
sbx --links
sbx --panel-url
sbx --apply-firewall
sbx --uninstall

python3 /etc/sbx/panel.py show
python3 /etc/sbx/panel.py daily 60
python3 /etc/sbx/panel.py selftest
python3 /etc/sbx/panel.py reset node:2
```

## 支持环境

Debian/Ubuntu、RHEL系、Alpine；systemd/OpenRC；amd64、arm64、armv7、armv6、386、s390x、riscv64。

依赖 Python 3 标准库、curl、openssl、nftables 或 iptables。

## 文件

```text
/etc/sbx/panel.py
/etc/sbx/nodes_tool.py
/etc/sbx/panel.json
/etc/sbx/nodes.json
/etc/sbx/traffic.db
/etc/sbx/nft.conf
/etc/sbx/iptables.sh
/etc/sbx/web/
/etc/sing-box/config.json
/usr/local/bin/sbx
```

`nodes.json` 是 SBX 管理节点的唯一数据源。SBX只重建 tag 以 `sbx-n` 开头的 inbound，不覆盖其它自定义 inbound。配置修改前执行 `sing-box check`，失败自动回滚。

## 开发

```text
installer-template.sh   安装器模板
sbx.sh                  自包含发布脚本
src/panel.py            采集器和HTTP服务
src/nodes_tool.py       节点管理和链接生成
web/                    面板前端
build.py                发布构建脚本
test_*.py / test_*.sh    回归测试
```

重新构建：`python3 build.py`

沙箱安装：

```bash
SBX_ROOT=/tmp/sbx-test SBX_NO_SERVICE=1 SBX_PAYLOAD_DIR=$PWD SBX_SB_BIN=/path/to/sing-box bash installer-template.sh
```

回归覆盖计数差分、归零、epoch换代、SQLite事务、nft/iptables格式解析、节点管理、TCP/UDP、升级、沙箱安装、HTTP API和发布包解包。
