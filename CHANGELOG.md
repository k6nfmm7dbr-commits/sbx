# 更新日志

本项目使用「版本号 + 提交」双重标识：`APP_VERSION` 不随每次改动递增，
产物由 CI 从 `main` 的每次推送重建并发布到 `dist` 分支（rolling latest），
每次发布在 `dist/archive/<version>/<commit7>/MANIFEST` 留档。

本文件记录**用户可见**与**运维相关**的变更。逐条实现细节见 git log。

## v3.0.23 — 节点配置管理迁入面板

Web 面板新增「配置」页签，接管节点新增、查看脱敏信息/分享链接、编辑和删除；删除默认保留历史流量，并可在二次确认后选择清除。`sbx` 交互菜单不再列出节点 CRUD。节点密钥由服务器生成，节点列表只返回展示和编辑所需的非密钥信息，分享 URI 通过独立鉴权路由按需查看。删除节点默认保留历史流量；用户可再次确认后选择同步清除该节点的 daily/totals/samples 记录。

新增与编辑流程复用原节点领域校验：候选配置经 `sing-box check` 后再备份并原子提交；节点变更共用 sbx 跨进程锁，重启失败自动恢复旧配置并尝试重新启动旧服务。计数规则和 IP 策略在节点变更后同步，无法重建时保留节点变更并向面板返回 warning。主菜单只保留系统运维、更新与卸载。首页仍使用浅色新设计，无顶部品牌/连接状态栏和速率卡蓝条；不增加新的节点策略功能。

## v3.0.22 — 菜单去重与浅色面板重设计


主菜单移除重复的「流量统计」入口，节点配置子菜单当时仍包含添加/分享/编辑/删除；保留面板未提供的服务运维、面板设置与统计自检/清空。仅调整已有浅色界面层级与排版，移除顶部品牌/状态栏和实时速率卡蓝色装饰条，不增加字段或面板功能。

## v3.0.21 — 移除流量配额，增加节点暂停/启用

按用户要求彻底删除流量配额：面板卡片、节点管理控件、前端状态/API、后端计算、SQLite 配额列与 nft 配额规则全部移除。升级时在 SQLite 单事务内保留节点暂停、IP 上限和限速配置，重建 `node_policy` 表删除旧 quota 字段；迁移失败则事务回滚。每日与累计流量统计不受影响，也不再存在 quota 达限拦截。

节点管理抽屉新增“节点启用”开关。暂停状态持久保存在 `node_policy.paused`，不删除 sing-box 节点、不重启服务；独立 `sbx_policy` 表通过 nft 同时 drop 该节点端口的 ingress/egress 流量，优先于计数链，因此暂停期间不计入流量。重新启用后立即恢复原节点；IP 上限和限速配置保留并继续生效。


## v3.0.20 — 移除配额用量进度条
节点卡片「流量配额」下方的视觉进度条已移除；保留原有已用量 / 配额上限文字、配额状态和管理设置，
没有增加新内容或改动配额计算与限制行为。

## v3.0.19 — 面板视觉整理（仅浅色主题）

仅调整 `style.css`：统一浅色调色板与文字对比度，拉开实时速率、总览指标和节点卡片的层级；
节点既有四项统计改为柔和分区，在线 IP 行、管理按钮、底部导航和管理抽屉的表面/交互反馈统一。
没有增加、删除或改名任何字段、控件、数据或功能；HTML 结构、JS、接口和既有浅色主题保持不变。

## v3.0.18 — 第三轮审计：缓存 no-op 路径、修复缓存并发故障

在 v3.0.17 基础上重新审查 CLI、collector 定时循环、nft v4/v6 生成、缓存惰性初始化、
API 流式导出、节点/配置 JSON 解码与 reconcile no-op 路径。保留 v4/v6 空 allow-set
语义、双 nft 表隔离及外部删表探测，不做会改变 enforcement 行为的简化。

