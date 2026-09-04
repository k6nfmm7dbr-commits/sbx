# FUTURE_IMPROVEMENTS

记录评估过但**暂不实施**的改进项（稳定性优先于理论收益）。

## 1. nftables 直接 netlink 读取（第二阶段优化）

现状：`internal/firewall.Nft.Read` 通过 `exec nft -j list counters table inet sbx_traffic`
读取并解析 JSON（与旧 Python 行为完全一致，两阶段迁移第一阶段）。

可选方案：`github.com/google/nftables` 直连 netlink，省去每 2 秒一次 fork/exec。

未实施原因：

- 现有 exec 路径在真机实测中开销可忽略（单次 ~5ms，CPU 占用 <0.1%）；
- netlink 库引入额外依赖面（内核版本兼容、权限、表/对象缓存语义），
  而"计数绝对不能失真"是本项目最高优先级；
- `google/nftables` 对 named counter 的批量读取 API 与 `nft -j` 输出存在
  细微口径差异需要逐一验证（如 epoch counter 的出现顺序）。

重启该工作的入口：给 `firewall.Backend` 增加第二个 **nftables** 实现 `NftNetlink`
（不是别的 netfilter 后端——SBX 是 nftables-only），用构建标签灰度切换；
先在 shadow 模式双读比对一个版本周期。注意 `firewall.Backend` interface 现在只为
「测试注入 fake backend」和这类同后端不同实现保留，绝不用于运行时后端选择。

## 2. armv6 的说明

Go 编译器支持 linux/arm（GOARM=6），modernc.org/sqlite v1.34.5 在 GOARM=6 下
交叉编译通过（已验证），因此 v3.0.0 继续发布 armv6 产物。若未来驱动升级后
GOARM=6 不再可用，将明确在 README 标注并在安装器给出提示，而不是静默失败。

## 3. 磁盘 web_root 自定义 UI

v2.x 支持从磁盘 `$APP_DIR/web` 读前端；v3.0.0 改为 //go:embed 内嵌。
原因是升级载体变为 sbx-core 二进制：若磁盘文件优先，旧文件会永久遮蔽新 UI。
如有用户自定义需求，将来可加 `web_override` 配置键（显式 opt-in，默认关闭）。

## 4. /api/live 推送化

前端目前 2s 轮询 `/api/live`。服务端已是内存快照读取（无 SQL），
轮询成本极低；WebSocket/SSE 会增加连接管理与代理兼容性问题，暂不做。
（注：在线 IP 已走 `/api/events` SSE 增量推送，v3.0.6 起 SSE 只靠
reconcile 的 notify 唤醒 + 5s 保险 tick，不再每秒全量快照。）

## 5. 配置 JSON 键序

早期 Python 参考实现写 JSON 保持插入序，Go 版按键排序。所有消费方均为 JSON
解析器，语义等价；仅手工 diff 配置文件时观感不同。为保持 diff 友好可引入
有序序列化，但会增加自维护代码，暂缓。（Python 参考实现已移除，本条目仅留档）

## 6. 已知平台限制

- iSH（iOS 模拟器）上 modernc.org/sqlite 在进程退出关库时可能触发其 libc
  层的段错误——iSH 对部分直接系统调用的模拟缺陷所致；真实 Linux 内核
  （Debian 12 实测）反复开关库与优雅退出均正常。
- iSH 无 AF_NETLINK，nftables 无法工作属环境限制（面板会以采集异常呈现，
  其余功能不受影响）。

## 7. 兼容性修复说明（有意为之的行为差异）

- `GET /login` 现在渲染登录页。旧版返回 404，导致登录失败重定向
  （302 → /login?error=1）后用户卡死，login.html 的错误提示逻辑从未生效。
  此为修复既有缺陷，非行为破坏。

## 8. 第二轮修复中评估未采纳的事项（v3.0.1）

- **state.json 严格读取**：nodes.json 已加严格读取；state.json 损坏时 next_node_id
  会回退到 max(现有节点 id)+1 兜底（ID 不会复用，安全），故暂不引入严格模式。
- **逐架构 .sha256 旁车文件**：坚持单一 SHA256SUMS 作为唯一校验来源，
  避免两套校验文件漂移；安装器/CI/测试三方共用同一提取逻辑。

## 9. 第三轮稳定性修复中记录但未修改的事项（v3.0.3）

- **`reset` 等统计类命令的配置读取仍为宽松模式**：`once/show/daily/reset/config-get`
  不启动网络服务、不改配置、不动防火墙；其中 reset 仅删除用户显式指定的
  统计数据。若未来要求全量 fail-closed，可将这些命令一并切换到
  `config.LoadStrict()`。
