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

## 15. 性能类审计项的评估结论与待办（v3.0.10）

本节记录 v3.0.10 稳定性审计中**实测后决定采纳 / 暂不采纳**的性能项，附真机
数据与理由，避免将来重复评估。

### 15.1 nft 计数器的 netlink 直读（审计项「fork/exec 开销」）——暂不采纳

**实测**（Debian 12 / 8 核，`table inet sbx_traffic`，约 900 字节输出）：

```
nft -j list counters table inet sbx_traffic    平均 4.6 ms/次
按默认 2 秒采样                                 ≈ 0.23% 单核
```

结论：**不是当前的主要开销**。同轮审计中真正的大头是每秒一次的策略 reconcile
（conntrack 解析 16.3ms + `/proc` 解析 4.1ms），已通过「conntrack 可用时不再读
`/proc`」与「解析去分配」修复——真机 A/B 实测：空载 3.78% → 3.33% 单核，
约 2400 连接下 7.27% → 5.27%。netlink 直读只能省掉那 0.23%，却需要引入 netlink
依赖（`github.com/google/nftables` 或手写 NETLINK_NETFILTER 编解码）、处理内核
版本差异与部分 dump 的续包逻辑（`NLM_F_DUMP` + `NLM_F_DUMP_INTR` 重试），
收益与复杂度不成正比。

**本轮已做的零风险改进**：`Nft.Read` 增加**合并读取（single-flight）**——
同一时刻的并发 Read 共享一次 exec，但**不缓存上一次结果**，因此不存在
「读到旧计数」的可能（`TestNftReadNoStaleCache` 锁定该性质）。
生产路径上目前只有采集线程读计数器，因此这属于防御性改动。

**若将来需要采纳**（例如采样间隔缩到 1 秒以下，或计数器数量增长到数百）：
1. 在 `firewall.Backend` 接口后新增 `netlinkBackend`，`NewNft` 按能力探测选择
   （探测失败仍回退 exec 实现，语义与今日一致）；
2. 用 `NLM_F_DUMP` 一次性取回 `sbx_traffic` 表内所有计数器对象，按 name 建 map；
   注意 `NLM_F_DUMP_INTR`（dump 期间表被修改）需重试；
3. 保留 `runCmdFn` 注入点，让现有测试与故障注入继续可用；
4. 验收标准：`Read` 的 p99 从 4.6ms 降到 <0.5ms，且与 exec 实现产出**完全一致**
   的快照（用同一内核状态对比两种实现）。

### 15.2 连接数改 conntrack / eBPF（审计项「连接数读取 /proc」）——暂不采纳

**现状**：`/proc/net/{tcp,udp}[6]` 由采集线程每 2 秒读一次并缓存，HTTP 请求
不再逐次读（`Collector.lastConns`）。

**本轮已采纳的改进**（见 `internal/connection`）：
- 行/字段解析改索引扫描 + 复用缓冲：`ParseLocalPorts` 在 1 万行输入下
  3.73ms / 358KB / **25 allocs**（原为每行一次切片分配）；
- 新增端口过滤：`/proc/net/tcp` 里**每个已建立连接都占一个不同的本地端口**，
  旧实现为每个端口建一个 map 条目，1 万连接即每次分配 1 万个 map。过滤后
  `RemoteIPsByPort` 从 14.36ms / **3.08MB / 40228 allocs** 降到
  6.31ms / **2.4KB / 18 allocs**（分配降低约 1280 倍）。

**为何不引入 conntrack/eBPF 做连接数**：
- conntrack 已作为「在线 IP 判活」的主数据源（见 §13），但**连接数口径不同**：
  `/proc/net/tcp` 是 socket 视角（含本机进程持有的 socket），conntrack 是流视角，
  两者对 UDP 通配入站、TIME_WAIT、NAT 的处理不一致。换口径会让面板数字变化，
  属于产品行为变更，需要单独评审；
- eBPF 需要 `CAP_BPF`/`CAP_SYS_ADMIN`、内核 4.18+ 且 BTF 可用，并引入
  cilium/ebpf 依赖与 CO-RE 构建链，对 Alpine/musl 与老内核的兼容成本高。
  本项目刻意保持「单二进制 + 仅 CAP_NET_ADMIN」的部署模型。

**触发重新评估的条件**：单机连接数常态 > 5 万，且 `CountForNodes` 耗时占采集
周期 20% 以上（届时先用基准定位，再决定是优化解析还是换数据源）。

