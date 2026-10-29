# sbx-sub

SBX 节点订阅的**私有保险柜**。

这个仓库唯一的用途：给 Cloudflare Worker 提供一份可读的订阅副本。它必须保持 **private**，改成 public 等于把节点密码贴到公网上。

## 里面有什么

| 文件 | 来源 | 说明 |
|---|---|---|
| `sub.txt` | `GET /sub/<token>` | base64 编码的链接列表，Shadowrocket / v2rayN 用 |
| `sub-singbox.json` | `GET /sub/<token>?target=singbox` | sing-box 完整配置，官方客户端「添加远程配置」用 |
| `.github/workflows/sub-mirror.yml` | 本仓库 | 每 6 小时从面板拉一次 |

两个数据文件由 Actions 自动生成，**不要手动编辑**——下次运行会被覆盖。

## 需要配置的 Secrets

`Settings → Secrets and variables → Actions → New repository secret`：

| 名称 | 值 |
|---|---|
| `PANEL_BASE` | 面板地址，带协议和端口，例如 `http://1.2.3.4:8080`，**结尾不要加斜杠** |
| `SUB_TOKEN` | 面板「配置 → 客户端订阅」里那串 32 位 hex |

配好后去 `Actions → mirror-sub → Run workflow` 手动跑一次，确认 `sub.txt` 出现。

## 私有仓库为什么还需要 Worker

`raw.githubusercontent.com` 拉私有仓库必须带 `Authorization` 头，而 sing-box 的远程配置和 Shadowrocket 的订阅**都不支持自定义请求头**（只有 Clash / mihomo 的 `proxy-providers` 有 `header:` 字段）。所以不能让客户端直连本仓库，必须经 Worker 鉴权转发。

## 安全须知

- **不要**给这个仓库开 GitHub Pages，也不要用 `github.dev` 之外的方式公开链接
- 给 Worker 用的 PAT 只勾选本仓库、只给 `Contents: Read-only`，不要复用账号级 token
- `sub.txt` / `sub-singbox.json` 里是明文节点密码。本地 `git clone` 后别把目录放进任何同步盘
- Fork 本仓库不会带出文件（私有库本身不可 fork），但**别把它转成模板库**

## 令牌轮换顺序

顺序反了会留一个可用的时间窗：

1. 先更新 Worker 的 `SUB_TOKEN`（`wrangler secret put SUB_TOKEN`），`wrangler deploy` 生效
2. 再更新本仓库的 `SUB_TOKEN` secret
3. 最后在面板点「重置订阅地址」

如果先重置面板，Worker 仍持旧 token 且缓存里还有旧内容，攻击者能继续拉到一份有效订阅，直到缓存过期。

## 停用

不想再用这套：删掉本仓库的 `sub-mirror.yml`（Actions 停止），然后删 Worker。面板本身的 `/sub/<token>` 不受影响，可以继续直连使用。
