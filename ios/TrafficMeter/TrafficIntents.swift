import AppIntents

/// 暴露给「快捷指令」的动作：读取今日流量。
///
/// 纯快捷指令做不到这件事——系统没有提供任何能读取网卡字节的动作，
/// 只有 App 内通过 getifaddrs 才能拿到。所以这里做成 App Intent，
/// 快捷指令里搜「今日流量」即可添加，也能挂到个人自动化上定时播报。
struct TodayTrafficIntent: AppIntent {
    static var title: LocalizedStringResource = "获取今日流量"
    static var description = IntentDescription("读取今日已用的蜂窝数据与 Wi-Fi 流量，区分上下行")
    static var openAppWhenRun: Bool = false

    func perform() async throws -> some IntentResult & ProvidesDialog {
        let text = await TrafficStore.shared.summaryText()
        return .result(dialog: IntentDialog(stringLiteral: text))
    }
}

/// 无需配置即可在快捷指令 / Siri 里直接说出来的短语。
struct TrafficShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(
            intent: TodayTrafficIntent(),
            phrases: [
                "用 \(.applicationName) 查今天用了多少流量",
                "\(.applicationName) 今日流量"
            ],
            shortTitle: "今日流量",
            systemImageName: "chart.bar.fill"
        )
    }
}