### 15.3 modernc.org/sqlite 升级——待评估

v3.0.10 只把 `golang.org/x/sys` 升到 v0.48.0（消除 GO-2026-5024），
`modernc.org/sqlite` 仍为 v1.34.5：`govulncheck` 在 Go 1.27.1 下对全仓库
（含模块级）报告 **0 个漏洞**，因此不构成已知风险。

但 v1.34.5 内嵌的 SQLite 版本已较旧，而 SQLite 本身的 CVE 不一定进入 Go
漏洞库。升级到 v1.59.0 需跨 25 个小版本，涉及 `modernc.org/libc` 等传递依赖
整体跃迁，属独立评估项。**建议动作**：单独开一次升级 PR，跑完整的
`go test -race ./...` 与真机 `traffic`/`policy`/`database` 包测试（这三个包
重度依赖 SQLite 行为），确认 WAL、`ON CONFLICT` upsert、`busy_timeout`
与并发写行为无回归后再合并。

## 16. 架构类审计项的评估结论（v3.0.10）

本节记录审计中「重构类」提议的评估结论。三项均**评估后不采纳**，理由如下
（不是"没时间做"，是做了会与本项目已确立的约束冲突）。

### 16.1 拆分 sbx.sh 为 lib/*.sh —— 不采纳

提议：把 1958 行的安装器拆成 `lib/detect.sh` / `lib/download.sh` /
`lib/install.sh` / `lib/service.sh`，主脚本 source 这些库。

**不采纳的理由（硬约束冲突）**：

1. **分发模型要求单文件**。README 与安装命令的核心是
   `bash <(curl -fsSL .../sbx.sh)` —— 用户拿到**一个**文件即可执行。
   拆成多文件后，进程替换方式（`/dev/fd/NN`）无法解析相对 `source lib/xxx.sh`，
   一键安装会直接失效；改为先下载整个目录又会让"先校验再执行"的流程复杂化。
2. **测试与 CI 依赖单文件的结构标记**。`tests/*_flow_test.sh` 用
   `sed -n '/^# >>> <name>/,/^# <<< <name>/p' installer-template.sh` 提取**真实
   代码块**做隔离测试（这是"测试与发布物同源"的保证），CI 另有
   `installer-template.sh` 与 `sbx.sh` 必须字节一致的 drift 检查。
   拆分后这两套机制都要重写，收益（可读性）与回归风险不成正比。
3. **单文件内部已经有明确分区**：区块用 `# >>> name` / `# <<< name` 标记，
   并配了「分区」注释标题。可维护性问题已通过该约定解决。

**已采纳的替代改进**：把跨发行版分支集中到 `detect_platform` / `pkg_install` /
`svc_do` 三个函数（原本已如此），并在本轮把所有 `die` 提示补成"可照抄的下一步"
（见 README 支持矩阵与 §15 之外的安装器提示强化）。

### 16.2 CLI 改用 cobra / urfave-cli —— 不采纳

提议：`cmd/sbx-core/main.go` 的手写 switch 改为 cobra 或 urfave/cli，
自动生成 help 与补全。

**不采纳的理由**：

1. **与"CLI 兼容"直接冲突**。`sbx.sh` 大量依赖 `sbx-core` 的既有输出格式
   （例如 `config-get` 对缺失键必须打印**空行**而不是 `<nil>`，
   `node port-used` 用退出码表达"端口被占用"，`node commit` 打印 `ok`）。
   cobra 会接管 `--help` 文本、未知参数的处理方式与错误输出格式，
   这些都是 shell 侧正在解析的接口。改框架等于同时改一批隐式契约。
2. **依赖面**。本项目第三方依赖只有 `modernc.org/sqlite`（纯 Go，为了无 cgo 的
   静态单二进制）。为 15 个子命令引入一个 CLI 框架，与这一取向相悖。
3. **现有实现已覆盖需求**：`printUsage()` 提供完整用法文本；
   子命令分发是扁平 switch（无嵌套子命令、无 flag 组合解析需求）；
   补全功能对"SSH 上去跑 `sbx` 进菜单"的使用方式价值很低。

**已采纳的替代改进**：本轮补了 `cmd/sbx-core` 的 CLI 集成测试
（见 `internal/service` 与 `cmd` 下的测试），把"输出格式与退出码"变成
可回归的断言——这才是防 CLI 兼容性回归的真正手段。

### 16.3 go:embed 前端资源拆分 —— 不采纳（实测无收益）

