# 把节点订阅托管在 GitHub 上

先给结论，再给做法。

## 三条硬事实

**1. 公开仓库 + GitHub Pages = 完全公开。**
任何人（包括搜索引擎和专门的订阅扫描项目）都能下载，还能被 Google 索引。
把路径设成随机串只是**拖延**，不是加密——一旦有人分享、截图、或客户端日志被看到，
链接就永久有效，直到你换。

**2. 私有仓库不能直接当订阅源。**
`raw.githubusercontent.com` 拉私有仓库必须带 `Authorization` 头，而
sing-box 的远程配置、Shadowrocket 的订阅都**不支持自定义请求头**。
只有 Clash / mihomo 的 `proxy-providers` 支持 `header:` 字段。
所以在 iOS 上主力用 sing-box 的话，私有仓库直链这条路走不通。

**3. GitHub 适合当保险柜，不适合当分发点。**
它能给你稳定的域名和可靠存储，但不能给你**鉴权**。而订阅的本质是
「凭 URL 取走全部节点凭据」，鉴权恰恰是最要紧的一环。
另外，公开的大规模代理分发仓库被举报后可能被处理；私有仓库风险低得多。

## 四种做法

| 做法 | 安全 | 客户端 | 成本 |
|---|---|---|---|
| **A. sbx 自带 `/sub/<token>`**（已经在跑） | 中，URL 即密码 | 全兼容 | 0 |
| **B. A + Cloudflare 代理** | 中高，隐藏源站 IP | 全兼容 | 0，需域名 |
| **C. 私有仓库镜像 + Worker 鉴权** | 高 | 全兼容 | 0，需 CF 账号 |
| D. 公开仓库 + Pages 随机路径 | 低 | 全兼容 | 0 |

**推荐组合：C 做分发，A 做备用。** GitHub 只存一份私有镜像，真正的鉴权在
Worker 上做；面板本身仍然可以直接出订阅，两条路互为备份。

```
客户端 ──HTTPS──▶ Cloudflare Worker ──(PAT, 只读)──▶ 私有仓库 sub.txt
                        │
                        └─ 校验 token / 限频 / 边缘缓存
                                   ▲
                        Actions 每 6 小时 ─┘
                                   │
                              sbx /sub/<token>
```

为什么这算「安全」：

- 私有仓库的明文只在 GitHub 私有空间里，公开面为零；
- 分发点要过 token 校验，且 token 与面板登录令牌、sbx 订阅令牌**三者独立**；
- 仓库和 Worker 的凭据分开：PAT 只读单仓库，泄漏了拿不到面板，也拿不到 Worker；
- 任一处怀疑泄漏，单独轮换即可，不影响其他。

## 部署（方案 C）

### 1. 建一个私有仓库

新建 `sbx-sub`，**Private**。里面只需要两个文件（由下一步自动生成）：

```
sub.txt              # base64 链接列表，给 Shadowrocket / v2rayN
sub-singbox.json     # sing-box 完整配置，给官方客户端
```

### 2. 让它自动同步

把 `github/sub-mirror.yml` 复制到私有仓库的 `.github/workflows/`，然后在
*Settings → Secrets and variables → Actions* 里加两个 secret：

| Secret | 值 |
|---|---|
| `PANEL_BASE` | `https://你的面板地址:端口`（不带结尾斜杠） |
| `SUB_TOKEN` | 面板「配置 → 客户端订阅」里的那串 32 位 hex |

它会每 6 小时拉一次，也可在 Actions 页手动 `Run workflow`。
拉取失败时**不会覆盖旧文件**（先写 `.tmp` 再 `mv`），所以面板挂了订阅还在。

> 面板如果只监听内网、GitHub 的机器够不着，这一步会失败。那种情况改用
> 本地 `scripts/pull-sub.sh` 手动同步，或给 runner 配内网穿透。

### 3. 建一个只读 PAT

GitHub → Settings → Developer settings → **Fine-grained tokens** → 新建：

- *Repository access*：只选 `sbx-sub`
- *Permissions*：**Contents = Read-only**，其他全部 No access
- 有效期设 90 天，到期换新的（换的时候只改 Worker 一个地方）

### 4. 部署 Worker

```bash
cd deploy/sub-hosting/worker
npm i -g wrangler          # 或 npx wrangler
wrangler login

# 先改 wrangler.toml 里的 REPO
wrangler deploy

# 两个密钥，都不进代码库
wrangler secret put SUB_TOKEN   # 自己生成：openssl rand -hex 32
wrangler secret put GH_TOKEN    # 上一步的 PAT
```

### 5. 绑域名（可选但建议）

Workers → 你的 Worker → *Settings → Domains & Routes* → 添加自定义域名。
`*.workers.dev` 在国内经常被污染，绑自己的域名（走 Cloudflare 代理）会稳很多。

### 6. 验证

```bash
curl -sS "https://sub.example.com/sub/<你的SUB_TOKEN>" | head -c 80
curl -sS "https://sub.example.com/sub/<你的SUB_TOKEN>?target=singbox" | python3 -m json.tool | head
```

第一条应输出 base64，第二条应是合法 JSON。两条都对，就可以粘进客户端了。

## 加固清单

- **token 用 32 位 hex**（`openssl rand -hex 32`），别用手机号、别用 `sub` 这种词
- **三把令牌分开**：面板登录、sbx 订阅、Worker 分发，任何一个泄漏都不牵连另外两个
- **Worker 里不要 `console.log(url)`**——路径里就是 token，日志会原样记下来
- **`X-Robots-Tag: noindex`** 已加在响应头里，至少挡住守规矩的爬虫
- **`Cache-Control: public, max-age=300`** 让 Cloudflare 边缘缓存，源站压力降到 1/6 小时
- **限频**：Cloudflare 后台加一条 Rate Limiting 规则（免费版额度有限，套餐不支持就跳过）
- **轮换**：面板里「重置订阅地址」会换掉 `sub_token`；Worker 的 `SUB_TOKEN` 要手动改，
  两边都换完旧链接才彻底失效

### 怀疑泄漏时的顺序

1. 先换 Worker 的 `SUB_TOKEN`（`wrangler secret put`）——这是对外暴露的那个
2. 再在面板里重置订阅地址，并更新私有仓库的 `SUB_TOKEN` secret
3. 换 Fine-grained PAT（撤销旧的）
4. 看 GitHub 仓库的 *Insights → Traffic* 有没有异常克隆

顺序不能反：先换面板那边的，攻击者仍能通过 Worker 的旧 token 拿到缓存内容。

## 一个必须说清的预期

订阅镜像能保证「面板挂了，订阅还能拉到」。但**节点本身在面板那台机器上**，
机器没了，订阅里的 IP 和端口也就没了。它救的是「地址」和「配置」，不是「服务」。

真正的灾备要做的是：把节点配置（密码、SNI、Reality 私钥）加密备份，换服务器后
能照着重建。这一步 `restore.sh` 没做，因为批量重建必须走面板 API，涉及权限更大，
建议保留在面板的导出功能里，别自动化。