- **消除 nft no-op 热路径重复工作**：节点端口形态摘要 `nodesShape` 按严格 loader
  返回的不可变 slice 身份缓存；quota/rate 的 `node→port` 展开延迟到确定需要写 nft
  之后，且合并为一次节点列表遍历。无状态变化时不再解析端口或创建端口 map。
- **未启用 IP limit 时跳过 allowSet 构造**：仍完整维护 slot / online IP / observed 状态，
  但不再为每个节点每轮生成一张随后丢弃的 nft allow map；对外的 `Reconcile` 原返回契约不变。
- **修复 API cache 惰性初始化的数据竞争**：`invalidateCache` 原先无锁读取 `cacheInst`，
  与并发 `cacheFor` 初始化存在 race 窗口；现在与读取路径统一经 `sync.Once`。
- **修复 singleflight panic 卡死**：缓存 loader panic 时先删除 inflight、通知等待者，
  再重抛由 HTTP recover middleware 处理；同 key 后续请求可重试。新增并发回归测试。
- **降低 TTL 清理成本**：cache miss 不再每次扫描全部 entry；按 TTL 间隔批量清理，
  命中的单项仍即时检查过期。过期未访问 key 最多多驻留约一个 TTL，map 不会无界增长。
- **降低 API 缓存键分配**：缓存 `dataVersion` 字符串，并用 `strings.Builder` 一次分配
  组装 key；真机微基准分别由 102ns/23B/2 allocs→12.5ns/0B/0 allocs，及
  130ns/64B/3 allocs→80ns/32B/1 alloc。
- **减少 JSON 输入复制**：配置与节点 `DecodeJSON` 直接用 `bytes.Reader`，去除整文件
  `[]byte→string` 复制。
- **导出稳定性**：CSV 使用请求 context 运行 SQLite query，客户端断开可取消读取；记录
  rows.Scan/rows.Err 中断日志，避免响应头发送后数据库错误静默变成“成功下载”。
- **瘦身**：删除 CLI links 中无效的 `host6Given` 状态及占位引用；内联仅单调用的
  `unwrapMsg`，删除已无意义的编译引用占位；修正文档中 IP slot 的过期 TTL 描述。

### 真机数据

Debian 12 / Go 1.27.1 / 2 核测试机：50 节点 × 50 flow，稳态 reconcile
`nodesShape` **23.5µs/4.86KB/252 allocs → 6.6ns/0B/0 allocs**；nft 端口映射旧
no-op 工作 **1.14µs/208B/3 allocs**，新 `applyEnforcement` no-op **182ns/0B/0 allocs**。
未启用 IP limit 但仍维护在线 IP 的 slot 稳态：50 IP **11.3µs/3160B/5 allocs →
8.2µs/1280B/1 alloc**；250 IP **55.8µs/19.8KB/5 allocs → 43.1µs/6.1KB/1 alloc**。
最终 reconcile（2000x）：50×10 **0.825ms/264KB/1600 allocs**；50×50
**3.277ms/0.956MB/1900 allocs**，相对 v3.0.17 同机基准分别少约 252/253 allocs。

60s 端到端：CPU **0.73% 单核**、RSS **24.5MB**；live p50/p90 **1.8/2.1ms**，
summary **2.2/2.7ms**，daily **7.5ms**。与既有真机结果同量级；RSS 有 Go heap
高水位波动，不宣称内存驻留下降。



## v3.0.17 — 二次审计：减少 reconcile 重复工作与维护面

在 v3.0.16 profile 优化后的代码上再次做全仓 deadcode / vet / race / 真机 A/B
复核，新增两项低风险优化：

- **缓存节点端口归属索引**：`buildActivity` 不再每秒重新遍历节点、调用
  `ParsePorts`、构造 `port→node` map；复用严格节点加载器返回的不可变 slice，
  节点文件原子替换后 slice 身份变化即自动重建。新增端口变更失效回归测试。
