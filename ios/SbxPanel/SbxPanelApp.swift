import SwiftUI

@main
struct SbxPanelApp: App {
    @StateObject private var store = PanelStore()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(store)
                // 面板是宣纸浅色主题，强制浅色可避免深色模式下状态栏与
                // 页面顶部撞色（面板本身没有 dark 主题）。
                .preferredColorScheme(.light)
        }
    }
}