提议：评估二进制体积，若过大则把前端资源拆为独立 embed.FS 或按需加载。

**实测数据**（Go 1.27.1 / linux-amd64）：

| 构建 | 体积 |
|---|---|
| 纯 `hello world`（`-s -w`） | 1.22 MB |
| `hello world` + `modernc.org/sqlite`（`-s -w`） | 6.05 MB |
| `sbx-core` 完整（`-s -w`） | **11.67 MB** |
| `sbx-core` 完整（默认，含符号表） | 17.21 MB |
| 内嵌前端静态资源（`internal/webui/static`） | **51 KB** |

结论：**前端资源只占 `-s -w` 产物的 0.44%**，拆分 embed 最多省 51KB，
却要让资源脱离二进制（重新引入"文件丢失/版本错配"这类部署故障）。
真正的体积大头是：Go 运行时（1.2MB）+ 纯 Go SQLite（约 4.8MB）+
net/http 与 crypto/tls（面板与 HTTPS 需要）——这些都是刻意的取舍：
**单二进制、无 cgo、无运行时依赖**换来的体积，正是本项目"服务器不再需要
Python/运行时依赖"这一核心卖点。

若将来确实要压体积，正确的方向是（按性价比排序）：
1. 发布产物继续用 `-s -w`（当前 `scripts/build-release.sh` 已如此）；
2. 评估 `-trimpath`（已用于构建）与 `GOAMD64=v3` 等目标平台优化；
3. 只有在内嵌资源增长到 MB 级时才考虑拆分——届时优先做**资源压缩**
   （gzip 后 embed + 运行时解压），而不是拆成外部文件。

## 17. v3.0.12 优化轮：实测采纳与否决记录

本轮做法：先在 1 核 VPS 上建立端到端基线（50 节点 × 1095 天 daily 历史、真实
nftables、按前端真实轮询节奏压测 120s）：空闲 CPU **0.27%** 单核、RSS 20.7MB、
`/api/live` p50 0.9ms、`/api/summary` p50 0.9ms、`/api/daily` p50 4.9ms。
结论：CPU/内存不是本项目的矛盾，**唯一随运行年限线性变差的是 `/api/daily`**。
因此本轮只改「实测有收益且写路径无可测回归」的项（详见 CHANGELOG v3.0.12）。

### 17.1 daily 趋势查询的三种 SQL 改写——否决（实测无效）

背景：`QDaily("")` 的聚合查询按索引序扫描并早退（LIMIT 生效），开销为
O(180 天 × scope 数) 的回表，而非 O(全表)——**先确认了这一点，再评估改写**。

实测（`internal/traffic` benchmark，50 scope × 1095 天）：

| 方案 | 全节点趋势 | 单节点趋势 | 分配量 vs 现状 |
|---|---|---|---|
| 现状（PK 索引扫描） | 4.11ms | 0.83ms | — |
| `IN (SELECT day ... LIMIT 180)` 子查询 | 4.16ms | — | 完全相同 |
| `JOIN (SELECT DISTINCT day ...)` 子查询 | 4.16ms | — | 完全相同 |
| 两步法（Go 先取 day 列表，再 180 个 `?`） | 5.07ms | — | 更多 |

三种改写的扫描量与现状**完全一致**：SQLite 不做 GROUP BY 提前退出，子查询
照样全索引扫；两步法反而因 180 个占位符的语句构建与参数绑定更慢。

**采纳的替代方案**：覆盖索引 `idx_daily_day_vals(day,scope,rx,tx,rx_pkts,tx_pkts)`
与 `idx_daily_scope_vals(scope,day,...)`——聚合/单节点查询都变成纯索引扫描：

| 变体 | 全节点趋势 | 单节点趋势 | 写路径（事务内 50 scope upsert） |
|---|---|---|---|
| 无覆盖索引 | 4.11ms | 0.83ms | 3.83ms/tick |
| 仅 day 先 | 1.87ms | 0.58ms | 2.69ms/tick |
| 仅 scope 先 | 4.08ms | **0.13ms** | 2.64ms/tick |
| **双索引（采纳）** | **1.90ms** | **0.14ms** | 2.66ms/tick |

写路径在事务内对三个索引方案无可测差异（差异在噪声内；autocommit 逐行写
的基准会误导，见 `BenchmarkIndexTradeoff` 的教训：必须测真实路径）。
代价是 daily 表磁盘占用约每行 +80B（50 节点 × 3 年 ≈ 4MB），可忽略。