- **单遍构造策略 IP 快照**：reconcile 原先分别遍历 `Slots/Observed` 构造
  `NodeIPSnapshot` 与 active IP 列表；现在一次遍历同时生成两个不可变视图，
  并沿用原有 IP 排序 / last-active 语义。`ActiveIPs` 计数从该 snapshot 复用，
  避免额外 `activeGrantedCount` 扫描。构造函数包装仍保留给测试契约。

真机（192.220.32.203，Debian 12，2 核 / 2GB，50 节点 × 50 flow，benchtime=2000x）
复测：reconcile **3.225ms / 0.966MB / 2210 allocs → 3.408ms / 0.961MB /
2153 allocs**（单次运行受调度噪声影响；分配稳定减少约 57 次/轮）。50 节点 × 10
flow 为 **1.189ms / 404KB / 3624 → 0.851ms / 269KB / 1852 allocs**；端到端
最终 60s：CPU 0.74% 单核、RSS 21.6MB、live/summary p50 1.9/2.3ms；无可见
稳定性或 API 行为回归。

本版仍以稳定性优先：无激进的 SQLite schema 改写、无运行时缓存失效猜测、无
nftables 语义改变；完整验证见 `FUTURE_IMPROVEMENTS.md §20`。

## v3.0.16 — reconcile 热路径优化（真机 CPU profile + A/B）

本轮不是凭感觉优化：先在真机跑 50 节点 × 50 活跃 IP 的 reconcile 基准并采
CPU profile，定位到稳态成本集中在 IP slot 排序、flow tracker 每轮字符串/对象
分配、以及为 GC 构造整轮 current-flow 集合；再逐项改、逐项 A/B。

### 性能（真机同机、同一组 benchmark，基线 v3.0.15 vs 本版）

- **IP slot 稳态不再排序，并移除冗余 seen map**：IP 集合没有新 admission 时，
  排序不会改变任何 slot 决策，直接跳过；输入 active/candidates 本身已经是去重 map，
  不再每轮构造第三张 `seen` 临时表。出现新 IP 时仍保留原优先级/FirstSeen/IP
  tie-break。50 IP 稳态单节点 `NodeIPState.Reconcile`：**43.9µs → 10.5µs
  （-76%）**；250 IP：268µs → 55.5µs（-79%）。
- **conntrack flow tracker 去分配**：key 从每轮拼接的字符串改为结构体键
  `(nodeID, srcIP, srcPort)`；value 由 `*flowState` 改内联 `flowState`；GC 用持久
  `SeenEpoch` 标记替代每轮新建 `currentFlowKeys` 全量 map。计费模式提示也并入
  主 flow 扫描，不再单独重复遍历 conntrack 列表。IP 只在首次进入长期状态时
  `strings.Clone`，防止 key 子串使整份 conntrack 文件缓冲区（可能数 MB）长驻堆。
- **展示排序改 typed sort + 稳态快路径**：使用 `slices.SortFunc` 移除反射
  `sort.Slice` swapper；`buildActiveIPsFromState` 在通常“所有 LastSeen 同一轮”
  情况只生成一个 IP 切片、按 IP 排序；时钟回拨等时间确实不同时仍走原完整
  last-active 倒序规则。
- **关键 fail-closed 缓存补强**：节点文件缓存命中除 `(mtime,size)` 外再用
  `os.SameFile` 比较身份；即使原子 rename 后新旧文件刚好同大小、mtime 被保留，
  也必然失效重读（有回归测试）。

| reconcile workload | v3.0.15 | v3.0.16 | 改善 |
|---|---:|---:|---:|
| 50 节点 × 10 活跃 IP：ns/op | 1.189ms | **0.905ms** | **-24%** |
| 同 workload：B/op / allocs | 404KB / 3624 | **273KB / 1909** | **-32% / -47%** |
| 50 节点 × 50 活跃 IP：ns/op | 5.963ms | **3.225ms** | **-46%** |
| 同 workload：B/op / allocs | 1.538MB / 8141 | **0.966MB / 2210** | **-37% / -73%** |