- **Apply 的 `--force` 强制重建开关**：v3.0.3 起"最终采样失败即中止 Apply"
  是刻意的默认行为（数据正确性 > 可用性）。若确有绕过需求（灾难恢复场景），
  未来可设计显式 `--force` 参数并输出强警告，本轮刻意未提供。

## 10. v3.0.5 已落实（自历史条目迁移，不再"未来处理"）

- **`/api/nodes` 凭据泄漏**：已改为返回脱敏 `PublicNodeDTO`（仅 id/name/type/port），
  普通面板 token 不再等价于节点私钥。
- **query string token 认证**：已彻底移除 `?token=` 渠道，仅保留
  `Authorization: Bearer` 与 HttpOnly Cookie。
- **认证常量时间比较**：已改用 `crypto/subtle.ConstantTimeCompare`。
- **防火墙后端状态一致性**：新增 `effective-backend` 单一事实源，
  Apply/Collector/Repair/Clear 共用同一后端，杜绝 auto 下 fallback 分叉。
- **节点语义校验**：`validateNodes` 校验 id/port/type 合法性、唯一性。
- **跨进程 mutation 锁**：`/run/lock/sbx.lock` flock 覆盖整个节点事务。
- **sing-box 供应链**：官方无逐文件 checksum，改为 pin 版本 + sha256 校验。
- **dist 供应链**：binary 与 SHA256SUMS 绑定同一 immutable commit。

## 11. v3.0.6 代码复审修复（全部已落实）

> **历史语境**：本节（及 §12）记录 v3.0.6 / v3.0.7 时期的修复。当时 SBX 仍是
> 「nftables 优先 + iptables 回退」的双后端架构，因此文中出现的 iptables 相关
> 问题描述属于**当时的行为**。自 v3.0.9 起 SBX 为 nftables-only，iptables 后端
> 已完全移除，这些条目仅作历史记录，不描述现行能力（见 §14）。

一轮全仓复审发现并修复的问题，均带回归测试并在真机（Debian 12）验证：

- **策略层并发崩溃（P0）**：`reconcile` 只持 `runMu` 就改 `ipStates` 及其内部 map，
  而 SSE / HTTP 读侧只持 `mu.RLock` 遍历同一批 map，多核上必然
  `fatal error: concurrent map read and map write`。改为
  「`ipStates`/`flows` 为 reconcile 私有（runMu 唯一守卫）+ 每轮末在 mu 下发布
  不可变快照（`ipSnaps`/`activeIPs`）」，读侧只看快照。
- **策略脚本覆盖计数规则（P0）**：`serve` 把 `cfg.NftConf` 当策略脚本路径传入，
  策略生效即覆盖 `nft.conf` 的 `sbx_traffic` 定义，且 `Nft.Repair` 自愈时重放
  策略脚本 → 计数器永久建不回来。改为独立 `policy.nft`。
- **`nf_conntrack_acct=0` 误踢在线用户（P1）**：Debian/Ubuntu 默认不开计费，
  conntrack 无 `bytes=` → 字节增量判活永远判不出流量 → 空闲窗口后把在用连接判死。
  后端自动降级为「ESTABLISHED 即在线」，安装器同时尝试开启并持久化 sysctl。
- **本机出站流被算成客户端（P1）**：conntrack 原方向 `src=本机`，若节点监听
  443/8443 等常见端口，服务器自身 HTTPS 出站会占 slot、虚报在线 IP。
  按本机地址集合过滤（`connection.LocalIPs`，30s 缓存）。
- **`nodes.json` 损坏导致策略 fail-open（P1）**：宽松读取得到「零节点」→ 策略表被
  清空、所有阻断解除。改为严格读取，损坏时保持上一轮 enforcement 并报错；
  策略 API 同步区分 503（文件不可用）与 404（节点不存在）。
- **iptables-only 主机策略永久不可用（P1）**：`nft -f` 每轮失败 → `states` 从不发布，
  面板全显示「不限」且每秒刷 WARN。改为 enforcement 失败不阻断状态发布，
  并以 `ErrEnforceUnsupported` 明确提示需要 nftables。
- **仅发 SYN 的陌生 IP 抢占名额（P1）**：provisional slot 优先级高于真实客户端，
  攻击者每 10s 重发 SYN 即可持续拒服。admission 排序改为
  「已建立且持正式 slot > 已建立 > 持 provisional > 其它候选」，
  名额满时真实客户端可抢占最早的 provisional。