### 17.2 评估后暂不做的项

- **`QRate`/`samples` 加覆盖索引**：实测 50 scope 仅 0.22ms、537 allocs，
  且 samples 表有 120s 清理策略恒小——不值得加。
- **`conntrack`/`/proc` 解析进一步去分配**：v3.0.10 已做索引扫描改造，
  本轮仅把 `os.ReadFile → string(b)` 的整表拷贝换成零拷贝视图；
  再往下的 []byte 化需要改动全部公共 API 与测试，收益（每秒省 1×表大小的
  分配）在小机器上（conntrack 表通常 <1MB）不构成瓶颈。
- **`/api/daily` 的 `days` 语义（有数据的天 vs 日历天）**：保持现状。
  改成日历窗口会改变输出行数语义，属产品行为变更。

### 17.3 本轮验证方法（可复用）

- 端到端基线：实验台脚本按前端真实节奏（2s/8s/60s）轮询并采样
  `/proc/PID/stat`，比微基准更能反映真实体验；改前改后各跑一轮。
- 微基准必须测**真实路径**（事务内、真实调用栈），并先看 `allocs/op` 是否
  变化——分配量不变基本等于「没有生效」。

## 18. v3.0.15 瘦身轮：nodes.json 解析缓存与死码清理

### 18.1 `LoadPanelNodesStrict` 的 (mtime,size) 缓存——已采纳

`deadcode` + 手工计数确认：该函数在 policy reconcile（1Hz）与 collector（0.5Hz）
每轮都调，但 `nodes.json` 只在人工节点操作时经原子 rename 改写。这是稳态下
最大的一项重复固定开销。实测（2 核 VPS，`internal/nodes` benchmark）：

| 节点数 | 改前 | 改后（缓存命中） |
|---|---|---|
| 5   | 44µs / 10KB / 155 allocs | 1.36µs / 272B / 2 allocs |
| 50  | 340µs / 87KB / 1386 allocs | 1.35µs / 272B / 2 allocs |
| 200 | 1.25ms / 355KB / 5447 allocs | 1.38µs / 272B / 2 allocs |

命中后只剩一次 `os.Stat`（272B/2 allocs 即 stat 的固定成本，见 `BenchmarkStatOnly`
的 1.16µs/256B/2 allocs——两者几乎相等，说明解析成本已被完全消除）。

**为何端到端 CPU 不显著下降**：基线本就只有 0.81% 单核（reconcile 里 SQL 与
conntrack/proc 解析才是大头，nodes 解析占比小）。本轮价值是消除**隐性的**每秒
重复解析与 GC 压力——在节点数多、或未来采样加密时收益放大；且减小了维护面。

**fail-closed 正确性论证**（有回归测试 `internal/nodes/loadcache_test.go` 锁定）：
- 只有解析+校验都成功才写缓存；任一次损坏/读失败返回 error 且**不缓存** →
  下一轮仍 miss、仍重读、仍 error，reconcile 保持上一轮 enforcement；
- 命中判据是 `(mtimeNS, size)` 双字段全等，写入必然改变其一；原子 rename
  保证不读半写文件；stat 用 read 之前的值，替换竞态下一轮自然失效；
- 缓存值是只读共享 `[]Node`，所有调用方只读遍历（已核实无 `n["k"]=` 写入）。

### 18.2 删除的死函数

`deadcode` 报告 + 全仓引用核实（区分"生产可达""仅测试引用""完全无引用"）：
完全无引用、直接删除的 5 个——`connection.CountByPort`、`connection.NodeRemoteIPs`、
`fsx.WriteJSONAtomic`、`traffic.TimeIn`、`traffic.TodayStr`（后者仅一个测试引用，
已改用 `TodayAt` 等价断言）。其余 deadcode 候选（`ttlCache.size`、`allPorts`、
`grantedCount`、`activeTCPConn`、`ParseRemoteIPs`、`RemoteIPsByPort`、
`SaveNodesFile`）**保留**——它们被单元测试作为契约/基准对照使用，删了会削弱
测试覆盖，不属于"无用代码"。

### 18.3 未采纳

- **prepared statement 跨 tick 复用**：collector 每 tick `Prepare` 3 条语句。
  实测 commitTick 的 Prepare 成本在整体事务里占比极小（SQLite 单连接、语句
  已被驱动缓存），改成长生命周期 stmt 会引入连接池/关闭时序的复杂度，收益
  不成正比。暂不做。