profile 优化后 50×50 reconcile pprof Top 从 `NodeIPState.Reconcile`（约 31% cum）
转为 `buildActivity` / map hashing / runtime scan，后续若继续优化应先针对真实
代理连接数 workload 采样，而不是继续微调排序。

端到端复测（2 核真机、50 节点 × 1095 天）：90s 采样 CPU 0.76% 单核 / RSS 22.6MB；最终修复状态发布顺序后再跑 60s，CPU **0.74%** / RSS **21.6MB**，`/api/live` p50 1.9ms、`/api/summary` p50 2.3ms、`/api/daily` 7.1ms（仅 1 个样本）。v3.0.15 基线此前 151s 采样为 CPU 0.81% / RSS 21.4MB（窗口长度不同，端到端数字只作方向参考）；无可见 CPU/延迟回归，RSS 仍处 21–23MB 区间（Go heap 高水位/机器连接数有波动）。主要收益在重度活跃 IP 数下 reconcile 的可量化余量与每轮 GC 压力，而非空载面板体感。

### 正确性验证

- `go test ./...`、`go vet ./...`、`go test -race ./...` 全绿；真机 Debian 12
  x86_64（2 核/2GB，gcc + nftables）。
- 回归测试覆盖：新 active 抢占 provisional、SYN admission 优先级、
  stale flow GC、last-active 排序与同刻 IP tie-break、1MiB conntrack 底层缓冲
  不被 flow/IP 长期状态保留、同 mtime/size 原子替换节点文件仍失效。
- 端到端 API schema、nft enforcement 与配置语义不变。


## v3.0.15 — 代码瘦身 + 降低稳态开销

一轮以真机实测驱动的瘦身，核心是消除稳态下最大的一项重复固定开销。

### 性能

- **`LoadPanelNodesStrict` 加文件身份 + (mtime,size) 缓存**：该函数在策略 reconcile（1Hz）
  与采集器（0.5Hz）的热路径上每轮都调用，但 `nodes.json` 只在人工
  add/edit/remove 时经原子 rename 改写。改为按 `(mtime, size)` 命中缓存、复用
  上轮解析结果。真机实测（2 核 VPS）：
  - 50 节点：**340µs → 1.35µs，87KB/1386 allocs → 272B/2 allocs**（每次读只剩一次 stat）；
  - 稳态每秒省下一次完整文件读 + JSON 解析 + 语义校验。
  - **fail-closed 语义不变**（关键，有回归测试锁定）：只有解析成功才写缓存；
    损坏/读失败一律返回 error 且不缓存，reconcile 继续保持上一轮 enforcement；
    原子 rename 保证不会读到半写文件，文件身份 / mtime / size 任一变化即失效重读，
    绝无"用缓存的好结果掩盖当前损坏"的可能。

### 瘦身

- 删除 5 个确认不可达的死函数（`deadcode` 工具 + 全仓引用核实）：
  `connection.CountByPort` / `connection.NodeRemoteIPs` / `fsx.WriteJSONAtomic` /
  `traffic.TimeIn` / `traffic.TodayStr`（及顺带简化 `CountByPortFiltered` 的文档）。
  均为历史迁移期遗留的导出 API，无任何生产或测试调用方。

### 兼容性

- 无行为变更、无 API schema 变化。纯内部优化 + 删死码；全部单元测试与
  `-race` 通过，端到端性能（CPU 0.81% / RSS 21MB）与改前持平（本项目稳态
  瓶颈本就不在 CPU，本轮价值在消除隐性的每秒重复解析与减小维护面）。

## v3.0.14 — 面板改为浅色主题

- **面板 UI 由深色改为浅色（且仅浅色）**：重写 `:root` 调色板为浅色系
  （白底 `#fff` 卡片、`#f4f5f7` 页面背景、深色文字），并相应调整所有
  半透明层（顶栏 / 底部导航玻璃、抽屉遮罩与投影、开关旋钮、进度条底色、
  表格 hover、Toast 与登录卡片投影）。状态色（上传绿 / 下载蓝 / 警告 /
  危险 / 强调蓝）改为在浅底上对比度达标的深色版本。