- **统计 reset 后配额长期失效（P1）**：`used = totals - baseline` 被 clamp 到 0，
  `reset` 删掉 totals 但基线仍在旧高水位。`Reset` 同事务清零基线，
  reconcile 侧另加「基线 > lifetime 即归零」自愈。
- **`genPolicyNFT` 输出非确定性（P2）**：遍历 map 生成规则行，相同输入产出不同文本。
  端口与节点 id 均排序后输出。
- **`main` 分支入库 67MB 过期二进制（P2）**：`dist/` 在 `.gitignore` 里却被跟踪，
  内嵌版本停留在 v3.0.0。已 `git rm --cached`。
- **CI 发布 dist 分支夹带整个源码树（P2）**：`git rm -r --cached . || true` 在
  产物被重建时报错并被吞掉，索引未清空 → dist 分支 120 文件 / 142MB，
  且含一份过期二进制副本。改用 `git read-tree --empty` + 索引断言 + 发布前内容断言。
- **`node_policy` 孤儿行（P2）**：节点删除走 CLI candidate/commit 流程不经过
  `DeleteNode`。reconcile 按当前节点集合清理。
- **登录无失败节流（P2）**：同源 IP 连续失败 5 次后每次强制延迟 2s（5 分钟窗口，
  成功即清零）；节流键只用 `RemoteAddr`，不信任可伪造的 `X-Forwarded-For`。
- **SSE / reconcile 固定开销（P2）**：SSE 去掉 1s 轮询（改 notify 唤醒 + 5s 保险）、
  序列化结果复用为 payload（原先每节点 Marshal 两次）；reconcile 的
  per-node `SELECT totals` 合并为单条查询。
- **死代码**：`nft.go` 的 `portRanges`、`ipslot.go` 中 `lastActive` 未使用的 `ip` 参数。

## 12. v3.0.7 复审修复（全部已落实）

> **历史语境**：同 §11——本节描述双后端时期（v3.0.7）的行为，其中 iptables
> 相关条目已随 v3.0.9 的 nftables-only 收敛失效。

第三轮复审（在 §11 之后）发现的残余问题，均带回归测试：

- **`sbx_policy` 表无清理路径（P1）**：`fw_clear` / `sbx-core clear` / 卸载只删
  `sbx_traffic`，从不清 `inet sbx_policy`。卸载后 quota/IP 限制的 drop 规则残留
  内核直到重启，被限端口继续被拦。现在 `Clear()` 与 `fw_clear()` 一并删除
  `sbx_policy`（`IsMissingMsg` 容错），并卸载时清理
  `/etc/sysctl.d/99-sbx-conntrack.conf`。注：面板单独停止时 enforcement
  仍刻意冻结在最后一轮状态（fail-closed），仅在显式 clear/卸载时清除。
- **`ErrEnforceUnsupported` 每秒刷 WARN + 保存必 500（P1）**：`reconcile` 上抛该错误，
  但 `Run` 对任何 reconcile error 都 `slog.Warn`（iptables 主机启用策略后每秒一条），
  且 `putPolicy`/`resetQuota` 在配置已落库、状态已发布时仍返回 500。现在
  `reconcile` 对该错误只写 `lastErr` 返回 `nil`；瞬态 enforcement 故障仍上抛。
- **SSE notify 单 chan 共享（P2）**：`Notify()` 返回同一 cap=1 chan，一次 signal
  只唤醒一个订阅者，多客户端时其余靠 5s fallback。改为 `Subscribe()` 扇出——
  每订阅者独立带缓冲 chan，发布时向全部订阅者非阻塞广播。
- **策略 nft 整表重写无节流（P2）**：仅 allow set 内容变化（slot 授予/释放）可被
  扫描者 SYN churn 诱发成每秒一次整表 `nft -f`。现在 quota 翻转 / 受限节点集合
  变化 / 节点端口形态变化仍立即应用，仅 IP 内容变化在 `enforceMinInterval`
  （3s）窗口内合并（持续不一致 → 间隔一到即收敛）。
- **节点改端口后 nft 死规则（P2，修复中附带发现）**：`applied` 比较的键是节点 id，
  规则却按端口生成——改端口但 allow set 不变时跳过应用，nft 残留旧端口规则、
  新端口失去 enforcement。新增 `nodesShape` 端口形态摘要比较，漂移即立即重写。
- **policy 节点文件路径与 `nodes_file` 分叉（P2）**：`policy` 写死
  `appDir/nodes.json`，忽略 `panel.json` 的 `nodes_file`。现在 `serve` 经
  `SetNodesFile(cfg.NodesFile)` 传入，两处读同一文件。
