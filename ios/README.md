# SBX iOS 端

这里有两个**互相独立**的 App，分别打开各自的 `.xcodeproj`：

| 工程 | 做什么 | 需要什么 |
|---|---|---|
| `TrafficMeter.xcodeproj` | 本机流量统计，区分蜂窝 / Wi-Fi | 免费 Apple ID，7 天签一次 |
| `SbxPanel.xcodeproj` | sbx 面板的 WKWebView 壳 | 同上 |

两个都用免费签名可装，都不需要付费开发者账号。

---

# 一、流量（TrafficMeter）

## 为什么不是「快捷指令」

系统**没有**任何能读到流量字节的快捷指令动作。快捷指令能拿到的只有
「当前是 Wi-Fi 还是蜂窝」「Wi-Fi 名称」这类状态，拿不到用量。

唯一可用的数据源是 `getifaddrs()` 返回的**网卡累计字节**，只有 App 内能读。
所以这里做成「App + App Intent」：数字由 App 采集，快捷指令通过
「获取今日流量」这个动作去取。这样既有真实数据，又能挂自动化：

> 快捷指令 → 自动化 → 每天 23:00 → 获取今日流量 → 朗读文本

## 口径（这三条决定了数字准不准）

**1. 只读物理网卡，不读隧道。**
Wi-Fi 取 `en0`，蜂窝取 `pdp_ip0…3`，累加。刻意跳过 `utun*`。

**2. 计数是内核累计值，重启才归零。**
App 存「基线」，每次采样做差。所以 App 关着的时候流量照样在涨，
下次打开一次性补上——**总量永远准**，只是「哪天用的」靠时间比例摊。

**3. 用 1000 进制。**
和 iOS「设置 → 蜂窝网络」一致。用 1024 会和系统数字差 7%，第一眼就觉得对不上。

## 和代理软件共存（重点）

**不会重复计数。** 开着 sing-box / Shadowrocket / Surge / Loon 时，流量路径是：

```
App → utun 隧道 → 代理进程 → 物理网卡（en0 / pdp_ip0）→ 网络
```

隧道里的字节最终仍然经过物理网卡，所以只读物理层，物理层只会计**一次**。
反过来，把 `utun` 也算进来才是双倍计数——这是同类工具最常见的错，
这里的 `InterfaceStats.read()` 注释里写明了为什么跳过它。

几个推论：

| 场景 | 统计结果 |
|---|---|
| 代理开启，全部流量走节点 | 正常计入，等于运营商侧看到的量 |
| 代理关闭 | 同样正常 |
| 代理软件自己握手 / 更新订阅 | 计入（确实是真实消耗） |
| 个人热点共享给别的设备 | 计入 `pdp_ip0`（确实吃你的套餐） |
| 访问局域网 / 本机地址 | 走 `lo0`，不计入（不算上网流量） |

**和代理软件自带面板差 3–8% 是正常的**，不是 bug：代理多半只报载荷字节，
而物理网卡还含 TCP/IP 头、TLS 记录开销、重传。

## 做不到的

- **按 App 拆分**。那份数据在系统「设置 → 蜂窝网络」里，没有公开 API，
  任何第三方 App 都读不到。想要「哪个 App 用了多少」，只能看系统那一页。
- **回溯安装前的用量**。计数器的历史值拿不到，只能从首次打开开始记。
- **后台精确到小时**。系统调度 `BGAppRefreshTask` 不保证（可能几小时才跑一次），
  所以「几点用的」有误差，但总量不受影响。

## 界面

- 今日总量 + 蜂窝 / Wi-Fi 比例条
- 两张分项卡：总量、下行、上行
- 实时速率（仅前台，每 2 秒）
- 近七日柱状图（下段墨色 = Wi-Fi，上段朱砂 = 蜂窝）
- 本月合计
- 顶部在有代理连接时显示「代理已连接」小标

## 装到手机

