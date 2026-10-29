import SwiftUI
import UIKit
import WebKit

/// 面板 WebView 容器。
///
/// 设计取舍：宁可让 App 只做「容器」，也不在 App 内重写面板 UI。
/// 面板是唯一事实来源，Web 端改版后 App 不需要重新发版——这对
/// 免费签名（7 天重签一次）的分发方式是决定性优势。
struct PanelWebView: UIViewRepresentable {
    let endpoint: PanelEndpoint
    let reloadToken: Int

    @Binding var progress: Double
    @Binding var isLoading: Bool
    @Binding var loadError: String?
    @Binding var toast: String?

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        // 默认数据仓库：Cookie 落在磁盘上，面板 7 天的登录态能跨启动保留。
        config.websiteDataStore = .default()
        config.allowsInlineMediaPlayback = true

        let wv = WKWebView(frame: .zero, configuration: config)
        wv.navigationDelegate = context.coordinator
        wv.uiDelegate = context.coordinator
        wv.allowsBackForwardNavigationGestures = true
        wv.scrollView.bounces = true
        // 交给页面自己处理安全区（style.css 的 dock 用了 env(safe-area-inset-*)）。
        wv.scrollView.contentInsetAdjustmentBehavior = .never
        wv.isOpaque = false
        // 与 --bg 一致：页面首帧渲染前的底色不该是刺眼的白。
        wv.backgroundColor = UIColor(red: 0.945, green: 0.929, blue: 0.890, alpha: 1)

        // 下拉刷新：WKWebView 不自带，必须挂在它的 scrollView 上。
        // 面板有 SSE 实时数据，但用户手动下拉仍是最直觉的"重新拉一次"。
        let rc = UIRefreshControl()
        rc.tintColor = UIColor(red: 0.208, green: 0.188, blue: 0.165, alpha: 1)
        rc.addTarget(context.coordinator, action: #selector(Coordinator.handleRefresh(_:)), for: .valueChanged)
        wv.scrollView.refreshControl = rc

        context.coordinator.attach(wv)
        context.coordinator.loadCurrent()
        return wv
    }

    func updateUIView(_ wv: WKWebView, context: Context) {
        context.coordinator.parent = self
        context.coordinator.syncIfNeeded()
    }

    // MARK: - Coordinator

    final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate {
        var parent: PanelWebView
        private weak var webView: WKWebView?
        private var progressObservation: NSKeyValueObservation?

        private var lastReloadToken: Int
        private var loadedAddress: String

        init(_ parent: PanelWebView) {
            self.parent = parent
            self.lastReloadToken = parent.reloadToken
            self.loadedAddress = ""
        }

        func attach(_ wv: WKWebView) {
            webView = wv
            progressObservation = wv.observe(\.estimatedProgress, options: [.new]) { [weak self] wv, _ in
                let p = wv.estimatedProgress
                DispatchQueue.main.async {
                    guard let self else { return }
                    if abs(self.parent.progress - p) > 0.01 { self.parent.progress = p }
                }
            }
        }

        func loadCurrent() {
            guard let wv = webView, let url = parent.endpoint.url else { return }
            loadedAddress = parent.endpoint.address
            var req = URLRequest(url: url)
            req.timeoutInterval = 20
            req.cachePolicy = .useProtocolCachePolicy
            wv.load(req)
        }

        /// 只在「用户点了刷新」或「切了服务器」时才动 WebView。
        /// 每次 updateUIView 都无条件 reload 会把用户正在填的表单冲掉。
        func syncIfNeeded() {
            if lastReloadToken != parent.reloadToken {
                lastReloadToken = parent.reloadToken
                reload()
                return
            }
            if loadedAddress != parent.endpoint.address {
                loadCurrent()
            }
        }

        private func reload() {
            guard let wv = webView else { return }
            if wv.url != nil {
                // 从源站重取，绕过缓存——用户点刷新就是想要最新的一份。
                wv.reloadFromOrigin()
            } else {
                loadCurrent()
            }
        }

        @objc func handleRefresh(_ sender: UIRefreshControl) {
            guard let wv = webView else { sender.endRefreshing(); return }
            if wv.url == nil {
                loadCurrent()
            } else {
                wv.reload()
            }
            // 刻意不在这里 endRefreshing：转圈要转到 didFinish/didFail，
            // 否则慢网下用户会看到"下拉一下就弹回去、页面还是旧的"。
        }

        // MARK: 导航

        func webView(_ webView: WKWebView, didStartProvisionalNavigation navigation: WKNavigation!) {
            parent.isLoading = true
            if parent.loadError != nil { parent.loadError = nil }
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            parent.isLoading = false
            parent.progress = 1
            webView.scrollView.refreshControl?.endRefreshing()
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            finishWith(error)
        }

        func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
            finishWith(error)
        }