- `<meta name="color-scheme">` 与 `html { color-scheme }` 均置为 `light`，
  面板不再跟随系统深色模式（明确锁定浅色）；登录页同步声明。
- 纯前端改动（`internal/webui/static` 的 CSS + 两处 meta），无 JS 逻辑、
  无 API 变化，所有 DOM 钩子与状态类不变。

## v3.0.13 — 面板 UI 调整

- **节点卡片统计区重构**：由 2+2+1 的不规则网格改为整齐 2×2——
  「累计 / 今日」（同一单元格内以 / 分隔，与 TCP/UDP 一致）｜「流量配额」｜
  「限速」｜「TCP / UDP」（合并一格，消灭原来的孤行）。流量配额未启用时
  显示弱化的「不限」（不再重复展示累计值）；启用时显示「已用 / 上限」并在
  数值下方加用量进度条（≥90% 变黄、达限变红），与状态徽标呼应。
- **抽屉单位下拉去掉箭头**：管理抽屉里 GiB/TiB、Mbps 的单位选择不再是
  "下拉框"形态（单位即文案，箭头属多余装饰）；顶部节点选择器的箭头保留。
- 纯前端改动（`internal/webui/static`），API schema 无变化；实时速率、
  在线 IP 的 DOM 钩子（`data-node-live` / `data-node-ips`）保持兼容，
  高频刷新逻辑不受影响。


## v3.0.12 — 热路径性能与观测性优化（全部真机实测）

本轮为一次以测量驱动的优化轮：先在 1 核 VPS（Debian 12，50 节点 × 3 年历史数据，
真实 nftables，按前端真实轮询节奏压测）建立基线，再只改**实测有收益且写路径
无可测回归**的项。基线：空闲 CPU 0.27% 单核、RSS 20.7MB——确认性能不是本项目的
主要矛盾，因此本轮以「消除随运行年限线性变差的项 + 提升故障可见性」为主。

### 性能

- **daily 表双覆盖索引**：`(day,scope,rx,tx,rx_pkts,tx_pkts)` 与
  `(scope,day,...)`。趋势查询只走索引、不回表。真机实测（50 节点 × 1095 天）：
  全节点 180 天趋势 4.11ms → 1.90ms，单节点趋势 0.83ms → 0.14ms；
  端到端 `/api/daily` p50 4.9ms → 2.5ms。写入侧（每 tick 每 scope 一次
  upsert，事务内）三个索引方案对比无差异（差异在噪声内）。存量库升级时由
  `IF NOT EXISTS` 自动补建。
  被否决的替代方案（实测记录）：`IN` 子查询 / `JOIN` 子查询限天数——三者的
  扫描量与现状完全一致（SQLite 不做 GROUP BY 提前退出），两步法反而因
  180 个占位符更慢。
- **SSE 跨连接共享序列化**：reconcile 每秒广播时，旧实现每个 SSE 连接各自
  对全部节点做 JSON marshal（哪怕内容没变）。现按 `policy.Version()` 缓存
  「节点 → payload」，同一版本的所有连接共享一份序列化结果。N 个浏览器
  标签页时每秒少做 N-1 份全量 marshal；正确性由版本号单调性保证
  （版本不变 ⇔ 快照不变）。
- **conntrack 整表读取去重复制**：`os.ReadFile` 后再 `string(b)` 会隐式
  拷贝整张表（繁忙服务器可达数 MB、每秒一次）。改用零拷贝视图，生命周期
  由解析结果的子串引用维持。
- **`/api/export` 改流式**：旧实现把整张 daily 表读进内存拼完 CSV 再写响应
  （大库有数 MB 到数十 MB 瞬时尖峰）；现逐行读、逐行写，驻留内存与单行等价。
  输出内容与响应头不变（回归测试锁定）。

### 运维 / 观测性