1. `open TrafficMeter.xcodeproj`
2. 选中 `TrafficMeter` target → *Signing & Capabilities*
3. **改 Bundle ID**：`com.example.trafficmeter` 换成你自己的唯一串
4. Team 选 *Personal Team*（普通 Apple ID 即可）
5. iPhone 连线 → 选设备 → `⌘R`
6. 首次：设置 → 通用 → VPN与设备管理 → 信任

**改了 Bundle ID 之后，`Info.plist` 里的 `BGTaskSchedulerPermittedIdentifiers`
和 `AppDelegate.refreshIdentifier` 也要跟着改**，否则后台刷新注册失败
（不崩，只是永远不后台采样）。两处必须逐字一致。

7 天后闪退，重连 Mac 再 `⌘R` 一次，历史数据不丢（存在 Application Support）。

## 文件

```
TrafficMeter/
├── TrafficMeterApp.swift     @main，scenePhase 处理
├── AppDelegate.swift         BGTask 注册与调度
├── InterfaceStats.swift      getifaddrs 读数、VPN 探测、开机时刻、字节格式化
├── TrafficStore.swift        基线差值、按天分摊、持久化、给快捷指令的文案
├── ContentView.swift         水墨风界面
├── TrafficIntents.swift      App Intent（快捷指令 / Siri）
├── Info.plist
└── Assets.xcassets/
```

---

# 二、面板（SbxPanel）

一个 **WKWebView 壳**，把 sbx 面板包成 iPhone 上的 App。不是代理客户端——
真正连节点用 [sing-box 官方 iOS 客户端](https://apps.apple.com/app/sing-box/id6451272673)，
面板「配置 → 客户端订阅」里生成的地址可直接导入。

## 为什么是壳，不是原生重写

面板是唯一事实来源。Web 端改版后 App **不用重新打包**——这对 7 天有效期
的免费签名是决定性的：重写原生的方案每改一次 UI 就要重签一次。

壳本身额外补了三件 Web 版做不到的事：

| 能力 | 说明 |
|---|---|
| 原生顶部栏 | 多服务器切换、刷新、设置，不占页面空间 |
| 下拉刷新 | WKWebView 不自带，挂在 scrollView 上 |
| 链接唤起 | 页面里的 `ss://` `sing-box://` 一点就跳对应 App，没装则复制到剪贴板 |

## 首次启动

输入面板地址，例如 `1.2.3.4:8080`。省略 `http://` 会自动补上。
面板不内置 TLS，**默认按 http 连接**；如果前面套了 Nginx + HTTPS，
就完整写 `https://panel.example.com`。地址存在 `UserDefaults`，不上传。

## 已知限制

- **ATS 全放开**：面板地址是裸 IP，无法用域名白名单精确限制。自签自用没问题。
- **后台不保活**：切后台几十秒后 SSE 会被系统掐断，回前台重连，
  中间那段实时曲线会断一截（历史数据仍在库里）。
- **不能上架**：这类自建面板 App 过不了 Guideline 4.2（minimum functionality）。

---

## 免费签名通用说明

需要一台 Mac（黑苹果 / 虚拟机也行）+ Xcode 15 或更新。

1. **改 Bundle ID**（必须）。免费账号也要求唯一，不改会报
   *"The app identifier cannot be registered"*。
2. **Team 选 Personal Team**。普通 Apple ID 登录即可，不用付费。
3. **`⌘R` 装到真机**，首次去 设置 → 通用 → VPN与设备管理 信任。
4. **7 天到期**重连 Mac 再 `⌘R`，数据不丢。嫌麻烦可装
   [SideStore](https://sidestore.io) 自动续签（需要一台常开设备做配对）。

> Windows 绕不开 macOS：`xcodebuild` 出包本身就要 macOS。
> 免费签名这条路没有例外。

## 目录

```
ios/
├── TrafficMeter.xcodeproj/      # 流量统计（直接 open）
├── TrafficMeter/
├── SbxPanel.xcodeproj/          # 面板壳（直接 open）
├── SbxPanel/
└── project.yml                  # XcodeGen 备用（仅 SbxPanel，用于对照重建）
```

手写的 `project.pbxproj` 是唯一事实来源，可直接打开；`project.yml` 只是留个
可读的工程描述，不参与构建。
