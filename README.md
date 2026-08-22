# SBX

sing-box 节点搭建 + 内核精确流量面板。自用、轻量、无多用户、无分流、无订阅转换。

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

安装完成后运行：

```bash
sbx
```

## 当前版本

```text
v2.6.0
```

## 功能

### 节点

支持：

- VLESS Reality
- Shadowsocks 2022
- Hysteria2
- Trojan
- TUIC v5
- VMess WebSocket
- AnyTLS

菜单支持：

- 添加节点
- 修改节点端口
- 修改 VLESS / Trojan / Hysteria2 / TUIC / AnyTLS 的 SNI
- 修改 Hysteria2 端口跳跃范围
- 删除节点
- 输出 IPv4 / IPv6 分享链接和 Base64 订阅

全新安装不会自动创建默认节点。节点按添加顺序显示。

### 流量面板

面板显示：

- 实时总速率、上传速率、下载速率
- 每节点实时速率
- TCP 连接数
- UDP 会话数
- 今日流量
- 累计流量
- 每节点今日 / 累计流量
- 最近 60 天每日流量
- 单节点最近 60 天每日明细

每日数据使用 Excel 风格表格：

```text
日期 | 上传 | 下载 | 合计
```

手机端支持横向滚动，四列完整显示。面板支持深色 / 浅色主题切换。

## 统计口径

流量来自内核 netfilter 计数器：

- 优先使用 nftables named counter
- 不支持 nftables 时回退 iptables 自定义链
- 按节点监听端口独立计数
- 上传：服务器从客户端收到的 IP 层字节
- 下载：服务器发往客户端的 IP 层字节
- 包含 TCP/IP 包头与协议开销
- 中国时间 UTC+8 跨天

客户端通常只显示应用层有效载荷，因此面板数字一般会高约 2%～5%；云厂商流量计费通常更接近面板的 IP 层口径。

### 实时速率

- 默认每 2 秒采集
- 使用真实 `duration_ms` 计算 `字节 / 实际耗时`
- 首次启动、规则换代、计数器归零只计入累计，不制造假瞬时速率
- 实时接口 `/api/live` 与历史概览分离
- 前端请求防重入、数字短时缓动、后台暂停轮询
- 实时样本只保留 2 分钟

### TCP / UDP

- TCP：读取 `/proc/net/tcp` 和 `/proc/net/tcp6` 的 ESTABLISHED socket
- UDP：读取 `/proc/net/udp` 和 `/proc/net/udp6` 中存在远端地址的会话 socket
- Hysteria2 / TUIC 使用 QUIC 多路复用，UDP socket 数不等同于客户端逻辑连接数，仅作活跃度参考

## 在线升级

```bash
sbx --update
```

强制重新应用线上版本：

```bash
sbx --update --force
```

升级保留：

- 节点配置
- 面板令牌
- 面板端口
- 流量数据库

升级器同时比较版本号与脚本 SHA-256。即使版本号误漏更新，只要远端脚本内容发生变化也会继续升级。

## 常用命令

```bash
sbx                         # 管理菜单
sbx --update                # 在线升级
sbx --show                  # 命令行查看节点流量
sbx --links                 # 输出分享链接
sbx --panel-url             # 输出面板地址
sbx --apply-firewall        # 重建计数规则
sbx --uninstall             # 卸载

python3 /etc/sbx/panel.py selftest
python3 /etc/sbx/panel.py daily 60
python3 /etc/sbx/panel.py reset node:2
```

## 支持环境

- Debian / Ubuntu
- CentOS / Rocky / AlmaLinux
- Alpine Linux
- systemd / OpenRC
- amd64 / arm64 / armv7 / armv6 / 386 / s390x / riscv64

依赖：

- Python 3（仅标准库）
- curl
- openssl
- nftables 或 iptables

## 文件位置

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

`nodes.json` 是 SBX 管理节点的唯一数据源。SBX 只重建 tag 以 `sbx-n` 开头的 inbound，不覆盖其它自定义 inbound。配置修改前会执行 `sing-box check`，失败自动回滚。

## 开发

```text
installer-template.sh   安装器模板
sbx.sh                  自包含发布脚本
src/panel.py            采集器和 HTTP 服务
src/nodes_tool.py       节点管理与链接生成
web/                    面板前端
build.py                发布版构建脚本
test_*.py / test_*.sh   回归测试
```

重新构建：

```bash
python3 build.py
```

沙箱安装：

```bash
SBX_ROOT=/tmp/sbx-test \
SBX_NO_SERVICE=1 \
SBX_PAYLOAD_DIR=$PWD \
SBX_SB_BIN=/path/to/sing-box \
bash installer-template.sh
```

## 测试

当前测试覆盖：

- 计数器差分
- 计数器归零
- 规则 epoch 换代
- SQLite 事务一致性
- nftables / iptables 真实格式解析
- 节点添加 / 修改 / 删除
- Reality 特殊参数安全
- TCP / UDP socket 统计
- 在线升级
- 沙箱安装
- 单文件解包
- HTTP 鉴权与 API