- **`/healthz` 追加降级详情**：`collector_error` / `policy_error` /
  `sample_age_s`（距上次成功采样的秒数）。HTTP 状态与 `ok:true` 语义完全
  不变——一切正常时输出与旧版逐字节一致，既有探活脚本不受影响；降级时
  监控与用户能一眼区分「进程活着」与「采集/策略在报错」。

### 兼容性

- **无破坏性变更**：API schema 仅追加字段；数据库仅新增索引；SSE 事件格式
  不变（只是序列化结果共享）。
- `/api/export` 在导出中途数据库出错时会输出截断的 CSV（旧实现返回 500）。
  这是流式化的固有取舍：CSV 语义上「残缺文件」比 500 更接近真实情况。

### 测试

- 新增：覆盖索引查询计划断言（`EXPLAIN QUERY PLAN` 必须命中覆盖索引）、
  索引重复迁移幂等、healthz 干净态逐字节一致 / 降级态字段、SSE 共享缓存
  （版本未变不重复序列化 / 版本变化重建 / 内容与直接序列化逐字节一致）。
- 新增 benchmark：`internal/traffic` 的 QDaily 全量/单 scope 与 commitTick
  写路径（本文件的数字都来自它）。

## v3.0.11 — 节点限速 + 趋势窗口延长

### 新增

- **节点限速（Mbps）**：Web 面板 → 节点管理抽屉新增「限速」开关，可为每个节点
  单独设置带宽上限（单位 Mbps，上传 / 下载各自独立限到该值）。由 nftables
  `limit rate over ... drop` policer 在内核执行，不引入 tc/qdisc；与 quota / IP 上限
  相互独立。据实说明其性质：这是**限速器（policing，丢弃超额包）而非整形器
  （shaping，排队）**——对 TCP 仍能有效限流（丢包触发拥塞回退），但不如 tc HTB
  平滑、会有少量重传。节点卡片新增「限速」一行展示当前状态。
- **每日趋势窗口 60 → 180 天**：面板「趋势」页与「节点详情」页的每日流量表由最近
  60 天扩展到 180 天（`/api/daily` 后端上限仍为 365 天，未改）。

### 数据

- `node_policy` 表新增 `rate_limit_enabled` / `rate_limit_mbps` 两列；旧库升级时
  由迁移逻辑 `ALTER TABLE ADD COLUMN` 无损补列（默认 0 = 不限速），既有配额 /
  IP 限制配置原样保留。

### 兼容性

- **无破坏性变更**：`/api/summary` 与 `/api/nodes/<id>/policy` 仅**新增**
  `rate_limit_enabled` / `rate_limit_mbps` 字段（omitempty，未启用不输出）；
  其余 API 路由、JSON 字段、CLI、`panel.json` 结构均不变。

## v3.0.10 — 稳定性与安全审计

### 安全

- **不再向客户端透传内部错误详情**：500/503 响应改为返回稳定的
  `error_code` + 通用文案 + `request_id`，完整错误只写服务端日志
  （此前会泄漏文件绝对路径、SQL 片段、nft 报错）。响应头新增
  `X-Request-Id`，便于用户报障时与日志对齐。
- **空 token 防御**：`tokenEqual("", "")` 原返回 true（两个空串长度相等且
  `ConstantTimeCompare` 对空切片返回 1）。当前调用方已短路故不可利用，
  已改为任一侧为空一律 false。
- **配置路径校验**：`db` / `nodes_file` / `nft_conf` 必须为绝对路径、
  不含 `..`、不位于 `/proc` `/sys` `/dev`；非法配置在 `LoadStrict`
  阶段 fail-closed（serve / config-set / apply 均受影响）。
- **安装器环境变量白名单**：`SBX_GH_PROXY` / `SBX_ROOT` /
  `SBX_SCRIPT_SHA256` / `SBX_SB_VERSION` 做格式校验，非法取值在任何状态改动
  之前中止并说明原因。
