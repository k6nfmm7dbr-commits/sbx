# SBX

sing-box 节点搭建与内核流量统计面板。自用、轻量、无多用户、无分流、无订阅转换。

**v3.0.0 起：后端为 Go 单二进制 `sbx-core`，服务器不再需要 Python 运行时。**

## 架构

```text
sbx.sh（安装 / 更新 / 卸载 / 交互菜单 / 服务管理）
   │
   ├── sing-box          节点数据面（官方二进制）
   └── sbx-core (Go)     控制面单二进制
        ├─ Collector      nftables/iptables 计数器单调差分 → SQLite
        ├─ HTTP API       /api/summary /api/live /api/daily ...（与前端契约不变）
        ├─ Nodes          节点增删改 / 分享链接 / sing-box 配置重建
        └─ webui          //go:embed 内嵌前端（升级二进制即升级 UI）
```

## 安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh)
```

安装后运行 `sbx` 打开命令菜单。安装器会：

1. 检测系统（Debian/Ubuntu、RHEL 系、Alpine）与 init（systemd/OpenRC）；
2. 按 `uname -m` 从 GitHub Releases 下载对应架构的 `sbx-core` 并校验 SHA256，
   失败则报错退出且不破坏现有安装；开发可用 `SBX_CORE_BIN=/path/to/sbx-core` 跳过下载；
3. 安装 sing-box（Alpine 自动选 musl 构建）；
4. 注册并启动三个服务：`sbx-firewall`（计数规则，oneshot）、`sing-box`、`sbx-panel`。

## 当前版本

```text
v3.0.3
```

## 节点

支持 VLESS Reality、Shadowsocks 2022、Trojan、AnyTLS。菜单两级结构与 v2.8.0 完全一致：
主菜单（添加节点 / 节点管理 / 流量统计 / 系统设置 / 检查更新 / 卸载）；支持 IPv4/IPv6 分享链接与 Base64 订阅。
节点按 `nodes.json` 添加顺序显示；ID 单调递增永不复用（历史流量不会串节点）。

## 统计口径（与 2.x 一致）

- 数据来自内核 netfilter 计数器：优先 nftables named counter，回退 iptables 自定义链；
- rx = 服务器收到（=用户上传），tx = 服务器发出（=用户下载），含包头（比客户端显示高约 2%～5%）；
- 单调差分累加：首次见到计数器只入累计不计速率；计数器归零补记当前值不制造假峰值；
- 规则集带世代标记（epoch），规则重建后面板自动衔接，零丢计零重复；
- 默认每 2 秒采集，实时速率按真实 `duration_ms` 计算，samples 仅保留约 2 分钟；
- 中国时间 UTC+8 跨天（内嵌 tzdata，无时区库的系统同样正确）。

TCP 连接读 `/proc/net/tcp[6]`，UDP 会话读 `/proc/net/udp[6]`，由采集线程缓存供 API 直接读取。

## Web 面板

三页底部导航（首页 / 每日 / 节点），令牌登录（POST 提交，登录后使用 HttpOnly Cookie）。
前端资源已内嵌进 sbx-core，磁盘上不再有可被旧文件遮蔽的副本；
`panel.json` 的 `web_root` 键保留解析但不再读取。

## 在线升级

```bash
sbx --update            # 拉取新 sbx.sh + 匹配版本的 sbx-core（SHA256 校验）
sbx --update --force    # 强制重装
```

升级保留节点配置、面板端口和全部流量历史。二进制原子替换，失败自动回滚旧版。

## 常用命令

```bash
sbx                     # 管理菜单
sbx --show              # 今日/累计流量
sbx --links             # 分享链接
sbx --panel-url         # 面板地址
sbx --apply-firewall    # 重建计数规则
sbx --uninstall         # 卸载

sbx-core show | daily 60 | selftest | reset node:2     # 直接调用后端
sbx-core node list --json | links | add ...            # 节点管理
```

## 支持环境

Debian/Ubuntu、RHEL 系、Alpine；systemd/OpenRC。
CPU 架构：amd64、arm64、armv7、armv6、386、s390x、riscv64（纯 Go SQLite，CGO_ENABLED=0）。

依赖：curl、openssl、tar、nftables 或 iptables。**不再依赖 Python。**

## 文件

```text
/usr/local/bin/sbx-core        Go 后端（含内嵌前端）
/usr/local/bin/sbx             菜单入口（封装 sbx.sh）
/etc/sbx/panel.json            面板配置
/etc/sbx/nodes.json            节点唯一数据源
/etc/sbx/state.json            ID 游标 / 分享地址
/etc/sbx/traffic.db            SQLite 流量库（WAL）
/etc/sbx/nft.conf              计数规则（nftables）
/etc/sbx/iptables.sh           计数规则（iptables 回退）
/etc/sing-box/config.json      sing-box 配置（仅重建 tag 以 sbx-n 开头的 inbound）
/etc/systemd/system/sbx-{panel,firewall}.service 等
```

配置修改前执行 `sing-box check`，失败自动回滚（候选文件机制不变）。

## 开发

```text
cmd/sbx-core/            入口
internal/{api,collector→traffic,database,nodes,config,traffic,connection,firewall,service}
internal/webui/static/   前端四文件（//go:embed）
installer-template.sh    sbx.sh 模板（python3 build.py 直出发布脚本）
src/panel.py, src/nodes_tool.py   Python 参考实现（仅用于回归对拍，不再部署）
tests/gen_goldens.py     用参考实现生成金标夹具（Go 测试对其对拍）
tests/run_all.py         旧 Python 回归套件（55 项）
scripts/build-release.sh 七架构交叉编译 + SHA256SUMS
scripts/e2e_remote.sh    真机端到端验收（安装/流量/换代/重启/对拍/卸载）
docs/AUDIT.md            迁移前行为审计（Go 实现的行为基线）
```

```bash
go test ./... && go test -race ./...    # 单元 + 金标回归
python3 tests/gen_goldens.py            # 再生金标（CI 中校验无漂移）
python3 build.py                        # 生成发布版 sbx.sh
./scripts/build-release.sh dist         # 构建全部架构产物
```

沙箱安装（不动真实系统）：

```bash
SBX_ROOT=/tmp/sbx-test SBX_NO_SERVICE=1 \
SBX_CORE_BIN=$PWD/dist/sbx-core-linux-amd64 SBX_SB_BIN=/path/to/sing-box bash sbx.sh
```