- **二进制体积**：删死码后体积无变化（11.6MB，死函数被链接器 DCE 本就不计入）。
  体积大头仍是 Go 运行时 + 纯 Go SQLite，见 §16.3，无低风险瘦身空间。
## 19. v3.0.16 reconcile 热路径：profile 定位后再优化

### 19.1 基线与热点

测试机：Debian 12 x86_64，2 核 / 1967MB / Go 1.27.1 / gcc；实验 workload
50 个 VLESS 节点、每节点 10 或 50 条 ESTABLISHED conntrack flow；SQLite / nft
真实运行，`nftApply` 基准里为 no-op（它自身已实测低于总开销，不在本轮范围）。

`BenchmarkReconcile` + `-cpuprofile` 定位：原始 v3.0.15 50×50 每轮
**5.96ms / 1.54MB / 8141 allocs**；累计热点集中于 `NodeIPState.Reconcile`
（排序 + 比较器内重复 map lookup）、`buildActivity`（flow key 拼接/状态对象/本轮
flow 集合 map）和 map hashing/GC。SQL 两查询合计约 0.29ms，但未排入本轮——
它不是 50×50 活跃负载里的首要热点，合并查询会增加查询结构复杂度，收益未证实。

### 19.2 采纳的改动与正确性约束

1. **`ipslot.Reconcile` 稳态跳过排序并移除冗余 seen map**：先判断所有 active/candidate
   是否已持 slot 或刚释放；若无新 admission，顺序不影响任何授予/拒绝决策，跳过排序。
   active/candidates 输入本身是去重 map，因此删除额外 `seen` map，用源 map 直接判重/判离线。
   新 IP 时仍保持原优先级/FirstSeen/IP tie-break，不改变严格 admission 语义。
2. **flow tracker 用 `flowKey` 结构体 + 内联 value**：去掉每条 flow 每 tick 的
   `nodeID + "\x00" + IP + ":" + strconv.Itoa(port)` 拼接，以及 `*flowState`
   每次字节变化/Bytes=0 时的独立堆对象。语义保留「bytes 增量刷新 LastSeen；
   静默 grace 不刷新；超时但 conntrack 仍存在则不计 active、但保留 tracker」。
3. **`SeenEpoch` 替代本轮 `currentFlowKeys` map**：每次 buildActivity 增加 epoch，
   在持久 flowState 上标记本轮出现。GC 条件仍精确等价于旧逻辑：只有「本轮未出现
   且距 LastSeen > ipIdle」才删。删除节点按结构体 key 的 nodeID 字段精确清理。
4. **克隆长生命周期 IP key**：conntrack parser 返回的 SrcIP 是整张 conntrack
   文本的 substring；若直接持久化 map key，会把整个数 MB 文件缓冲区随着单个
   活跃 IP 长期保留。IP 第一次进入 Observed/flow tracker 时 `strings.Clone`，
   后续用内容相等的 substring lookup，不再重复 clone。
5. **快照排序快路径**：常规 reconcile 中所有 slot 的 LastSeen 是同一轮 now，
   于是 active IP 排序等价于 IP 升序。检测到全部 last-active 时间相同就直接对
   `[]string` 排序（只分配一个切片）；时钟回拨等不同时间场景回到原完整时间排序。
   `IPEntry` / Rejected 的确定性输出排序改用 Go `slices.SortFunc`，移除
   `sort.Slice` 的 reflect swapper 热路径。
6. **计费模式提示与 flow 扫描合并**：`nf_conntrack_acct=0` 的提示逻辑原先先扫一遍
   `cr.Flows` 计数、随后主逻辑再扫一遍处理 flow。计数过滤仍使用相同的节点端口与
   本机出站过滤，改为在主扫描中累加，循环结束后发相同状态变更日志。
7. **节点文件缓存失效加文件身份**：在既有 mtime+size 之外检查 `os.SameFile`，
   原子 rename 替换即使刻意保持同大小与 mtime 也不会误命中旧内容。

### 19.3 A/B 实测

同一台真机、同一个 workload、benchtime=2000x（slot 基准单节点、reconcile 基准 50 节点）：

