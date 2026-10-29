# SBX iOS 客户端

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

## 用免费 Apple ID 装到手机（7 天有效）

需要一台 Mac（黑苹果/虚拟机也行）+ Xcode 15 或更新。

1. **改 Bundle ID**（必须）
   打开 `SbxPanel.xcodeproj` → 选中 `SbxPanel` target → *Signing & Capabilities*
   把 `com.example.sbxpanel` 改成任何**全局唯一**的字符串，例如
   `com.你的名字.sbx`。免费账号也要求唯一，不改会报
   *"The app identifier cannot be registered"*。

2. **登录 Apple ID**
   同一页 *Team* 下拉 → *Add an Account…* → 用普通 Apple ID 登录（不用付费）。
   登录后回到 Team 选 *Personal Team*。

3. **连手机跑**
   iPhone 用数据线连 Mac → Xcode 顶部设备选择里选中它 → `⌘R`。
   首次会提示 *"Untrusted Developer"*：
   iPhone 上 设置 → 通用 → VPN与设备管理 → 点你的 Apple ID → 信任。

4. **7 天后失效**
   免费签名的描述文件只有 7 天。到期后 App 打不开（闪退），
   重连 Mac 再 `⌘R` 一次即可，数据不会丢（UserDefaults 与 WebView Cookie 都在）。

   嫌麻烦可以装 [SideStore](https://sidestore.io)，让手机自己续签，
   代价是需要一台常开的设备做配对服务。

> 用 Windows 的话，先 `xcodebuild` 出未签名 ipa 做不到（需要 macOS），
> 只能走 Sideloadly 的"重签名"路径，仍然需要一份由 Mac/Xcode 产出的 ipa。
> 换句话说：**免费签名绕不开 macOS**。

## 首次启动

输入面板地址，例如：

```
1.2.3.4:8080
```

省略 `http://` 会自动补上。面板不内置 TLS，**默认按 http 连接**；
如果你在前面套了 Nginx + HTTPS，就完整写 `https://panel.example.com`。

地址存在 `UserDefaults` 里，不会上传到任何地方。

## 已知限制

- **ATS 全放开**：`Info.plist` 里 `NSAllowsArbitraryLoads = true`。面板地址是
  裸 IP，无法用域名白名单精确限制。自签自用没问题，上架会被审核问到。
- **后台不保活**：切到后台超过几十秒，SSE 长连接会被系统掐断。回到前台时
  WebView 会重连，但中间那段的实时曲线会断一截（历史数据仍在库里）。
- **不支持 iPad 横屏**：`TARGETED_DEVICE_FAMILY = 1`，只出 iPhone 版。
- **不能上架**：面板是网络代理管理工具，App Store 中国区需相关资质；
  这类自建面板 App 也基本过不了 Guideline 4.2（minimum functionality）。

## 目录

```
ios/
├── SbxPanel.xcodeproj/          # 直接 open 即可
├── SbxPanel/
│   ├── SbxPanelApp.swift        # @main
│   ├── RootView.swift           # 主界面 / 引导页 / 设置 / 错误页
│   ├── PanelWebView.swift       # WKWebView 封装（下拉刷新、scheme 拦截、JS 对话框）
│   ├── PanelStore.swift         # 多服务器地址的持久化
│   ├── Info.plist
│   └── Assets.xcassets/         # 图标与配色（与面板水墨主题同源）
└── project.yml                  # XcodeGen 备用：xcodegen generate 可重建工程
```

## 想改成原生 UI？

不建议。真要做，先看两项成本：

1. 面板有约 20 个 API 端点，其中 `/api/events` 是 SSE 长连接，
   Swift 侧要自己实现重连与版本对齐（服务端已有版本号退避逻辑）。
2. 每加一个面板功能都要写两遍 Swift + JS。

先做 PWA（Safari → 分享 → 添加到主屏幕）能覆盖 90% 的"想要一个 App 图标"
的需求，零签名、零续签。
