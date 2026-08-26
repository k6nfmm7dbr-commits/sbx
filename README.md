# SBX

sing-box 节点管理与流量统计面板。后端为 Go 单二进制 `sbx-core`，无 Python 依赖。

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

安装完成后运行 `sbx` 进入菜单。

安装器依次完成：系统与 init 检测（Debian/Ubuntu、RHEL 系、Alpine；systemd/OpenRC）→ 按架构下载 `sbx-core` 并用 `SHA256SUMS` 校验（不符即中止，不影响已有安装）→ 安装 sing-box → 注册并启动 `sbx-firewall`、`sing-box`、`sbx-panel` 三个服务。

## 当前版本

```text
v3.0.5
```

源码在 `main` 分支，二进制从 `dist` 分支分发，不使用 Tag。

## 协议

支持 VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5 / v6。

Snell 需 sing-box ≥ 1.14，创建时会自动升级内核。分享链接提供两种格式：

- 通用 URI（Shadowrocket / sing-box / Stash / Loon）：

  ```text
  snell://psk@主机:端口#名称 (Snell v5)
  ```

- Surge 配置格式（粘入 `[Proxy]` 段）：

  ```text
  名称 = snell, 主机, 端口, psk=xxx, version=5, reuse=true, tfo=true, ecn=true
  ```

节点按 `nodes.json` 顺序显示，ID 单调递增不复用。

## 流量统计

数据源为内核 netfilter 计数器（nftables named counter，回退 iptables 自定义链），非估算。

- rx = 服务器接收（用户上传），tx = 服务器发送（用户下载），含包头，比客户端显示高约 2%–5%；
- 单调差分累加：首次采集只入累计；计数器归零则补记当前值，不产生假峰值；
- 规则集带 epoch 世代标记，重建后自动衔接，不丢计不重复；
- 默认每 2 秒采集，速率按真实 `duration_ms` 计算，采样保留约 2 分钟；
- 跨天按 UTC+8（内嵌时区数据）。

连接数读 `/proc/net/tcp[6]`、`/proc/net/udp[6]`，由采集线程缓存。

## Web 面板

底部三页签（首页 / 每日 / 节点），令牌登录（HttpOnly Cookie）。前端经 `go:embed` 内嵌进 `sbx-core`，升级二进制即升级界面。

## 节点策略

在 Web 面板 → 节点卡片 → 管理，可为每个节点独立设置流量配额与同时在线 IP 上限（不进 `sbx` CLI 菜单）。

- **流量配额**：基于现有内核 byte counter 累计（GiB/TiB），达限只阻断目标节点端口；提高额度自动恢复；「重置已用流量」只清零额度使用量，不删除历史累计。
- **同时在线 IP**：按公网源 IP 统计（NAT 下多设备算一个），支持 TCP/UDP、IPv4/IPv6；达限阻止新 IP，不随机踢已在线 IP，UDP slot 超时（120s）自动释放。

两种策略默认「不限」，旧节点升级后行为不变。

## 升级

```bash
sbx --update            # 更新 sbx.sh + sbx-core（SHA256 校验）
sbx --update --force    # 强制重装
```

保留节点配置、面板端口与流量历史；二进制原子替换，失败回滚。

## 命令

```bash
sbx                     # 菜单
sbx --show              # 今日/累计流量
sbx --links             # 分享链接
sbx --panel-url         # 面板地址
sbx --apply-firewall    # 重建计数规则
sbx --uninstall         # 卸载

sbx-core show | daily 60 | selftest | reset node:2
sbx-core node list --json | links | add ...
```

## 支持环境

Debian/Ubuntu、RHEL 系、Alpine；systemd/OpenRC。架构：amd64、arm64、armv7、armv6、386、s390x、riscv64（`CGO_ENABLED=0`）。

依赖：curl、openssl、tar、nftables 或 iptables。无需 Python。

## 文件

```text
/usr/local/bin/sbx-core        Go 后端（含前端）
/usr/local/bin/sbx             菜单入口
/etc/sbx/panel.json            面板配置
/etc/sbx/nodes.json            节点数据
/etc/sbx/state.json            ID 游标 / 分享地址
/etc/sbx/traffic.db            SQLite 流量库（WAL）
/etc/sbx/nft.conf              计数规则（nftables）
/etc/sbx/iptables.sh           计数规则（iptables 回退）
/etc/sing-box/config.json      sing-box 配置
/etc/systemd/system/sbx-{panel,firewall}.service
```

配置写入前执行 `sing-box check`，失败回滚（候选文件机制）。

## 开发

```text
cmd/sbx-core/            入口
internal/                各模块（api / database / nodes / config / traffic / connection / firewall / service / policy）
internal/webui/static/   前端（go:embed）
installer-template.sh    sbx.sh 模板
scripts/build-release.sh 七架构交叉编译 + SHA256SUMS
scripts/e2e_remote.sh    真机验收
docs/AUDIT.md            早期 Python 版行为审计（留档）
```

```bash
go test ./... && go test -race ./...
./scripts/build-release.sh dist
```

核心逻辑（节点、流量、nftables、SQLite、sing-box 配置、Web API）全部由 Go 实现，仓库无 Python 文件。`internal/*/testdata` 为测试静态快照。

沙箱安装：

```bash
SBX_ROOT=/tmp/sbx-test SBX_NO_SERVICE=1 \
SBX_CORE_BIN=$PWD/dist/sbx-core-linux-amd64 SBX_SB_BIN=/path/to/sing-box bash sbx.sh
```