| workload | v3.0.15 | v3.0.16 | 降幅 |
|---|---:|---:|---:|
| Reconcile 50 节点 × 10 IP | 1.189ms / 404KB / 3624 allocs | **0.905ms / 273KB / 1909 allocs** | **24% time / 32% B / 47% allocs** |
| Reconcile 50 节点 × 50 IP | 5.963ms / 1.538MB / 8141 allocs | **3.225ms / 0.966MB / 2210 allocs** | **46% time / 37% B / 73% allocs** |
| Slot reconcile 稳态 50 IP | 43.9µs / 6464B / 15 allocs | **10.5µs / 3160B / 5 allocs** | **76% time / 51% B / 67% allocs** |
| Slot reconcile 稳态 250 IP | 268µs / 46.8KB / 21 allocs | **55.5µs / 19.8KB / 5 allocs** | **79% time / 58% B / 76% allocs** |

端到端最终确认（同机 2 核、50 节点 × 1095 天）：最终修复状态发布顺序后复跑 60s CPU 0.74% 单核 / RSS 21.6MB，live/summary p50 1.9/2.3ms，daily 单次 7.1ms。v3.0.15 基线 151s 采样 CPU 0.81% / RSS 21.4MB；采样窗不同仅作方向参考。无可见 CPU/延迟回归；RSS 在 21–23MB 区间，受 Go heap 高水位与机器连接数影响。

### 19.4 验证矩阵

- `go test -count=1 ./...` / `go vet ./...` / `go test -race -count=1 ./...` 全绿；
- 测试显式锁定 admission priority、provisional 抢占、flow 超时 GC、排序语义、
  same-mtime/size 原子替换失效、conntrack 大缓冲不被长期 key 引用；
- 真机端到端最终复测 CPU 0.74% 单核 / RSS 21.6MB，live/summary p50 1.9/2.3ms，
  daily 单次 7.1ms；与 v3.0.15 同量级，无功能或性能回归。

## 20. v3.0.17 二次审计：节点端口索引缓存与单遍 snapshot

### 20.1 全仓复核范围

在 v3.0.16 之后重新检查了：生产/测试 deadcode、所有 reconcile/collector/API
读写路径、`go vet`、全量 race、安装器流程、真机 `cpuprofile`/`memprofile` 与
50 节点 × 10/50 flow A/B。当前 deadcode 报告中剩余的 `ttlCache.size`、
`allPorts`、`ParseRemoteIPs`、`RemoteIPsByPort`、`SaveNodesFile`、
`grantedCount`、`activeGrantedCount`、`buildActiveIPsFromState`、
`activeTCPConn`、`buildNodeIPSnapshot` 均被测试作为契约/基准/诊断辅助使用，
不属于可直接删除的生产死码；强删会削弱测试覆盖，故保留。

### 20.2 采纳的低风险改动

- `Service.activityPortIndex` 以 strict loader 返回的不可变 nodes slice 首元素地址
  + 长度判断是否复用端口索引。nodes.json 原子替换得到新 slice，自动重建；
  端口变更有回归测试。正常每秒不再 `ParsePorts` 和重建 `portNode` map。
- `buildNodeSnapshots` 一遍 Slots/Observed 遍历同时产出 API snapshot 与 active IP
  顺序，并从 `snap.Granted` 填入 `State.ActiveIPs`。发布顺序明确为「先填
  `st.ActiveIPs`，再写 `newStates`」，避免快照字段滞后。

### 20.3 结果与未采纳项

同机最终基准（50 节点，benchtime=2000x）：50×10 flow 为 **0.851ms /
269KB / 1852 allocs**；50×50 flow 为 **3.408ms / 0.961MB / 2153 allocs**。相对
v3.0.15 基线（1.189ms/404KB/3624 与 5.963ms/1.538MB/8141），累计降幅约
28%/33%/49% 与 43%/37%/74%。单次 CPU 受真机调度波动，分配与内存更稳定；
主要新增收益是每轮少约 57 次分配、少约 5KB，并消除节点端口索引重复构造。
端到端最终 60s CPU 0.74% 单核、RSS 21.6MB、live/summary p50 1.9/2.3ms，
未见稳定性或 API schema 回归。

本轮重新评估但不做：

- SSE 连接上限：存在理论上的公网资源耗尽面，但属于产品容量策略，需要配置项、
  反向代理/多标签页语义与用户容量目标后再加，避免无提示地拒绝合法面板连接；
- prepared statement 长期复用：SQLite 单连接且实测查询/Prepare 不是主要热点，
  长生命周期关闭/连接重建复杂度高于收益；
- 大范围 map/slice 池化：会延长对象生命周期、增加并发/清空错误风险，当前
  profile 已将大头降到 buildActivity/runtime map，继续池化需先有真实高连接数数据。