- **Go CLI 节点变更无内建锁（P2）**：flock 只在 sbx.sh；直接并发跑
  `sbx-core node add/edit/remove` 有 NextID 竞态 / candidate 串写。现在
  `sbx-core node` 的 mutation 子命令内建 flock（`SBX_LOCK`），经菜单调用时
  shell 持锁并导出 `SBX_LOCK_HELD=1` 跳过自锁；锁不可用 fail-open。
- **shell 提交 config.json 无目录 fsync（P2）**：nodes.json 走 `RenameAtomic`
  （rename + fsync 父目录），config 走 shell `mv -f`。现在 `sbx-core node commit`
  一并提交两个候选文件，统一 rename+fsync 语义（shell 不再自行 mv）。
- **P3**：`putPolicy` 加 1 MiB body 限制并拒绝尾随数据；snell 分享链接 fragment
  全量百分号编码（空格/括号不再裸出）；`fw_clear` 去掉重复的 `nft delete` 行；
  `config-get` 缺失键打印空行（不再输出 Go 的 `<nil>`）；
  `State.ActiveTCPConn` 从 JSON DTO 移除（从未被任何 API 消费方使用，
  数据保留在包内 `activeTCP` 供测试断言）。
- **策略表外部删除自愈（修复中附带发现）**：`sbx --clear-firewall` 在面板运行中
  删掉 `sbx_policy` 后，reconcile 因内存 applied 快照与目标一致而永不重写，
  enforcement 静默失效。现在无变化分支对「需要 enforcement」的节点做
  10s 节流的存在性探测（`nft list table`），缺失即主动重建。
- **`sh iptables.sh clear` 在无链时返回非 0（修复中附带发现）**：nft-only 主机上
  `sbx --clear-firewall` 必报「iptables 计数链清除失败」并以 1 退出。
  `clear_one` 末尾显式 `return 0`——「链本就不存在」对 clear 语义即成功。

## 13. v3.0.8 修复：conntrack 未激活导致「有连接数但在线 IP 恒为 0」

真机（Debian 12，7 台家宽 VPS）暴露的缺陷：面板节点卡片显示 TCP 连接数正常，
但「在线 IP」永远是 0。

**根因链条**：内核只在**存在引用 conntrack 的 netfilter 规则**时才真正为连接建立
conntrack 条目。这批机器很干净——没有任何防火墙规则，而 SBX 的计数表 `sbx_traffic`
只有 named counter、不引用 `ct`，于是 `nf_conntrack` 模块虽已加载、
`/proc/net/nf_conntrack` 存在可读，内容却恒为空（`nf_conntrack_count=0`）。

而策略层的在线 IP 判活以 conntrack 为主数据源，且刻意规定「conntrack 可用时绝不
回退 /proc」（防止已断开的 socket 残留复活成在线 IP）。判定「可用」的依据是文件能否
读到——文件确实在，只是永远 0 行。于是：**conntrack 判定可用 → 0 条流 → 不回退 →
在线 IP 恒为 0**；连接数走另一条路径（直读 `/proc/net/tcp`）所以正常，
表现就是「有连接数、在线 IP 是 0」。

诊断决定性实验：临时加一条 `ct state new counter` 规则，5 秒后 conntrack 立刻开始
跟踪（count 0 → 2），删掉规则后恢复为 0。

**两处修复（缺一不可）**：

1. `firewall.GenNFT` 增加 `sbx_ct` 链（`hook input priority -150; policy accept;`
   + 唯一动作 `ct state new counter name sbx_ct_activate`）。它不做任何放行/拦截
   决策，唯一作用是让内核为流量建 conntrack 条目。写进 `nft.conf` 后随
   `sbx-firewall` 开机自动生效，重启不丢。计数器名刻意不匹配
   `ParseCounterName` 的 `sbx_(n<id>|sys)_(i|o)` 形态，不会被采集器当流量入账。
2. `connection.ReadConntrack` 对「文件可读但整表 0 条」返回
   `Available=false, Inactive=true`，让判活自然走 /proc 回退；策略层用 `ctInactive`
   在状态变化时提示一次原因（不每秒刷屏）。这是对旧规则集、或规则被外部清空
   场景的兜底——单靠第 1 条，存量机器在 `apply` 前仍是坏的。

**为什么「整表 0 条」是可靠判据**：一台有网络活动的服务器不可能一条 conntrack 都
没有（SSH/DNS 自身就会产生条目）。而「conntrack 正常但当前无客户端连接」时表里仍有
其它流（`Entries > 0`），仍走 conntrack 口径，原有的「不复活已死 IP」语义完整保留。

