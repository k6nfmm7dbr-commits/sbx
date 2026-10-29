import SwiftUI

@main
struct TrafficMeterApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @Environment(\.scenePhase) private var phase

    var body: some Scene {
        WindowGroup {
            ContentView()
                .onChange(of: phase) { _, newPhase in
                    switch newPhase {
                    case .active:
                        Task { @MainActor in TrafficStore.shared.sample() }
                    case .background:
                        // 进后台时预约一次刷新。系统不保证执行，但执行一次就赚一次。
                        AppDelegate.scheduleRefresh()
                    default:
                        break
                    }
                }
        }
    }
}
