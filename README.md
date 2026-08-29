# SBX

sing-box 节点一键搭建 + 精确流量统计面板。后端为 Go 单二进制 `sbx-core`，前端经 `go:embed` 内嵌，无 Python 依赖。

## 特性

- **5 种协议**：VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5/v6，菜单化创建与分享链接
- **内核级流量统计**：nftables named counter（回退 iptables 自定义链），非估算、不丢计不重复
- **节点策略**：独立流量配额（Quota）与同时在线公网 IP 上限（IP Limit），达限由内核执行
- **实时面板**：三页签 Web UI（首页 / 每日 / 节点），令牌登录，SSE 实时推送在线 IP
- **在线升级**：保留节点与流量历史，二进制原子替换，失败自动回滚

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

安装完成后运行 `sbx` 进入管理菜单。

安装器自动完成：系统与 init 检测（Debian/Ubuntu、RHEL 系、Alpine；systemd/OpenRC）→ 按架构下载 `sbx-core` 并以 `SHA256SUMS` 校验（不符即中止，不影响已有安装）→ 安装 sing-box → 注册并启动 `sbx-firewall`、`sing-box`、`sbx-panel` 三个服务。

> 任意登录用户（`root` / `ubuntu` 等）均可安装：脚本要求 root 权限，普通用户用 `sudo` 执行即可，服务由 systemd 以 root 运行，与登录用户名无关。

## 当前版本

```text
v3.0.5
```

源码在 `main` 分支，二进制从 `dist` 分支分发（rolling latest），不使用 Tag。

## 协议与节点

支持：VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5 / v6。

- Snell 需 sing-box ≥ 1.14，创建时自动升级内核；分享链接提供通用 URI 与 Surge 配置两种格式
- 节点按 `nodes.json` 顺序显示，ID 单调递增不复用
- 添加节点采用候选配置 + `sing-box check` + 原子提交 + 失败回滚的事务流程；
  提交前自动校正 `route.final` 悬空引用（`sing-box check` 不校验该引用，悬空会导致启动 FATAL 并回滚）

## 流量统计

数据源为内核 netfilter 计数器（nftables named counter，回退 iptables 自定义链），非估算。

- **rx** = 服务器接收（用户上传），**tx** = 服务器发送（用户下载），含包头，比客户端显示高约 2%–5%
- 单调差分累加：首次采集只入累计；计数器归零补记当前值，不产生假峰值
- 规则集带 epoch 世代标记，重建后自动衔接，不丢计不重复
- 默认每 2 秒采集，速率按真实 `duration_ms` 计算，采样保留约 2 分钟
- 跨天按面板时区（默认 Asia/Shanghai，内嵌时区数据）
- 连接数读 `/proc/net/tcp[6]`、`/proc/net/udp[6]` 由采集线程缓存；
  UDP 为「可观测 socket/会话」口径，通配 UDP 入站（如 sing-box）不展开为客户端会话数

## Web 面板

底部三页签（首页 / 每日 / 节点），令牌登录（HttpOnly Cookie，SameSite=Lax）。

- **首页**：节点卡片（实时速率 / 累计流量 / TCP·UDP 连接数 / 在线 IP / 配额状态）+ 顶部 KPI
- **每日**：全节点流量趋势（近 60 天）与单节点详情
- **节点**：节点管理抽屉 —— 流量配额、IP 数量限制、重置已用流量、查看在线 IP
- **实时**：`/api/live` 2s 高频轮询刷新速率与连接数；`/api/events`（SSE）推送在线 IP 增量，无需整页刷新

## 节点策略

在 Web 面板 → 节点卡片 → 管理，可为每个节点独立设置（不进 `sbx` CLI 菜单）。

### 流量配额

- 基于内核 byte counter 累计（GiB/TiB），达限只阻断目标节点端口（nft 双向 drop）
- 提高额度自动恢复；「重置已用流量」只清零额度使用量，不删除历史累计
- 达限节点在面板显示「已暂停接入」，配额状态实时联动

### 同时在线 IP 上限

- 按服务端可见的公网源 IP 统计（NAT 下多设备算一个出口 IP），支持 TCP/UDP、IPv4/IPv6
- 达限只阻止新 IP，不随机踢已在线 IP；UDP slot 超时自动释放
- 基于 conntrack 判活：移动端异常断开（无 FIN）的连接，字节增量停止后按空闲窗口释放；
  conntrack 不可用时回退 `/proc` ESTABLISHED
- 内核执行：nft allow set 只放行已获 slot 的 IP，新 IP 的 SYN 放行、established 数据拦截（避免「第二个 IP 永远连不上」的死锁）

两种策略默认「不限」，旧节点升级后行为不变。

## 升级

```bash
sbx --update            # 更新 sbx.sh + sbx-core（SHA256 校验 + 内容比对）
sbx --update --force    # 强制重装当前/最新版本
```

保留节点配置、面板端口与流量历史；二进制原子替换，失败回滚；升级后自动重建计数规则。

## 命令

```bash
sbx                     # 管理菜单
sbx --update            # 在线升级
sbx --show              # 今日/累计流量
sbx --links             # 分享链接
sbx --panel-url         # 面板地址
sbx --apply-firewall    # 重建计数规则
sbx --clear-firewall    # 移除计数规则
sbx --uninstall         # 卸载
sbx --version           # 版本信息

sbx-core show | daily 60 | selftest | reset node:2
sbx-core node list | links | add | edit | remove | sync
```

## 支持环境

- 系统：Debian/Ubuntu、RHEL 系、Alpine；init：systemd / OpenRC
- 架构：amd64、arm64、armv7、armv6、386、s390x、riscv64（`CGO_ENABLED=0`）
- 依赖：curl、openssl、tar、jq、nftables 或 iptables（计数后端）、iproute2 `ss`（连接数回退，随系统自带）
- 注意：Ubuntu 20.04 等使用 iptables-legacy 的系统，面板服务已带 `CAP_NET_RAW` 适配，避免「计数器不存在 / filter table Permission denied」

## 文件

```text
/usr/local/bin/sbx-core        Go 后端（含内嵌前端）
/usr/local/bin/sbx             菜单入口
/etc/sbx/panel.json            面板配置（端口 / token / 时区 / 后端）
/etc/sbx/nodes.json            节点数据（含凭据，0600）
/etc/sbx/state.json            ID 游标 / 分享地址
/etc/sbx/traffic.db            SQLite 流量库（WAL）
/etc/sbx/nft.conf              计数规则（nftables）
/etc/sbx/iptables.sh           计数规则（iptables 回退）
/etc/sing-box/config.json      sing-box 配置
/etc/systemd/system/sbx-{panel,firewall}.service
```

## 开发

```text
cmd/sbx-core/            入口
internal/                各模块（api / database / nodes / config / traffic / connection / firewall / service / policy）
internal/webui/static/   前端（go:embed）
installer-template.sh    sbx.sh 模板（与 sbx.sh 保持同步，CI 校验）
scripts/build-release.sh 七架构交叉编译 + SHA256SUMS
scripts/e2e_remote.sh    真机验收
docs/AUDIT.md            行为审计（留档）
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

## 分发

- `main`：源码（含安装器与测试）
- `dist`：编译产物（`sbx-core-linux-<arch>` + `SHA256SUMS`），安装器按架构从这里下载
- `backup/pre-reset-removal-20260828-0246`：定时流量重置功能移除前的完整版本备份（含月度配额自动归零状态机）