- **安装器支持 SHA256 自校验**：新增 `SBX_SCRIPT_SHA256`，提供后脚本会校验
  自身哈希，不匹配立即中止；`curl | bash`（无法回读自身）会明确报错而非静默跳过。
- **升级 Go 工具链到 1.27.1**：`govulncheck` 归零（原 26 个漏洞全部来自
  go1.23.9 标准库，涉及 crypto/tls、crypto/x509、net/url、net/http、os/exec）；
  `golang.org/x/sys` 升到 v0.48.0（GO-2026-5024）。
- **安装器改为原子替换 sing-box 二进制**：原 `install` 原地截断写入，
  升级中断会给运行中的服务留下损坏二进制。

### 性能

- **策略层不再做无用功**：`reconcile` 此前每秒无条件读取并解析 4 个
  `/proc` 文件，而 conntrack 可用时该结果根本不被消费。真机 A/B 实测：
  空载 3.78% → 3.33% 单核；约 2400 连接下 7.27% → 5.27%（-27.5%）。
- **conntrack / `/proc` 解析去分配**：行与字段解析改索引扫描 + 复用缓冲。
  `RemoteIPsByPort` 在 1 万连接下从 14.36ms / 3.08MB / 40228 allocs
  降到 6.31ms / 2.4KB / 18 allocs（新增端口过滤：不再为每个临时源端口建 map）。
- **`/api/summary`、`/api/live`、`/api/daily` 短 TTL 缓存**：key 带数据版本
  （采集器采样时间 + 策略快照版本），数据一变 key 就变；带单飞防击穿；
  缓存最终 JSON 字节。
- **SQLite 启用 `synchronous=NORMAL`**（WAL 推荐档位），并可用
  `SBX_SQLITE_SYNCHRONOUS=FULL` 覆盖回零丢失模式；新增
  `PRAGMA journal_size_limit` 限制 WAL 高水位。
- **nft 计数器读取合并**（single-flight）：并发 Read 共享一次 exec，
  且不缓存上一次结果（无计数陈旧风险）。

### 可靠性

- **panic 恢复修复**：中间件改为在 `ServeHTTP` 内统一装配（此前挂在
  `http.Server` 上，直接用 `Server` 当 handler 的路径会静默绕过）；
  响应已开始写出时不再重复写头；`trackingWriter` 保留 `http.Flusher`
  与写截止时间控制（SSE 依赖）。
- **dist 发布可追溯**：新增 `dist/archive/<version>/<commit7>/`（含
  SHA256SUMS、安装器、MANIFEST，保留最近 20 个版本）与版本 Tag；
  `dist/sbx.sh.sha256` 随发布公布，供"先校验再执行"。

### 文档

- README：新增「安全安装方式」（下载 → 校验 → 执行）、支持矩阵
  （发行版/init/包管理器、架构）、「版本化下载与回退」。
- 安装器：nftables 缺失时给出按发行版可照抄的安装命令与三类排查方向。
- FUTURE_IMPROVEMENTS §15/§16：记录 netlink 直读、conntrack/eBPF 换数据源、
  SQLite 升级、脚本拆分、CLI 框架、embed 拆分等项的**实测数据与不采纳理由**。

### 兼容性

- **无破坏性变更**：API 路由、JSON 字段、CLI 子命令与输出格式、
  `panel.json` 键、`nodes.json`/`traffic.db` 结构均保持不变
  （仅**新增** `error_code`/`request_id` 字段与 `X-Request-Id` 响应头）。
- 构建要求：Go **1.27.1+**（原 1.23.9）。产物仍为静态单二进制、无 cgo。

## v3.0.9 — nftables-only 架构收敛

- 彻底移除 iptables 后端、后端选择与回退路径；`panel.json` 不再有
  `backend` / `ipt_script` 键（升级时自动清理，其余配置原样保留）。
- 计数表与策略表分离（`sbx_traffic` / `sbx_policy`），互不覆盖。

（更早版本的变更见 git log 与 `docs/AUDIT.md` 的历史存档说明。）
