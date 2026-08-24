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

重启该工作的入口：给 `firewall.Backend` 增加第二个实现 `NftNetlink`，
用构建标签或配置项灰度切换；先在 shadow 模式双读比对一个版本周期。

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

## 5. 配置 JSON 键序

Python 版写 JSON 保持插入序，Go 版按键排序。所有消费方均为 JSON 解析器，
语义等价；仅手工 diff 配置文件时观感不同。为保持 diff 友好可引入有序序列化，
但会增加自维护代码，暂缓。

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