## 14. v3.0.9 架构收敛：nftables-only（iptables 后端彻底移除）

SBX 从「nftables 优先 + iptables 回退」的双后端项目收敛为 **nftables-only**。
自本版本起，netfilter / 流量统计 / 策略执行的唯一后端是 nftables；不支持
iptables 与 ip6tables，不存在 `backend=auto|nft|iptables` 运行时选择，
也不存在任何形式的降级路径。

**删除的实现**：`internal/firewall/iptables.go`（`Iptables` 后端：自定义链读取、
双栈聚合、partial snapshot 保护、Repair）、`rules.go` 的 `GenIPTables`
（`SBX_IN` / `SBX_OUT` 链脚本生成）、`backend.go` 的 `New` / `DetectBackend` /
`probeBackend` / `probeBackendForced` / `normalizeBackend`、`state.go` 的
effective-backend 状态文件机制（`/run/sbx/effective-backend`，双后端下用于
「Apply 后端 == Collector 后端」的单一事实源，单后端后失去意义）、
`policy` 的 `ErrEnforceUnsupported` / `SetEnforceBackend`，以及对应测试与
`testdata/gen_iptables.golden`。

**保留的抽象**：`firewall.Backend` interface 仍在，但只服务两件事——Collector
单测注入 fake backend 做故障注入，以及未来同后端不同实现（见 §1 的 netlink 直读）。
生产代码唯一构造器是 `firewall.NewNft(cfg.NftConf)`。`BackendName` 常量固定为
`"nft"`，`/api/summary` 的 `backend` 字段维持 schema 兼容（只读上报，不再可配）。

**fail-closed 行为**：安装/升级时确认 nftables 可用（`nft` 在 PATH 且
`nft list tables` 成功——命令存在但无 netlink 权限/内核不支持同样算不可用），
装不上即中止；`sbx-core apply` 的 `nft -f` 失败直接返回非 0（旧版会 fallback
到 `sh iptables.sh apply` 并谎报成功）；策略层 nft 应用失败上抛并写入
`policy_error`，同时状态照常发布（面板仍显示真实用量）。

**升级兼容**（老用户数据一律不动：nodes.json / traffic.db / token / port / tz）：

- `panel.json` 的废弃键 `backend`（含值 `iptables`）与 `ipt_script` 被**忽略**而
  非拒绝——`Validate` 不再校验 backend，老配置照常启动；
- `sbx-core config-migrate`（升级路径显式调用，也由 `config-set` / `EnsureToken`
  顺带完成）只删这两个键，走 `fsx.WriteFileAtomic`（临时文件 + fsync + 原子
  rename，0600），其余键含用户自定义键原样保留；幂等，损坏配置拒绝迁移；
- 安装器 `cleanup_legacy_backend` 删除 `$APP_DIR/iptables.sh`，并 best-effort
  删除旧版自建的 `SBX_IN` / `SBX_OUT` 链及其 INPUT/OUTPUT 跳转。**这是清理旧版
  残留的一次性 migration，不是重新支持 iptables**；绝不 flush INPUT/OUTPUT/filter、
  不改默认 policy、不触碰其它程序或用户自己的链；
- Collector 的 partial-snapshot 守卫对老库里 `sbx:n1:i@v4` 形态的历史基线键
  豁免（新快照里必然缺失，若计入守卫会导致升级后每轮拒绝提交、统计停摆），
  首次成功提交时随 `counter_state` 整表重写自然清除。

**capability 收敛**：`sbx-panel.service` 去掉 `CAP_NET_RAW`。它是 v3.0.6 为
iptables-legacy（AF_INET SOCK_RAW）加的；nft 走 AF_NETLINK/NETLINK_NETFILTER
只需 `CAP_NET_ADMIN`，连接数与 conntrack 判活读 `/proc` 不需要 capability。

**防误伤（不可回归的强制要求）**：SBX 只管理自己创建的
`table inet sbx_traffic` 与 `table inet sbx_policy`。安装、升级、清除、卸载
全程不执行 `nft flush ruleset`、不清空系统 INPUT/OUTPUT、不修改默认 policy。
回归测试锁定：`GenNFT` 输出不含 `flush ruleset` 且 `delete` 只针对 SBX 自己的表
（`internal/firewall`），`Clear` 只发出 `nft delete table inet sbx_{traffic,policy}`
且不出现 flush/INPUT/OUTPUT/-F/-X 与任何 iptables 调用（`internal/service`）。
