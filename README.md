# SBX

> sing-box 节点一键部署 + 内核级精确流量统计面板

SBX 用一条命令在你的服务器上搭好 sing-box 代理节点,并附带一个实时 Web 面板,把每个节点的流量、在线 IP、连接数看得清清楚楚。后端是一个 **Go 静态单二进制**(`sbx-core`),前端经 `go:embed` 内嵌,**服务器上无需 Python、无需任何运行时**。

netfilter 后端是 **nftables-only**:流量统计、节点暂停、在线 IP 上限和节点限速全部由 nftables(表 `sbx_traffic` / `sbx_policy`)在内核里完成。不支持 iptables,也没有后端自动选择或回退——nftables 不可用时 SBX 会**明确失败并中止**,绝不静默降级或"假装成功"。

<p>
  <img alt="version" src="https://img.shields.io/badge/version-v3.0.30-blue">
  <img alt="go" src="https://img.shields.io/badge/Go-1.27.3%2B-00ADD8">
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
- [节点策略:暂停、IP 上限与限速](#节点策略暂停ip-上限与限速)
- [节点访问域名](#节点访问域名)
- [节点访问限制(只允许访问特定服务)](#节点访问限制只允许访问特定服务)
- [命令参考](#命令参考)
- [配置与文件布局](#配置与文件布局)
- [安全性](#安全性)
- [流量日报（飞书推送）](#流量日报飞书推送)
- [升级、版本化下载与回退](#升级版本化下载与回退)
- [构建与开发](#构建与开发)
- [分发机制](#分发机制)
- [许可证](#许可证)

---

## 为什么是 SBX

市面上的一键脚本大多把"能连上"当作终点,而流量统计要么靠客户端自报、要么靠周期性估算,既不准也容易被绕过。SBX 的设计取向不同:

- **统计来自内核,不是估算。** 每个节点的收发字节由 nftables named counter 直接计数,单调差分累加,不丢计、不重复、不产生假峰值。
- **限制由内核执行,不是提示。** 暂停节点会由 nft 双向阻断该节点端口;在线 IP 超上限只挡新 IP,节点限速由 nft policer 执行。
- **失败就是失败。** nftables 缺失、规则应用失败、conntrack 不可用,SBX 都会如实报错并在面板呈现(`policy_error`),而不是"警告一下然后当作成功"。
- **一个静态二进制,零运行时依赖。** 交叉编译到 7 种架构,`CGO_ENABLED=0`,连 SQLite 都是纯 Go 实现(`modernc.org/sqlite`)。

工程原则的优先级是:**正确性 > 兼容性 > 稳定性 > 可维护性 > 资源占用 > 性能**。

---

## 功能特性

| | |
|---|---|
| **6 种协议** | VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5/v6、SOCKS5,菜单化创建,自动生成分享链接 |
| **客户端订阅** | `/sub/<token>` 输出 base64 链接列表或 sing-box 完整配置,一条地址导入手机客户端;订阅令牌与面板登录令牌相互独立,可单独轮换 |
| **手机可用** | 面板自带 PWA manifest 与主屏图标,「添加到主屏幕」即全屏运行;仓库内附 WKWebView 壳的 Xcode 工程(`ios/`),可用免费 Apple ID 自签 |
| **内核级流量统计** | nftables named counter,非估算;单调差分累加,规则重建自动衔接,不丢计不重复 |
| **节点暂停** | 单节点启用 / 暂停,只屏蔽该节点端口,不删配置、不重启 sing-box |
| **在线 IP 上限(IP Limit)** | 按服务端可见的公网源 IP 统计,达限只挡新 IP;基于 conntrack 判活 |
| **节点限速(Rate Limit)** | 每节点独立 Mbps,上传/下载方向分别限速 |
| **实时 Web 面板** | 首页 / 每日 / 节点,令牌登录,SSE 实时推送在线 IP,`/api/live` 高频刷新速率 |
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

- 未提供 `SBX_SCRIPT_SHA256` 时，首次启动脚本保持便捷行为；正式在线升级不受此设置影响，升级器会先从官方 RAW 源下载 `sbx.sh.sha256` 校验新脚本，再覆盖安装器本体。
- 若官方哈希端点暂不可达，升级 fail-closed 中止；可从可信渠道取得官方哈希后用 `SBX_SCRIPT_SHA256=<官方哈希> sbx --update` 显式重试。
- 在线升级仍可通过 `SBX_GH_PROXY` 加速脚本字节下载，但该代理不会用于获取信任哈希；官方哈希和镜像下载内容不一致时升级会中止。
- 一键启动脚本的显式 `SBX_SCRIPT_SHA256` 校验仍适用；`curl | bash` 管道方式无法回读自身，会明确报错而不静默跳过。

安装器识别的环境变量(均做格式白名单校验,非法取值在任何改动前中止):

| 变量 | 作用 |
|---|---|
| `SBX_SCRIPT_SHA256` | 脚本自校验哈希 |
| `SBX_GH_PROXY` | GitHub 下载代理前缀(网络受限环境,仅大文件请求使用) |
| `SBX_RAW_BASE` | 可选大文件 RAW 下载镜像；校验清单与脚本哈希仍固定从官方源读取 |
| `SBX_ROOT` | 安装根目录(默认 `/etc/sbx` 等标准路径) |
| `SBX_SB_VERSION` | 固定 sing-box 版本(留空取最新稳定版) |
| `SBX_NO_SERVICE` | 只装不起服务(沙箱/测试用) |

### 安装器做了什么

安装完成后运行 `sbx` 进入管理菜单。整个流程:

1. **检测系统与 init** — Debian/Ubuntu、RHEL 系、Alpine;systemd / OpenRC。
2. **确认/安装 nftables** — 缺失或不可用即中止(这是硬性前置条件,见下)。
3. **下载 `sbx-core`** — 二进制可走 `SBX_GH_PROXY` 镜像加速，但其 `SHA256SUMS` 从官方 RAW 源直连获取；校验不符即中止，**不影响已有安装**。二进制与校验清单不再同源于可控镜像。
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

`sbx` 主菜单只保留面板没有的系统设置与运维、检查更新、卸载。添加/查看/编辑/删除节点都在面板新增的「配置」页完成；`sbx-core node` 子命令仍保留，供脚本及高级运维调用。

```text
1) 系统设置与运维    面板访问设置 / 分享地址 / 服务启停与日志 / 统计自检与清空
2) 检查更新
3) 卸载
0) 退出
```

首页流量总览和每日趋势从面板查看。节点创建、分享链接、修改与删除统一在「配置」页；启用/暂停、IP 上限和限速在面板的节点卡片「管理」中设置。`sbx-core show` / `daily` 仍可用于终端、脚本和自动化。


---

## 协议与节点

支持 6 类协议:**VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS、Snell v5 / v6、SOCKS5**(SOCKS5 支持用户名密码,留空则为开放代理,由使用者自行承担暴露风险)。

- **Snell** 需 sing-box ≥ 1.14,创建时自动升级内核;分享链接同时给出通用 URI 与 Surge 配置两种格式。
- 节点按 `nodes.json` 顺序显示,**ID 单调递增、不复用**。
- 添加节点是一个**事务**:生成候选配置 → `sing-box check` 校验 → 原子提交 → 失败回滚。提交前自动校正 `route.final` 悬空引用(`sing-box check` 不校验该引用,悬空会导致启动 FATAL 并触发回滚)。
- 节点凭据(密码 / UUID / 私钥)保存在 `nodes.json`(权限 `0600`),API **绝不下发**这些字段——`/api/nodes` 只返回脱敏后的公开信息。

---

## Web 面板

浅色主题保持不变。首页从实时速率与现有 KPI 直接开始（不再显示品牌/连接状态顶栏）；桌面速率卡采用左右分栏，手机端上传/下载并列。新增「配置」页签把既有节点配置操作迁入面板；原有统计字段、IP 策略和限速逻辑不变。

底部四页签,令牌登录(HttpOnly Cookie,`SameSite=Lax`,`Max-Age=7d`):

| 页签 | 内容 |
|---|---|
| **首页** | 节点卡片 + 顶部 KPI 汇总。卡片展示累计/今日流量、限速、TCP/UDP；在线 IP 行和状态徽标；暂停节点明确显示“已暂停” |
| **每日** | 全节点流量趋势(近 180 天)与单节点详情 |
| **节点** | 单节点流量趋势与详情 |
| **配置** | 节点列表、新增、分享链接查看、编辑与删除;页面末尾是**客户端订阅**地址 |

删除节点会保留该节点的累计流量总额（`totals`），默认清理每日与采样历史（`daily`/`samples`）。唯一一次文字验证会明确说明累计流量保留、其他历史将清除；必须输入「我已确定」才会继续。API 默认相同；显式指定 `clear_history=0` 可同时保留每日与采样历史。

实时性由两条通道保证:

- `/api/live` — 2 秒高频轮询,刷新速率与连接数。
- `/api/events`(SSE) — 推送在线 IP 增量,无需整页刷新。

**API 一览**(全部走统一鉴权,`GET /healthz` 除外):

| 路由 | 方法 | 说明 |
|---|---|---|
| `/healthz` | GET | 健康检查,免鉴权,恒为 HTTP 200 + `{"ok":true}`;降级时仅追加 `collector_ok:false` / `policy_ok:false` 与 `sample_age_s`,错误原文只写日志 |
| `/api/summary` | GET | 汇总:节点列表 + KPI + 策略状态(短 TTL 缓存) |
| `/api/live` | GET | 轻量实时:速率 + 连接数(短 TTL 缓存) |
| `/api/events` | GET | SSE,推送在线 IP 增量 |
| `/api/daily?days=N&scope=` | GET | 每日流量表(默认 30,钳制 `[1,365]`); `scope` 仅允许空值或 `node:<正整数>` |
| `/api/nodes` | GET / POST | 脱敏节点列表 / 新增节点（服务器生成节点密钥） |
| `/api/logout` | POST | 需鉴权;使当前浏览器端的 `sbx_token` Cookie 过期 |
| `/api/export` | GET | CSV 导出全量流量 |
| `/api/nodes/<id>` | PUT / DELETE | 修改节点可编辑项 / 删除节点（累计 `totals` 保留，默认清理 `daily/samples`；`clear_history=0` 可保留） |
| `/api/nodes/<id>/links` | GET | 获取该节点分享链接（及 Snell Surge 配置） |
| `/api/nodes/<id>/policy` | GET / PUT | 读取或设置暂停状态、IP 上限和限速 |
| `/api/nodes/<id>/active-ips` | GET | 查看节点当前在线 IP |
| `/api/sub` | GET | 读取订阅地址、可导入节点数与跳过原因(需登录) |
| `/api/sub/reset` | POST | 轮换订阅令牌,旧地址立即失效(需登录) |
| `/sub/<token>` | GET | 订阅下载,**不走 Cookie 鉴权**,令牌在路径里;默认返回 base64 链接列表,`?target=singbox` 返回 sing-box 完整配置 |

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

## 节点策略:暂停、IP 上限与限速

在 Web 面板 → 节点卡片 → 管理里为每个节点分别设置。暂停不会删除 sing-box 配置，也不会重启 sing-box；通过独立 nft 策略表同时屏蔽该节点端口的入站与出站流量，暂停状态写入 SQLite，服务重启后仍保持暂停。点击启用即可恢复原节点。

### 同时在线 IP 上限(IP Limit)

- 按服务端可见的**公网源 IP** 统计(NAT 下多设备算一个出口 IP),支持 TCP/UDP、IPv4/IPv6。
- 达限只阻止**新 IP**,不随机踢已在线 IP;UDP slot 超时自动释放。
- 只发 SYN 未完成握手的 IP 拿到的是**临时名额**,真实客户端优先级更高,不会被扫描流量挤掉。
- **基于 conntrack 判活**:移动端异常断开(无 FIN)的连接,字节增量停止后按空闲窗口释放。内核未开 `nf_conntrack_acct`(Debian/Ubuntu 默认)时自动降级为“ESTABLISHED 即在线”;安装器会尝试开启并持久化;conntrack 完全不可用时回退 `/proc` ESTABLISHED。
- 服务器自身发起的出站连接不计入客户端(即使目的端口与节点监听端口相同)。
- **内核执行**:nft allow set 只放行已获 slot 的 IP;新 IP 的 SYN 放行、established 数据拦截,避免“第二个 IP 永远连不上”的死锁。

### 节点限速(Rate Limit)

- 每节点独立设置 Mbps,上传、下载方向分别使用 nft policer。
- policer 通过丢弃超额包实现,不是 tc/qdisc 的排队整形;TCP 会通过拥塞控制回退。
- 暂停节点时暂停规则优先拦截全部双向流量;解除暂停后原 IP 上限与限速配置继续生效。

### 边界与保证

- 策略规则写入 `/etc/sbx/policy.nft`(独立表 `sbx_policy`),与计数规则 `/etc/sbx/nft.conf`(表 `sbx_traffic`)**完全分离,互不覆盖**。
- 判活以 conntrack 为主数据源。内核只在存在引用 `ct` 的规则时才建 conntrack 条目,因此计数表里有一条 `sbx_ct` 链(`policy accept`,唯一动作是 `ct state new counter`)专门用于**激活跟踪**——它不做任何放行/拦截决策。
- 规则生成是**确定性**的:同一份配置反复保存产生字节一致的 `policy.nft`(端口升序),不会因 map 遍历顺序而无意义重写。
- 暂停/恢复、IP 受限节点集合变化、节点改端口会**立即**应用;仅在线 IP allow set 的增删在 3 秒窗口内合并,避免扫描流量诱发高频整表重写。
- enforcement 生命周期:面板单独停止时策略**冻结在最后一轮状态**(fail-closed,不放开);仅“重建/清除计数规则”(`sbx --clear-firewall` / `sbx-core clear`)或**卸载**时,`sbx_policy` 表才与计数表一并从内核清除。
- `nodes.json` 损坏或不可读时,策略端点返回 **503** 并说明“配置文件不可用,策略维持上一轮状态”,而不是误报 404;此期间 enforcement **不会 fail-open**。
- `sbx-core reset [scope]` 只清统计数据,保留节点暂停、IP 上限与限速设置。

---

## 节点访问域名

「节点」页选中某个节点后,上方是**带标记的日历**:有流量记录的日期可以点,
选中哪一天,上方汇总和下面的域名列表就一起切到那一天。默认选中今天。

域名列表显示**选中那天**该节点访问过哪些域名、各用了多少流量、连接了多少次,
按流量从大到小排列。域名明细与流量趋势一样保留 **180 天**,可以回看任意一天。

### 数据是怎么来的

来自 **sing-box 的实时连接表**:每条连接上都带着目标域名、已传输字节数,以及它属于
哪个入站(节点)。这是 sing-box 架构下唯一能同时给出这三者的地方——nftables 计数器
只有端口维度,看不到域名。

因此 SBX 会为 sing-box 写入一个管理接口(`experimental.clash_api`),它的安全约束是:

- **默认对外监听**(0.0.0.0),端口由内核分配,自带随机强令牌认证
- **带 32 字节随机密钥**,无密钥访问返回 401

这个接口具备控制能力(可切断连接、切换出站),所以绝不能对外暴露。已有配置不会被覆盖;
升级后第一次启动面板时会自动补上(会重启一次 sing-box,连接短暂中断),失败则降级。

采集每 2 秒一次,按连接做差分累加,与计数入账在同一个事务里。

### 精度:流量是估算,次数是准确的

连接表**只包含当前存活的连接**,连接一断立刻消失。因此:

| 连接类型 | 统计情况 |
|---|---|
| 存活超过约 2 秒(下载、视频、长连接) | 流量准确 |
| 更短的连接 | 会被漏掉 |

界面底部因此单列一行 **「未归类(短连接)」** = 该节点总流量 − 已归类到域名的部分。
**这一行的数值越接近上方合计,说明域名明细越不完整**;数值很小则可以放心按它判断谁在吃流量。

「次数」列是**准确的**(每条连接只计一次),可作为流量估算的交叉参考。

### 看不到域名怎么办

部分客户端会先自行解析域名、只把 IP 发给服务端。这种情况下域名规则无从判断,
界面会退化为显示目标 IP。要看到域名,需让客户端把域名交给代理(多数客户端的默认行为)。

## 节点访问限制(只允许访问特定服务)

给**单个节点**配白名单:只有勾选的服务能访问,其余一律拒绝。典型用法是一个账号自用、
一个给员工——自用节点不限制,员工节点只放行办公与常用服务。

限制**按节点隔离**(SBX 每个节点的 inbound 标签独立),两者共存于同一个 sing-box 进程,
互不影响。

### 配置

管理菜单 → 节点管理 → 编辑节点 → 勾选「只允许访问特定服务」→ 选择服务 → 保存。

可选服务来自内置清单(上游 `blackmatrix7/ios_rule_script` 的全部规则集,去掉国内站点
与广告/拦截类,共四百余项),按「常用」+ 首字母分组。列表默认折叠、支持搜索——
直接输入 `netflix` 或 `telegram` 即可定位。

清单里有几处归并,是为了少让人纠结:

- **AI 是一整项**。上游把 OpenAI / Claude / Gemini / Copilot 拆成八个文件,这里合并成
  一个「AI」,勾一次就全覆盖。
- **不收录国内站点**。这些站点本来就不需要走代理,放进白名单只会让列表变长。

首次启用会自动从上游下载规则文本并转换、缓存在 sing-box 配置同级的 `rulesets/` 目录,
之后重启不再依赖网络。

服务器连不上外网时,可以手动离线导入:把 Surge 格式的 `<名称>.list` 放进该目录,
sbx 会在下次保存节点时把它转换成 sing-box 规则集。**已存在的规则集文件不会被覆盖**,
删除它才会重新生成。注意文件名用的是规则集名本身(如 `Netflix.list`),内容按行写,
例如:

```text
# NAME: Netflix
DOMAIN-SUFFIX,netflix.com
IP-CIDR,203.0.113.0/24,no-resolve
```

命令行等价写法:

```bash
sbx-core node edit <id> --allow=Google,TikTok   # 设置白名单
sbx-core node edit <id> --allow=                # 解除限制
sbx-core node rulesets                          # 提前把需要的规则集下载/转换好
```

### 生效方式

为每个受限节点生成五条路由规则,**顺序即优先级**:

1. **放行 DNS**(53 端口)
2. **放行客户端延迟探测域名**(精确域名与域名后缀各一条)
3. **白名单内的目标放行**
4. **其余一律拒绝**

第 1 条是必需的:不少客户端会把域名解析也交给代理,若 DNS 被兜底规则拒掉,用户连
「域名 → IP」都做不到,表现为「这个节点什么网站都打不开」——而症状与白名单里勾了
什么毫无关系,极难排查。

第 2 条是为了**让客户端能测出延迟**。客户端的「延迟测试」通常是一次真实的 HTTP 请求,
白名单模式下会落到兜底拒绝上,节点在客户端里就一直显示超时——用户看到的是「节点挂了」,
实际只是探测请求被拒。这些目标内置放行、不需要勾选:

```text
www.gstatic.com  connectivitycheck.gstatic.com  www.google.com
cp.cloudflare.com  speed.cloudflare.com
www.apple.com  captive.apple.com
www.msftconnecttest.com  www.msftncsi.com
connectivitycheck.platform.hicloud.com  wifi.vivo.com.cn
connect.rom.miui.com  connectivitycheck.android.com
www.baidu.com
（后缀：gstatic.com / msftconnecttest.com / msftncsi.com）
```

任何其它节点完全不受影响:五条规则都带 `inbound` 条件,只匹配该节点。

### 需要注意

- **只在用这个节点时生效**。员工换用别的节点或直连,不受任何限制。
- **依赖客户端把域名交给服务端**。若客户端先自行解析域名、只把 IP 发过来,域名规则
  会失效。规则集自带一部分 IP 段能兜住主要服务,但不完整。如果配完发现「勾了的服务
  也打不开」,请先检查客户端的 DNS 模式。
- **「只允许」的代价**:Google 搜索能用、结果页能显示,但点进搜索结果里的第三方网站
  会被拒绝。这是白名单模式的固有效果,不是故障。若目标是「别刷视频/别玩游戏」而非
  「只允许办公」,白名单会造成困扰。
- **延迟测试能用,IP 形式的探测不行**。上一条列出的探测域名已经内置放行;但
  `http://1.1.1.1/cdn-cgi/trace` 这类**直接打 IP** 的探测会失败。原因是实测确认过:
  路由规则里只要出现 `ip_cidr` 条件,sing-box 就会先把连接的域名解析掉、目标变成 IP,
  于是同一份规则里靠前的域名规则**全部失配**——整个白名单会失效。两害相权取其轻,
  所以只放行域名维度。主流客户端(Clash / sing-box / Surge / Loon / Quantumult X /
  Windows)默认都用域名。
- 规则集来自第三方公开列表,**已缓存的文件不会自动更新**,删除后下次保存会重新生成。
- 上游用的是 Surge 格式的文本规则,sbx 会在本地转换成 sing-box 规则集。转换是**有损的**:
  `IP-ASN`、`USER-AGENT`、`PROCESS-NAME`、`URL-REGEX` 这些条件在 sing-box 的规则集里
  没有对应表达,会被跳过(少数规则集靠 `IP-ASN` 覆盖不少 IP,例如 Cloudflare、
  Telegram,勾它们时覆盖会比上游原文略少)。
- **从旧版本升级不用担心白名单失效**:旧清单里的名字会自动映射到新清单——`Github` →
  `GitHub`、`OpenAI`/`Claude` → `AI`、`xAI` → `Twitter`(Grok 的域名在 Twitter 规则集里)、
  `TronLink` → `Crypto`;不再收录的国内站点(如 `Bilibili`)会被丢弃并记进日志。
  映射在读取时完成,不会改动你的 `nodes.json`;下次在面板上保存节点时会自然写回。
- 规则集取不到时**整次保存失败并回滚**,不会把缺失的配置写进 sing-box——配置里引用
  的是本地文件,缺文件会让 sing-box 起不来,那是全节点一起挂。

## 命令参考

```bash
# sbx(菜单入口 / 运维开关)
sbx                     # 管理菜单
sbx --update            # 在线升级(SHA256 校验 + 内容比对)
sbx --update --force    # 强制重装当前/最新版本
sbx --show              # 今日/累计流量
sbx-core node links     # CLI/自动化查看分享链接
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
/etc/sbx/policy.nft            策略规则(节点暂停 / IP allow set / 限速,表 sbx_policy)
/etc/sbx/traffic.db            SQLite 流量库(WAL)
/etc/sbx/nft.conf              计数规则(nftables,表 sbx_traffic)
/etc/sing-box/config.json      sing-box 配置
/etc/systemd/system/sbx-{panel,firewall}.service
```

`panel.json` 主要键:`listen`(默认 `0.0.0.0`,对外监听,自带随机强令牌认证)、`port`(默认 `8080`)、`token`、`interval`(采集秒数,默认 `2`)、`tz`(默认 `Asia/Shanghai`)、`db`、`nodes_file`、`nft_conf`。需从外网访问时，显式将 `listen` 配为 `0.0.0.0` 或具体网卡地址，并强烈建议置于 HTTPS 反代之后、设置 `secure_cookie=true`。路径类键在 `LoadStrict` 阶段强校验:必须为绝对路径、不含 `..`、不位于 `/proc` `/sys` `/dev`,非法配置 fail-closed。

`trusted_origin`(默认不配置)与 `secure_cookie=true` 一样属于**可选键**，不写就没有这个键。它的作用是声明"浏览器实际访问面板的公开地址"，只影响状态变更接口的 CSRF 来源校验：默认要求 Origin/Referer 与请求 Host 的 host 与端口都一致；当反向代理把 Host 改写成带后端端口（如 `panel.example.com:8080`）时，浏览器发出的 Origin 是 `https://panel.example.com`，严格比较会拒绝请求，此时把 `trusted_origin` 配成 `https://panel.example.com` 即可放行该来源。取值必须是绝对 `http(s)` URL，可带路径但会被忽略；非法取值在 `LoadStrict` 阶段 fail-closed 拒绝启动（避免"配了却没生效"这种更难排查的状态）。它只放宽到"这一个确切的来源"，不影响其它校验。

---

## 安全性

- **令牌鉴权**:优先 `Authorization: Bearer <token>`,其次 HttpOnly Cookie `sbx_token`;**不再接受** `?token=` 查询参数(会泄漏进日志/Referer)。面板提供 `POST /api/logout` 清除当前浏览器的 Cookie；阶段 A 不维护服务端会话，已复制的 Bearer/token 仍有效，改 token 后原凭据才会失效。服务端未配置 token 时视为完全开放(仅建议内网/本机场景)。
- **登录节流**:同一来源 IP 连续失败 5 次后,后续每次失败强制等待 2 秒(5 分钟窗口累计,只惩罚失败、输对立即放行),挡住凭据喷洒与日志刷屏。来源只取 `RemoteAddr`,不信任可伪造的 `X-Forwarded-For`。
- **CSRF 来源校验**:只对已鉴权 API 的 POST/PUT/DELETE/PATCH 检查 Origin/Referer；**默认要求来源与请求 Host 的 host 与端口都一致**（同主机名不同端口也会被拒绝——浏览器按 scheme+域名判定 same-site 时忽略端口，端口回退会让这层兜底失效），不匹配返回 403 + `cross_site_blocked`。没有这两个浏览器头的 curl/自动化客户端仍可使用；GET/HEAD、`/healthz`、登录页与静态资源不受影响。反向代理改写了 Host（浏览器看到 `https://panel.example.com`，转发到后端时 Host 变成 `panel.example.com:8080`）时严格比较会拒绝请求，这类部署需显式配置 `trusted_origin`（见配置章节），用声明式白名单取代隐式放宽。
- **API 限速**:`/api/*` 按 `RemoteAddr` IP 执行 50 req/s、burst 100 的宽松令牌桶，支持正常多标签页轮询；追踪状态最多 8192 个来源并定期回收，满表时拒绝新的来源，不信任 `X-Forwarded-For`。
- **反向代理下的来源口径**:API 限速与登录节流都只按 `RemoteAddr` 计算来源 IP，且**刻意不采信** `X-Forwarded-For`——它可由客户端伪造，采信会同时绕过限速与登录节流。因此置于反向代理之后时，所有客户端共享同一个来源 IP，即共享同一个限速桶与登录失败节流。面板自身轮询很轻（约 0.625 req/s/标签页），单机与少量管理员场景无影响；多管理员经同一反代访问时需知晓这一行为。
- **安全审计日志**:记录登录成败、节点创建/编辑/删除、策略修改、CSV 导出、CSRF 拦截与限速拦截，含 request_id、来源 IP、路径和结果；日志明确禁止记录 token、节点密码、PSK、UUID 与私钥。
- **内部错误不外泄**:500/503 只返回稳定的 `error_code` + 通用文案 + `request_id`(响应头 `X-Request-Id`),健康探针降级时只回布尔状态、采样年龄，完整错误只写服务端日志,不泄漏文件路径 / SQL 片段 / nft 报错。报障时把 `request_id` 提供给运维即可对齐日志。
- **敏感文件权限**:`nodes.json` / `state.json` / sing-box config 创建即 `0600`,已存在的宽权限文件会被收紧(即使 `umask 000` 也不留暴露窗口)。
- **最小能力**:面板服务 capability 收敛为 `CAP_NET_ADMIN`(nft 走 netlink)+ `CAP_NET_BIND_SERVICE`(绑定端口)。
- **HTTPS**:面板本身跑 HTTP。默认对外监听 `0.0.0.0`（自带随机强令牌认证）；需要收回本机时把 `listen` 改为 `127.0.0.1` 并经 SSH 隧道访问；公网部署强烈建议置于 HTTPS 反代之后、设置 `secure_cookie=true`。设置后 Cookie 带 `Secure` 与 HSTS 响应头；不设置时 HTTP Cookie 会以明文传输。设置后 Cookie 带 `Secure` 与 HSTS 响应头；不设置时 HTTP Cookie 会以明文传输。
- **供应链**: `sbx-core` 大文件可经 `SBX_GH_PROXY` / `SBX_RAW_BASE` 加速下载，但**信任决策依据一律不经镜像**：`SHA256SUMS` 与 `sbx.sh.sha256` 固定从官方 RAW 源直连获取，防止自定义镜像同时投毒二进制和清单形成自证式校验。在线升级固定直连官方脚本 URL 并用官方 `sbx.sh.sha256` 校验，校验失败或取不到可信哈希时 fail-closed，不覆盖现有安装。用 `curl | bash` / `bash <(curl ...)` 一键安装时，需要重新落盘的那份安装器副本同样直连官方源并在校验通过后才写入（它随后会成为常驻的 `sbx` 命令）；校验不通过或官方源不可达时只告警并跳过落盘，不会装入未经验证的脚本。显式 `SBX_SCRIPT_SHA256` 可作为独立哈希锚。此校验仍信任 GitHub 官方仓库/账号，官方源被攻破不在哈希清单本身的防护范围内；后续可增加独立签名密钥。CI 用 `govulncheck` 扫描依赖漏洞，GitHub Actions 全部 pin 到完整 commit SHA。

> ⚠️ 面板默认监听 `0.0.0.0`,自带随机强令牌认证。公网部署请确认令牌强度,并强烈建议置于 HTTPS 反代之后、启用 `secure_cookie=true`;需要仅本机访问时把 `listen` 改为 `127.0.0.1`。

---

## 流量日报（飞书推送）

每天在固定时刻，把「前一天全天流量 + 全部历史账本」渲染成一张飞书卡片推到群里。
只有一个渠道（飞书群机器人），只做一件事：让流量记录离开这台机器。

### 三句话说明白

- **卡片自带全部历史**：不是只报前一天，而是把有数据的每一天都列出来。由此带来两个好处——某次推送失败不会丢数据（下一天的卡片里它还在），也就不需要增量同步或补偿队列。
- **采集与推送完全解耦**：推送不做采集，采集也不依赖推送。不配推送，流量统计照常运行。
- **凭据只进不出**：webhook 域名被强制锁定为 `open.feishu.cn`；签名密钥在 `notify-config show` 里一律显示为 `***`。

### 快速上手

1. 飞书里建一个群（只有你自己也行）→ 群设置 → 群机器人 → 添加机器人 → **自定义机器人**
2. 复制它给出的 Webhook 地址
3. 管理菜单：「系统设置与运维 → 4) 流量日报配置 → 配置 / 修改」
4. 选「发送测试推送」，确认群里能收到

命令行等价写法：

```bash
sbx-core notify-config enable '<webhook>' '<签名密钥, 可留空>' '00:05'
systemctl restart sbx-panel
sbx-core notify-test
```

### 发送时机与内容

| 场景 | 触发条件 | 卡片主体 |
|---|---|---|
| **每日日报** | 每天 `daily_at`（默认 `00:05`，面板时区） | 前一日全天流量（已冻结的终值） |
| **异常补推** | 启动时发现上次是崩溃/断电 | 今日 00:00 至重启时刻 |

「异常补推」的判据：进程在 `meta` 表里维护一个运行标记，**正常收尾时清除**；崩溃 / 断电 / `kill -9` 会留下残留，下次启动据此补推一次（同一天只补一次，避免反复重启刷屏）。普通的重启与升级**不会**触发补推。

> ⚠️ 整机损毁（连数据盘都没了）时不会有任何推送——那天就是没有了。这是"消息里不内嵌可回放数据"这个选择的必然代价。顺带一提：**哪天早上没收到日报，本身就是"服务器出事了"的信号**。

### 卡片形态

```
╭──────────────────────────────────────╮
│ 📊 SBX 流量日报 · 10月13日 周二        │
├──────────────────────────────────────┤
│   总量         上传         下载       │
│  5.20 GiB    3.10 GiB    2.10 GiB    │
├──────────────────────────────────────┤
│ 历史记录 · 累计 187.4 GiB · 45 天      │
│                                      │
│ 10-13   5.20 GiB                     │
│ 10-12   4.10 GiB                     │
│   ⋮                                  │
│ 08-25   6.30 GiB                     │
╰──────────────────────────────────────╯
```

历史按日期**倒序**（最新在上）。超过 60 天时，最近 60 天逐日列出，更早的部分按月归并（`2026-08 · 32 天 · 96.3 GiB`）——飞书对单条消息有大小限制，超限会导致推送**静默失败**，宁可有损展示也不能断供。

### 配置项

`panel.json` 的 `notify` 段：

| 键 | 必填 | 说明 |
|---|---|---|
| `enabled` | 是 | 关闭时保留其余字段，允许暂存半配置 |
| `type` | 否 | 固定 `feishu`，省略即默认 |
| `webhook` | 启用时必填 | 必须 https + `open.feishu.cn` + `/open-apis/bot/v2/hook/` 前缀 |
| `secret` | 否 | 飞书机器人「签名校验」密钥；填了则每条消息带签名 |
| `daily_at` | 否 | 默认 `00:05`，24 小时制 HH:MM |
| `timeout_seconds` | 否 | 默认 `8`，范围 3–60 |

非法配置在 `LoadStrict` 阶段 fail-closed 拒绝启动——"配了却没生效"比"配置错误直接拒绝启动"危险得多。

> 升级说明：旧版的多渠道配置（`channels` 数组）会被识别并按「未启用」处理，**不会**阻断面板启动，但会在日志里以 Error 级提示重新配置。这是刻意的例外——配置格式变更若让面板整体启动不了，代价远大于"暂时没有日报"。

### 签名校验（建议开启）

在飞书机器人设置里开启「签名校验」后把密钥填进来：即使 webhook 地址泄露，别人也无法往你的群发消息。注意飞书的签名算法与常规 HMAC 写法**相反**：

```
sign = base64(HMAC-SHA256(key = timestamp + "\n" + secret, data = ""))
```

密钥是「时间戳 + 换行 + secret」这个拼接串，待签名的数据是**空**。写反了不会报错，只会一直返回签名校验失败。仓库里有一条固定向量的单测把它钉死。

### 安全与边界

- webhook 主机强制为 `open.feishu.cn`：只校验"是合法 URL"不够，那会让任何地址通过，凭据就有被送到第三方的机会。
- 所有进入日志的错误都经脱敏：`net/http` 的 `*url.Error` 会带上完整 URL，直接打印会把 hook token 写进日志。
- 响应体做业务语义判定：只看 HTTP 200 会把"业务拒绝"当成投递成功；拿不到明确成功码一律判失败。
- 推送失败不影响采集、策略与 HTTP 服务；推送对计费数据（`daily`/`totals`）**只读**，只写 `meta` 表里的 `notify_*` 键。

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
   curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/archive/v3.0.11/<commit7>/MANIFEST
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

构建要求 **Go 1.27.3+**,产物为静态单二进制、`CGO_ENABLED=0`、无 cgo(连 SQLite 都是纯 Go)。

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
v3.0.30
```

源码在 `main` 分支,二进制从 `dist` 分支分发(rolling latest)。

---

## 许可证

[MIT](LICENSE) © 2026 k6nfmm7dbr-commits
