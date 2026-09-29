# 更新日志

本项目使用「版本号 + 提交」双重标识：`APP_VERSION` 不随每次改动递增，
产物由 CI 从 `main` 的每次推送重建并发布到 `dist` 分支（rolling latest），
每次发布在 `dist/archive/<version>/<commit7>/MANIFEST` 留档。

本文件记录**用户可见**与**运维相关**的变更。逐条实现细节见 git log。

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