        private func finishWith(_ error: Error) {
            parent.isLoading = false
            webView?.scrollView.refreshControl?.endRefreshing()
            let ns = error as NSError
            // -999 是被我们主动 cancel（例如点了节点链接要跳去 sing-box），
            // 报成"加载失败"会让用户以为面板挂了。
            if ns.code == NSURLErrorCancelled { return }
            parent.loadError = Self.describe(ns)
        }

        static func describe(_ e: NSError) -> String {
            switch e.code {
            case NSURLErrorTimedOut:
                return "连接超时。面板服务没响应，检查服务器是否在跑、端口是否放行。"
            case NSURLErrorCannotFindHost, NSURLErrorDNSLookupFailed:
                return "找不到这个地址。域名写错了，或者 DNS 解析不出来。"
            case NSURLErrorCannotConnectToHost:
                return "端口连不上。服务没启动，或者防火墙拦了这个端口。"
            case NSURLErrorNotConnectedToInternet:
                return "手机当前没有网络。"
            case NSURLErrorSecureConnectionFailed, NSURLErrorServerCertificateUntrusted:
                return "HTTPS 证书不被信任。面板用自签证书时请改用 http:// 访问。"
            case NSURLErrorAppTransportSecurityRequiresSecureConnection:
                return "系统拦截了明文 HTTP 请求（ATS）。本工程已配置放行，若仍出现请检查 Info.plist。"
            default:
                return "加载失败：\(e.localizedDescription)（\(e.code)）"
            }
        }

        /// 面板里的节点链接（ss:// vmess:// trojan:// socks5:// snell://）
        /// 以及 sing-box:// 这类自定义 scheme，一律交给系统处理——
        /// 这样在手机上点一下就能直接唤起已安装的客户端。
        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            guard let url = navigationAction.request.url, let scheme = url.scheme?.lowercased() else {
                decisionHandler(.allow)
                return
            }
            let allowed: Set<String> = ["http", "https", "about", "data", "blob", "file"]
            if allowed.contains(scheme) {
                decisionHandler(.allow)
                return
            }

            decisionHandler(.cancel)
            UIApplication.shared.open(url, options: [:]) { ok in
                guard !ok else { return }
                DispatchQueue.main.async {
                    // 没装对应客户端时给个明确说法，而不是"点了没反应"。
                    UIPasteboard.general.string = url.absoluteString
                    self.parent.toast = "没有 App 能打开 \(scheme):// 链接，已复制到剪贴板"
                }
            }
        }

        // MARK: JS 对话框
        //
        // WKWebView 默认不实现这些方法时，页面里的 alert()/confirm() 会被
        // 静默吞掉，表现为"点了按钮没反应"。面板前端已改用页内弹窗，
        // 这里仍补齐实现，避免将来某处用到原生对话框时卡死。

        func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                     initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
            let alert = UIAlertController(title: nil, message: message, preferredStyle: .alert)
            alert.addAction(UIAlertAction(title: "好", style: .default) { _ in completionHandler() })
            present(alert)
        }

        func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                     initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
            let alert = UIAlertController(title: nil, message: message, preferredStyle: .alert)
            alert.addAction(UIAlertAction(title: "取消", style: .cancel) { _ in completionHandler(false) })
            alert.addAction(UIAlertAction(title: "确定", style: .default) { _ in completionHandler(true) })
            present(alert)
        }

        func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String,
                     defaultText: String?, initiatedByFrame frame: WKFrameInfo,
                     completionHandler: @escaping (String?) -> Void) {
            let alert = UIAlertController(title: nil, message: prompt, preferredStyle: .alert)
            alert.addTextField { $0.text = defaultText }
            alert.addAction(UIAlertAction(title: "取消", style: .cancel) { _ in completionHandler(nil) })
            alert.addAction(UIAlertAction(title: "确定", style: .default) { _ in
                completionHandler(alert.textFields?.first?.text)
            })
            present(alert)
        }

        private func present(_ alert: UIAlertController) {
            guard let scene = UIApplication.shared.connectedScenes
                .compactMap({ $0 as? UIWindowScene })
                .first(where: { $0.activationState == .foregroundActive }),
                  let root = scene.keyWindow?.rootViewController else { return }
            var top = root
            while let presented = top.presentedViewController { top = presented }
            top.present(alert, animated: true)
        }
    }
}