## 21. v3.0.18 第三轮全面审计：缓存稳定性与 no-op 热路径

### 21.1 覆盖范围

按本轮要求复查 nodes CLI 参数解析、collector 定时循环、nft 生成的 v4/v6 set、
`sbx_traffic` / `sbx_policy` 表隔离、lazy cache 初始化、API singleflight/TTL、
CSV 流式导出、nodes/config JSON 读取、策略 no-op enforcement 与 reconcile 基准。

确认并保留的语义：

- 受限节点的 IPv4/IPv6 allow set 必须同时存在。某一族为空时空 set 表示该族无已授权 IP；
  删除“空族”set 或相应规则会改变 drop 语义，因此未做过滤简化。
- 两张 nft 表仍分别属于流量计数与策略 enforcement；生成器只 delete/create 自有表，
  不 flush ruleset。外部删策略表探测仍保留（10s 探测节流），不能因 no-op 优化而取消。
- Collector 的 deadline 循环在长暂停后重新对齐当前时钟；使用一次性 timer 并在取消时 Stop，
  不需要换成 ticker 或保留后台定时 goroutine。
- `parseArgs` 重复 flag 延续 last-value-wins 行为，不改 CLI 兼容性；`host6Given` 只被赋值后
  空引用，移除不改变显式空 host6 与默认 host6 的分支行为。

### 21.2 稳定性修复

1. **Server cache lazy-init race**：旧 `invalidateCache()` 在未经过 `cacheOnce` 的情况下读取
   `cacheInst`，与并发 `cacheFor()` 初始化构成真正的未同步读写。现在失效路径统一调用
   `cacheFor().invalidate()`；策略写操作很少，首次可能多建一个空 cache，换取正确同步。
   新增多 goroutine 初始化/失效 race 测试。
2. **singleflight loader panic**：panic 若直接逸出，外层 HTTP recover 虽能结束 leader request，
   但旧 inflight entry 和 WaitGroup 永不完成，所有同 key 请求将永久阻塞。现在先删除 inflight、
   把 panic 错误通知等待者并 Done，再重抛给现有 HTTP recover。新增 waiter 解阻与后续重试测试。
3. **cache TTL sweep**：每个 miss 都扫描全部缓存项；许多 query key 在短窗口出现时会产生 O(N²)
   的清理比较。改为每 TTL 批量 sweep，过期项单 key 命中仍立即判 miss；未访问过期项额外驻留
   不超过约一个 TTL。新增过期 key 回收测试。
4. **CSV 导出**：SQLite query 接入 `r.Context()`；扫描/迭代错误在响应已开始后无法重写状态码，
   现在记录结构化 warning；客户端取消时不误报数据库故障。

### 21.3 性能与代码瘦身

- `nodesShape` 基于严格加载器共享的不可变 nodes slice 身份缓存；与 `activityPortIndex` 使用
  独立身份标记，避免一个缓存提前更新 slice 标记、导致另一个缓存误判命中的错误。
- `applyEnforcement` 的 quota/rate 端口展开从 no-op 比较前移到真正要生成/应用 nft 之后；
  并一次遍历 nodes 同时构造两类映射。保持策略 map 比较、shape 检测、外部删表自愈和节流顺序。
- 未启用 IP limit 时，`NodeIPState` 仍维护 IP slots/Observed 供 UI 使用，但 production 路径不再
  构造未消费的 nft allowSet map；公开 `Reconcile` 仍保持原有返回契约。
- `/api` cache version 字符串重用，`cacheKey` 用预留容量的 Builder 一次构建。
- `nodes.DecodeJSON` 与 `config.decodeConfig` 从 `strings.NewReader(string(data))` 改为
  `bytes.NewReader(data)`，移除整文件 byte→string 复制。
- 移除无效 `host6Given`、`unwrapMsg` 一次性包装和陈旧的编译占位；保留被测试直接调用的
  `grantedCount` / `activeGrantedCount` 等测试契约函数。
- service.go 顶部 IP slot 说明从旧 120s 描述修正为当前 60s flow idle grace / 10s provisional TTL。

真机（Debian 12 / Go 1.27.1 / AMD EPYC 7K62）微基准，固定节点/输入：

