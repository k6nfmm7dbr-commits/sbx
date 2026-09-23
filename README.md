# SBX

> sing-box 节点一键部署 + 内核级精确流量统计面板

SBX 用一条命令在你的服务器上搭好 sing-box 代理节点,并附带一个实时 Web 面板,把每个节点的流量、在线 IP、连接数看得清清楚楚。后端是一个 **Go 静态单二进制**(`sbx-core`),前端经 `go:embed` 内嵌,**服务器上无需 Python、无需任何运行时**。

netfilter 后端是 **nftables-only**:流量统计、流量配额、在线 IP 上限全部由 nftables(表 `sbx_traffic` / `sbx_policy`)在内核里完成。不支持 iptables,也没有后端自动选择或回退——nftables 不可用时 SBX 会**明确失败并中止**,绝不静默降级或"假装成功"。

<p>
  <img alt="version" src="https://img.shields.io/badge/version-v3.0.10-blue">
  <img alt="go" src="https://img.shields.io/badge/Go-1.27.1%2B-00ADD8">
  <img alt="license" src="https://img.shields.io/badge/license-MIT-green">
  <img alt="backend" src="https://img.shields.io/badge/netfilter-nftables--only-orange">
  <img alt="cgo" src="https://img.shields.io/badge/CGO-disabled-lightgrey">
</p>

---

## 目录

