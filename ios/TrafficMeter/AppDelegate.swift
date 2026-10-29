import UIKit
import BackgroundTasks

final class AppDelegate: NSObject, UIApplicationDelegate {
    /// 必须与 Info.plist 的 BGTaskSchedulerPermittedIdentifiers 完全一致，
    /// 且以 Bundle ID 为前缀，否则 submit 会抛 BGTaskSchedulerErrorCodeUnavailable。
    static let refreshIdentifier = "com.example.trafficmeter.refresh"

    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        BGTaskScheduler.shared.register(
            forTaskWithIdentifier: Self.refreshIdentifier,
            using: nil
        ) { task in
            guard let refresh = task as? BGAppRefreshTask else {
                task.setTaskCompleted(success: false)
                return
            }
            Self.handle(refresh)
        }

        Task { @MainActor in TrafficStore.shared.sample() }
        Self.scheduleRefresh()
        return true
    }

    func applicationDidEnterBackground(_ application: UIApplication) {
        Self.scheduleRefresh()
    }

    static func scheduleRefresh() {
        let request = BGAppRefreshTaskRequest(identifier: refreshIdentifier)
        // 15 分钟是系统允许的下限；实际执行时机由系统按使用习惯决定，
        // 可能几小时才跑一次。这不影响总量准确性，见 TrafficStore.sample 的说明。
        request.earliestBeginDate = Date(timeIntervalSinceNow: 15 * 60)
        try? BGTaskScheduler.shared.submit(request)
    }

    private static func handle(_ task: BGAppRefreshTask) {
        // 先预约下一次，避免这次失败后彻底断掉。
        scheduleRefresh()

        let work = Task { @MainActor in
            TrafficStore.shared.sample()
            task.setTaskCompleted(success: true)
        }

        task.expirationHandler = {
            work.cancel()
            task.setTaskCompleted(success: false)
        }
    }
}