| 路径 | 原工作 | 新工作 |
|---|---:|---:|
| 50 节点 nodesShape | 23.5µs / 4.86KB / 252 allocs | 6.6ns / 0B / 0 allocs（稳态 cache hit）|
| 限速 no-op 端口展开 | 1.14µs / 208B / 3 allocs | applyEnforcement no-op 182ns / 0B / 0 allocs |
| dataVersion | 102ns / 23B / 2 allocs | 12.5ns / 0B / 0 allocs |
| cache key（daily+days+scope）| 130ns / 64B / 3 allocs | 80ns / 32B / 1 alloc |
| Slot 稳态 50 IP（在线展示、IP limit 关闭）| 11.3µs / 3160B / 5 allocs | 8.2µs / 1280B / 1 alloc |
| Slot 稳态 250 IP（在线展示、IP limit 关闭）| 55.8µs / 19.8KB / 5 allocs | 43.1µs / 6.1KB / 1 alloc |

50 节点×flow 的 reconcile（benchtime=2000x）：v3.0.17 为 50×10 **0.851ms /
269KB / 1852 allocs**、50×50 **3.408ms / 0.961MB / 2153 allocs**；本版为
50×10 **0.825ms / 264KB / 1600 allocs**、50×50 **3.277ms / 0.956MB / 1900 allocs**。
主要稳定收益为每轮少约 252–253 allocs，CPU 时间小幅变化不夸大。

端到端 60s（50 节点×1095 天历史）：CPU **0.73% 单核**、RSS **24.5MB**；
`/api/live` p50/p90 1.8/2.1ms，summary 2.2/2.7ms，daily 7.5ms。RSS 与 Go heap 高水位
受进程预热影响，30s 初测 28.7MB、60s 24.5MB；不声称 RSS 降低。

### 21.4 验证

- 真机 `go vet ./...`、`go test -count=1 ./...`、`CGO_ENABLED=1 go test -race -count=1 ./...` 全通过；
- baseline 61/0；安装器 checksum/commit/installer/dist/hardening 各流程断言全部通过；
- shell 模板已与 sbx.sh 同步；CSV 响应、CLI nodes、nodes/config strict JSON、策略 v4/v6 与表语义回归通过。
- shellcheck 0.10 本地对 baseline/远程脚本检查时返回已有 `SC2034` / `SC2319` 警告；
  安装器大文件检查在 iSH 超时。本轮没有修改 shell 逻辑（仅同步版本常量），不能把本地
  shellcheck 结果计作通过；GitHub CI 的 shellcheck 结果仍是发布门禁。


## 22. v3.0.21：删除配额、增加持久节点暂停

用户明确要求移除全部流量配额并新增节点暂停/启用。

- 移除配额状态、额度/基线字段、每轮 lifetime totals 扫描、自愈修正、重置 API、前端控件、
  卡片数字、配额 nft set/drop 及 quota 专项测试/基准。流量 `daily/totals/samples` 统计保留。
- SQLite migration 在原迁移单事务内为旧 `node_policy` 补 `paused` 与保留策略列，然后在同一
  transaction 中重建表并只复制 `node_id/paused/ip_limit_enabled/ip_limit_max/rate_limit_enabled/rate_limit_mbps`。
  旧 quota 数据因用户选择“彻底移除”而丢弃；迁移失败整体回滚。增加旧 schema 保留策略字段、
  去除 quota 列和新库 schema 回归测试。
- pause 状态落盘 `node_policy.paused`；面板 PUT 未提供 `paused` 时保留旧状态，避免旧客户端覆盖。
  pause 通过自有 `inet sbx_policy` 的 `paused_ports` set，在 priority 200 input dport / output sport
  同时 drop TCP 与 UDP；规则早于 traffic priority 300，因而暂停期间不累计这些连接的流量。
  不触碰 sing-box config、不重启服务，恢复立即去掉 drop，原 IP/限速配置保留。
- 增加启动自愈：进程重启时若 SQLite 已无有效策略但旧 `sbx_policy` 表仍在（例如停面板期间删除节点），
  首轮探测到表后写入空策略目标，清掉可能残留的旧端口 pause/IP/rate 规则。
- `/api/nodes/:id/policy` GET/PUT 仍为策略接口；POST `/quota/reset` 和相关 error code 删除。
  summary/live 暴露 paused；NodeIP/SSE 与流量统计契约保留。
- E2E 策略流程从 quota block 改为 pause→nft 双向规则→流量不可达→resume 恢复，并以 paused
  静态规则测试外部删表自愈；IP allow-set、限速、clear 自有表安全测试继续保留。