- [为什么是 SBX](#为什么是-sbx)
- [功能特性](#功能特性)
- [安装](#安装)
  - [推荐:先校验再执行](#推荐先校验再执行)
  - [一键安装(便捷,有风险)](#一键安装便捷有风险)
  - [让脚本自校验](#让脚本自校验)
  - [安装器做了什么](#安装器做了什么)
  - [支持矩阵](#支持矩阵)
- [管理菜单](#管理菜单)
- [协议与节点](#协议与节点)
- [Web 面板](#web-面板)
- [流量统计原理](#流量统计原理)
- [节点策略:配额与 IP 上限](#节点策略配额与-ip-上限)
- [命令参考](#命令参考)
- [配置与文件布局](#配置与文件布局)
- [安全性](#安全性)
- [升级、版本化下载与回退](#升级版本化下载与回退)
- [构建与开发](#构建与开发)
- [分发机制](#分发机制)
- [许可证](#许可证)

---

## 为什么是 SBX

市面上的一键脚本大多把"能连上"当作终点,而流量统计要么靠客户端自报、要么靠周期性估算,既不准也容易被绕过。SBX 的设计取向不同:

- **统计来自内核,不是估算。** 每个节点的收发字节由 nftables named counter 直接计数,单调差分累加,不丢计、不重复、不产生假峰值。
- **限制由内核执行,不是提示。** 流量配额达限、在线 IP 超上限,都是内核 drop,不是前端弹个框就算数。
- **失败就是失败。** nftables 缺失、规则应用失败、conntrack 不可用,SBX 都会如实报错并在面板呈现(`policy_error`),而不是"警告一下然后当作成功"。
- **一个静态二进制,零运行时依赖。** 交叉编译到 7 种架构,`CGO_ENABLED=0`,连 SQLite 都是纯 Go 实现(`modernc.org/sqlite`)。

工程原则的优先级是:**正确性 > 兼容性 > 稳定性 > 可维护性 > 资源占用 > 性能**。

---

## 功能特性

| | |
|---|---|
| **5 种协议** | VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5/v6,菜单化创建,自动生成分享链接 |
| **内核级流量统计** | nftables named counter,非估算;单调差分累加,规则重建自动衔接,不丢计不重复 |
| **流量配额(Quota)** | 每节点独立额度(GiB/TiB),达限内核双向 drop 该节点端口,提额自动恢复 |
| **在线 IP 上限(IP Limit)** | 按服务端可见的公网源 IP 统计,达限只挡新 IP、不踢已在线;基于 conntrack 判活 |
| **实时 Web 面板** | 三页签(首页 / 每日 / 节点),令牌登录,SSE 实时推送在线 IP,`/api/live` 高频刷新速率 |
| **在线升级** | 保留节点与流量历史,二进制原子替换,失败自动回滚,SHA256 校验 + 内容比对幂等 |
| **零运行时依赖** | Go 静态单二进制,前端内嵌,服务器无 Python;7 架构交叉编译 |
| **安全默认** | 令牌走 HttpOnly Cookie,登录失败节流,内部错误不外泄,能力收敛到最小 capability |

---

## 安装

> 需要 root 权限(脚本会检查;普通用户用 `sudo` 执行即可)。安装器发布在仓库 `dist` 分支,和 `sbx-core` 二进制处于**同一提交**,保证"脚本 ↔ 二进制"版本严格对应。

### 推荐:先校验再执行

最安全的方式——先把安装器和官方哈希下载下来,校验通过后再执行:

```bash
# 1) 下载安装器与官方哈希
curl -fsSLO https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/sbx.sh
curl -fsSLO https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/sbx.sh.sha256

# 2) 校验通过再执行(校验失败即中止,不要继续)
sha256sum -c sbx.sh.sha256 && sudo bash sbx.sh
```

`sbx.sh.sha256` 由 CI 在发布时生成并随二进制一同公布,因此校验的是"当前 dist 上真正在分发的那份脚本"。

### 一键安装(便捷,有风险)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

> ⚠️ `curl | bash` 这类方式**无法在下载环节校验脚本完整性**:一旦 DNS/TLS 被劫持或镜像被投毒,执行的就是被篡改的脚本。请仅在信任当前网络环境时使用,否则改用上面的"先校验再执行"。

### 让脚本自校验

把官方公布的哈希用环境变量传进来,脚本会**在做任何状态改动之前**先校验自身,不匹配立即中止:

```bash
SBX_SCRIPT_SHA256=<官方公布的 sha256> bash <(curl -fsSL <RAW_URL>)
```

- 未提供 `SBX_SCRIPT_SHA256` 时保持便捷行为(不校验)。
- 一旦提供,校验失败会明确报出**期望值与实得值**并中止。
- 注意:`curl | bash`(管道)方式下脚本无法回读自身,此时会**明确报错**而不是静默跳过校验。

安装器识别的环境变量(均做格式白名单校验,非法取值在任何改动前中止):

| 变量 | 作用 |
|---|---|
| `SBX_SCRIPT_SHA256` | 脚本自校验哈希 |
| `SBX_GH_PROXY` | GitHub 下载代理前缀(网络受限环境) |
| `SBX_ROOT` | 安装根目录(默认 `/etc/sbx` 等标准路径) |
| `SBX_SB_VERSION` | 固定 sing-box 版本(留空取最新稳定版) |
| `SBX_NO_SERVICE` | 只装不起服务(沙箱/测试用) |

### 安装器做了什么

安装完成后运行 `sbx` 进入管理菜单。整个流程:

1. **检测系统与 init** — Debian/Ubuntu、RHEL 系、Alpine;systemd / OpenRC。
2. **确认/安装 nftables** — 缺失或不可用即中止(这是硬性前置条件,见下)。
3. **下载 `sbx-core`** — 按架构从 `dist` 分支拉取,并用 `SHA256SUMS` 校验;不符即中止,**不影响已有安装**。
4. **安装 sing-box** — 按需升级内核(如 Snell 需要 ≥ 1.14)。
5. **生成计数规则** — 写 `nft.conf` 并应用 `sbx_traffic` 表。
6. **注册并启动三个服务** — `sbx-firewall`(计数规则)、`sing-box`(代理)、`sbx-panel`(面板)。

> 任意登录用户(`root` / `ubuntu` 等)均可安装:脚本要求 root,普通用户用 `sudo` 执行即可;服务由 init 以 root 运行,与登录用户名无关。

### 支持矩阵

nftables 是唯一后端,不存在自动选择或降级路径。下表"支持"指安装器已实现对应分支;`nftables` 一列是**硬性前置条件**。

| 发行版 | 版本 | init | 包管理器 | 状态 |
|---|---|---|---|---|
| Debian | 11 / 12 | systemd | apt | ✅ 支持(12 为真机验收环境) |
| Ubuntu | 20.04 / 22.04 / 24.04 | systemd | apt | ✅ 支持 |
| RHEL / Rocky / Alma | 8 / 9 | systemd | dnf / yum | ✅ 支持 |
| Fedora | 38+ | systemd | dnf | ✅ 支持 |
| Alpine | 3.18+ | OpenRC | apk | ✅ 支持(二进制用 musl 构建) |
| Debian | 10 | systemd | apt | ⚠️ 已 EOL,需改用 archive 源(见下) |

| 架构 | 二进制 | 备注 |
|---|:---:|---|
| x86_64 (amd64) | ✅ | |
| aarch64 (arm64) | ✅ | |
| armv7 | ✅ | |
| armv6 | ⚠️ | 纯 Go SQLite 需 `GOARM≥7`,armv6 未验证 |
| i386 (386) | ✅ | |
| s390x | ✅ | |
| riscv64 | ✅ | |

**已 EOL 的发行版(如 Debian 10)**:官方安全源已下线,`apt-get install` 会报 `404`。安装器会给出明确提示,但 **nftables 装不上就无法继续**——请先把软件源指向归档站再重试(更推荐直接升级到 Debian 12+):

```bash
# Debian 11 (bullseye) 示例:换归档源并关闭 Valid-Until 检查
sudo sed -i 's|deb.debian.org|archive.debian.org|g; \
             s|security.debian.org/debian-security|archive.debian.org/debian-security|g; \
             /bullseye-updates/d' /etc/apt/sources.list
echo 'Acquire::Check-Valid-Until "false";' | sudo tee /etc/apt/apt.conf.d/99no-check-valid-until
sudo apt-get update
```

**nftables 不可用时**:安装/升级明确报错并中止(绝不降级),提示对应安装命令(`apt install nftables` / `dnf install nftables` / `apk add nftables`)并要求内核支持。运行期若 `nft` 消失或权限不足,面板仍可访问(便于登录排查),但统计与策略会失败并在 `policy_error` 中如实呈现。

---

## 管理菜单

安装后直接运行 `sbx` 进入交互式菜单,常用操作都在这里(策略配额/IP 上限在 Web 面板里设,不进 CLI 菜单):

```
添加节点        1) VLESS + Reality  2) Shadowsocks 2022  3) Trojan  4) AnyTLS  5) Snell
删除节点        选择节点删除(可选是否一并清除其历史流量)
节点管理        改端口 / 改加密方式 / 改 Snell 版本 / 查看分享链接
流量统计        今日/累计、每日趋势、运行自检、清空统计
面板设置        改端口 / 采集间隔 / 仅本机或公网访问
服务管理        全部重启/停止/启动、重建计数规则、查看 sing-box 日志
升级 / 卸载     在线升级、彻底卸载
```

---

## 协议与节点

支持 5 类协议:**VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5 / v6**。

- **Snell** 需 sing-box ≥ 1.14,创建时自动升级内核;分享链接同时给出通用 URI 与 Surge 配置两种格式。
- 节点按 `nodes.json` 顺序显示,**ID 单调递增、不复用**。
- 添加节点是一个**事务**:生成候选配置 → `sing-box check` 校验 → 原子提交 → 失败回滚。提交前自动校正 `route.final` 悬空引用(`sing-box check` 不校验该引用,悬空会导致启动 FATAL 并触发回滚)。
- 节点凭据(密码 / UUID / 私钥)保存在 `nodes.json`(权限 `0600`),API **绝不下发**这些字段——`/api/nodes` 只返回脱敏后的公开信息。

---

## Web 面板

底部三页签,令牌登录(HttpOnly Cookie,`SameSite=Lax`,`Max-Age=7d`):

| 页签 | 内容 |
|---|---|
| **首页** | 节点卡片(实时速率 / 累计流量 / TCP·UDP 连接数 / 在线 IP / 配额状态)+ 顶部 KPI 汇总 |
| **每日** | 全节点流量趋势(近 60 天)与单节点详情 |
| **节点** | 节点管理抽屉——流量配额、IP 上限、重置已用流量、查看在线 IP |

实时性由两条通道保证:

- `/api/live` — 2 秒高频轮询,刷新速率与连接数。
- `/api/events`(SSE) — 推送在线 IP 增量,无需整页刷新。

**API 一览**(全部走统一鉴权,`GET /healthz` 除外):

| 路由 | 方法 | 说明 |
|---|---|---|
| `/healthz` | GET | 健康检查,免鉴权,返回 `{"ok":true}` |
| `/api/summary` | GET | 汇总:节点列表 + KPI + 策略状态(短 TTL 缓存) |
| `/api/live` | GET | 轻量实时:速率 + 连接数(短 TTL 缓存) |
| `/api/events` | GET | SSE,推送在线 IP 增量 |
| `/api/daily?days=N&scope=` | GET | 每日流量表(默认 30,钳制 `[1,365]`) |
| `/api/nodes` | GET | 脱敏节点列表 |
| `/api/export` | GET | CSV 导出全量流量 |
| `/api/nodes/<id>/policy` | PUT | 设置节点配额 / IP 上限 |
| `/api/nodes/<id>/quota/reset` | POST | 重置节点已用流量 |
| `/api/nodes/<id>/active-ips` | GET | 查看节点当前在线 IP |

---

## 流量统计原理

数据源是**内核 netfilter 计数器**(nftables named counter,表 `sbx_traffic`),不是估算。

- **rx** = 服务器接收(用户上传),**tx** = 服务器发送(用户下载);含包头,比客户端显示高约 2%–5%。
- **单调差分累加**:首次采集只入累计;计数器归零时补记当前值,不产生假峰值。
- **世代衔接**:规则集带 epoch 世代标记,重建后自动衔接,不丢计、不重复。
- **采集频率**:默认每 2 秒一次,速率按真实 `duration_ms` 计算,采样保留约 2 分钟。
- **跨天口径**:按面板时区(默认 `Asia/Shanghai`,时区数据内嵌进二进制)。
- **连接数**:读 `/proc/net/tcp[6]`、`/proc/net/udp[6]` 由采集线程缓存;UDP 为"可观测 socket/会话"口径,通配 UDP 入站(如 sing-box)不展开为客户端会话数。

存储为 SQLite(`traffic.db`,WAL 模式),`synchronous=NORMAL`(可用 `SBX_SQLITE_SYNCHRONOUS=FULL` 覆盖回零丢失模式)。

---

## 节点策略:配额与 IP 上限

在 Web 面板 → 节点卡片 → 管理里,为每个节点**独立**设置。两种策略默认都是"不限",旧节点升级后行为不变。

### 流量配额(Quota)

- 基于内核 byte counter 累计(GiB/TiB),达限只阻断目标节点端口(nft 双向 drop),不影响其它节点。
- 提高额度自动恢复;"重置已用流量"只清零额度使用量,**不删除**历史累计。
- 达限节点在面板显示"已暂停接入",状态实时联动。

### 同时在线 IP 上限(IP Limit)

- 按服务端可见的**公网源 IP** 统计(NAT 下多设备算一个出口 IP),支持 TCP/UDP、IPv4/IPv6。
- 达限只阻止**新 IP**,不随机踢已在线 IP;UDP slot 超时自动释放。
- 只发 SYN 未完成握手的 IP 拿到的是**临时名额**,真实客户端优先级更高,不会被扫描流量挤掉。
- **基于 conntrack 判活**:移动端异常断开(无 FIN)的连接,字节增量停止后按空闲窗口释放。内核未开 `nf_conntrack_acct`(Debian/Ubuntu 默认)时自动降级为"ESTABLISHED 即在线";安装器会尝试开启并持久化;conntrack 完全不可用时回退 `/proc` ESTABLISHED。
- 服务器自身发起的出站连接不计入客户端(即使目的端口与节点监听端口相同)。
- **内核执行**:nft allow set 只放行已获 slot 的 IP;新 IP 的 SYN 放行、established 数据拦截,避免"第二个 IP 永远连不上"的死锁。

### 边界与保证

- 策略规则写入 `/etc/sbx/policy.nft`(独立表 `sbx_policy`),与计数规则 `/etc/sbx/nft.conf`(表 `sbx_traffic`)**完全分离,互不覆盖**。
- 判活以 conntrack 为主数据源。内核只在存在引用 `ct` 的规则时才建 conntrack 条目,因此计数表里有一条 `sbx_ct` 链(`policy accept`,唯一动作是 `ct state new counter`)专门用于**激活跟踪**——它不做任何放行/拦截决策。
- 规则生成是**确定性**的:同一份配置反复保存产生字节一致的 `policy.nft`(端口升序),不会因 map 遍历顺序而无意义重写。
- enforcement 节流:达限翻转、受限节点集合变化、节点改端口会**立即**生效;仅在线 IP 集合(allow set)的增删在 3 秒窗口内合并应用,避免被扫描流量诱发高频整表重写。
- enforcement 生命周期:面板单独停止时策略**冻结在最后一轮状态**(fail-closed,不放开);仅"重建/清除计数规则"(`sbx --clear-firewall` / `sbx-core clear`)或**卸载**时,`sbx_policy` 表才与计数表一并从内核清除。
- `nodes.json` 损坏或不可读时,策略端点返回 **503** 并说明"配置文件不可用,策略维持上一轮状态",而不是误报 404;此期间 enforcement **不会 fail-open**。
- `sbx-core reset [scope]` 清空统计时会**同事务**清零对应节点的配额基线,避免"统计归零后配额长期失效"。

---

## 命令参考

```bash
# sbx(菜单入口 / 运维开关)
sbx                     # 管理菜单
sbx --update            # 在线升级(SHA256 校验 + 内容比对)
sbx --update --force    # 强制重装当前/最新版本
sbx --show              # 今日/累计流量
sbx --links             # 分享链接
sbx --panel-url         # 面板地址
sbx --apply-firewall    # 重建计数规则
sbx --clear-firewall    # 移除计数规则
sbx --uninstall         # 卸载
sbx --version           # 版本信息

# sbx-core(Go 后端,通常由服务/菜单调用)
sbx-core serve                    # 启动面板与采集器
sbx-core show | daily 60          # 命令行查看
sbx-core selftest                 # 计数器自检
sbx-core reset node:2             # 清空指定 scope 统计
sbx-core node list|links|add|edit|remove|sync   # 节点管理
```

---

## 配置与文件布局

```text
/usr/local/bin/sbx-core        Go 后端(含内嵌前端)
/usr/local/bin/sbx             菜单入口(安装器本体)
/etc/sbx/panel.json            面板配置(端口 / token / 时区 等)
/etc/sbx/nodes.json            节点数据(含凭据,0600)
/etc/sbx/state.json            ID 游标 / 分享地址
/etc/sbx/policy.nft            策略规则(配额阻断 / IP allow set,表 sbx_policy)
/etc/sbx/traffic.db            SQLite 流量库(WAL)
/etc/sbx/nft.conf              计数规则(nftables,表 sbx_traffic)
/etc/sing-box/config.json      sing-box 配置
/etc/systemd/system/sbx-{panel,firewall}.service
```

`panel.json` 主要键:`listen`(默认 `0.0.0.0`)、`port`(默认 `8080`)、`token`、`interval`(采集秒数,默认 `2`)、`tz`(默认 `Asia/Shanghai`)、`db`、`nodes_file`、`nft_conf`。路径类键在 `LoadStrict` 阶段强校验:必须为绝对路径、不含 `..`、不位于 `/proc` `/sys` `/dev`,非法配置 fail-closed。

---

## 安全性

- **令牌鉴权**:优先 `Authorization: Bearer <token>`,其次 HttpOnly Cookie `sbx_token`;**不再接受** `?token=` 查询参数(会泄漏进日志/Referer)。服务端未配置 token 时视为完全开放(仅建议内网/本机场景)。
- **登录节流**:同一来源 IP 连续失败 5 次后,后续每次失败强制等待 2 秒(5 分钟窗口累计,只惩罚失败、输对立即放行),挡住凭据喷洒与日志刷屏。来源只取 `RemoteAddr`,不信任可伪造的 `X-Forwarded-For`。
- **内部错误不外泄**:500/503 只返回稳定的 `error_code` + 通用文案 + `request_id`(响应头 `X-Request-Id`),完整错误只写服务端日志,不泄漏文件路径 / SQL 片段 / nft 报错。报障时把 `request_id` 提供给运维即可对齐日志。
- **敏感文件权限**:`nodes.json` / `state.json` / sing-box config 创建即 `0600`,已存在的宽权限文件会被收紧(即使 `umask 000` 也不留暴露窗口)。
- **最小能力**:面板服务 capability 收敛为 `CAP_NET_ADMIN`(nft 走 netlink)+ `CAP_NET_BIND_SERVICE`(绑定端口)。
- **HTTPS**:面板本身跑 HTTP;若套了 HTTPS 反代,设置 `secure_cookie` 让 Cookie 带 `Secure` 标记。
- **供应链**:CI 用 `govulncheck` 扫描依赖漏洞;GitHub Actions 全部 pin 到完整 commit SHA,防 action 被替换。

> ⚠️ 面板默认监听 `0.0.0.0:8080`。若配置了 `token` 则有鉴权保护;若把 token 置空,面板将**完全开放**——请仅在受信网络或配合防火墙使用,并优先设置强 token。

---

## 升级、版本化下载与回退

### 升级

```bash
sbx --update            # 更新 sbx.sh + sbx-core
sbx --update --force    # 强制重装当前/最新版本
```

保留节点配置、面板端口与流量历史;二进制**原子替换、失败回滚**;升级后自动重建计数规则。`sbx --update` 只在**内容不一致**时替换二进制(先校验 `SHA256SUMS`,再自检、比对 `APP_VERSION`,最后原子替换),因此"重装同一版本"是幂等的、不会无谓重启服务。

从 v3.0.8 及更早版本升级到 nftables-only 时,升级流程额外做三件事(都只碰 SBX 自己的东西):确认/安装 nftables(不可用则中止,此时数据未被改动)→ 摘掉 `panel.json` 里废弃的 `backend` / `ipt_script` 键(其余配置原样保留)→ 清理旧版残留(`/etc/sbx/iptables.sh`、`SBX_IN`/`SBX_OUT` 自定义链)。**不 flush 任何系统链、不改默认 policy、不动你自己的防火墙规则。**

### 版本化下载与回退

`dist` 分支是 **rolling latest**(安装器按 `SHA256SUMS` 做**内容比对**决定是否下载,不按版本号),因此不存在会失效的版本化链接。为可追溯与回退,发布时额外做三件事:

1. **历史归档** — 每次发布的"小型可追溯件"(`SHA256SUMS` + `sbx.sh` + `sbx.sh.sha256` + `MANIFEST`)按 `dist/archive/<version>/<commit7>/` 归档,保留最近 20 个版本。

   ```bash
   # 查看某次发布到底发了什么
   curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/archive/v3.0.10/<commit7>/MANIFEST
   ```

2. **版本 Tag** — 发布时若 `v<APP_VERSION>` 不存在则创建注解 Tag 指向当时 main(已存在则跳过,避免指针漂移)。

3. **由 commit 复现二进制** — 归档刻意**不含二进制**(8 架构约 80MB/次,会撑爆分支),但二进制可由 commit 完全复现,归档里的 `SHA256SUMS` 可核对复现结果:

   ```bash
   git clone https://github.com/k6nfmm7dbr-commits/sbx.git && cd sbx
   git checkout <commit7>
   ./scripts/build-release.sh dist
   sha256sum -c <(curl -fsSL .../archive/v<version>/<commit7>/SHA256SUMS)
   ```

---

## 构建与开发

```text
cmd/sbx-core/            入口
internal/                各模块(api / database / nodes / config / traffic / connection / firewall / service / policy)
internal/webui/static/   前端(go:embed)
installer-template.sh    sbx.sh 模板(与 sbx.sh 保持同步,CI 校验)
scripts/build-release.sh 七架构交叉编译 + SHA256SUMS
scripts/e2e_remote.sh    真机验收
tests/                   安装器与提交流程的 shell 回归测试
docs/AUDIT.md            行为审计(历史存档)
FUTURE_IMPROVEMENTS.md   已知取舍与待办(诚实标注未做的部分)
```

构建要求 **Go 1.27.1+**,产物为静态单二进制、`CGO_ENABLED=0`、无 cgo(连 SQLite 都是纯 Go)。

```bash
go test ./... && go test -race ./...      # 单测 + 竞态检测
./scripts/build-release.sh dist            # 七架构交叉编译
bash tests/baseline_test.sh                # 版本一致性 + 安装器 fail-closed + 权限
bash tests/commit_flow_test.sh             # 节点提交事务 / route.final 校正
```

`-race` 必跑:策略层是"reconcile 私有状态 + 每轮末发布不可变快照"的并发模型,读侧(HTTP / SSE)只看快照,`internal/policy/concurrency_test.go` 专门锁定这一点。

沙箱安装(不起服务,不碰系统):

```bash
SBX_ROOT=/tmp/sbx-test SBX_NO_SERVICE=1 \
SBX_CORE_BIN=$PWD/dist/sbx-core-linux-amd64 SBX_SB_BIN=/path/to/sing-box bash sbx.sh
```

---

## 分发机制

- **`main`** — 源码(含安装器与测试)。
- **`dist`** — 编译产物(`sbx-core-linux-<arch>` + `SHA256SUMS` + `sbx.sh` + `sbx.sh.sha256` + 清单),安装器按架构从这里下载。CI 每次重建都把 dist 树整体替换(`git read-tree --empty`),只保留清单内文件,不累积历史产物。

CI 门禁(`main` 推送全绿才发布):`gofmt` / `go vet` / `go test` / `go test -race` / `shellcheck` / sbx.sh 与模板同步(drift 检查)/ 安装器各流程回归测试 / Alpine musl 矩阵 / `govulncheck`。**纯文档改动**(未触及 `cmd/`、`internal/`、`installer-template.sh`、`scripts/`、`go.mod` 等)不会触发 dist 重建。

版本采用"版本号 + 提交"双重标识:`APP_VERSION` 不随每次改动递增,产物由 CI 从 `main` 每次推送重建。用户可见变更见 [CHANGELOG.md](CHANGELOG.md)。

---

## 当前版本

```text
v3.0.10
```

源码在 `main` 分支,二进制从 `dist` 分支分发(rolling latest)。

---

## 许可证

[MIT](LICENSE) © 2026 k6nfmm7dbr-commits
