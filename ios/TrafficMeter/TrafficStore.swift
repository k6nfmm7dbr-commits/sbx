import Foundation
import Combine

/// 单日用量。
struct DayUsage: Codable, Equatable, Identifiable {
    var day: String          // yyyy-MM-dd（本地时区）
    var cellRx: UInt64 = 0
    var cellTx: UInt64 = 0
    var wifiRx: UInt64 = 0
    var wifiTx: UInt64 = 0

    var id: String { day }
    var cell: UInt64 { cellRx &+ cellTx }
    var wifi: UInt64 { wifiRx &+ wifiTx }
    var total: UInt64 { cell &+ wifi }
}

/// 落盘结构。
struct PersistedState: Codable {
    var baseline = InterfaceSnapshot.zero
    var baselineAt: TimeInterval = 0
    var bootTime: TimeInterval = 0
    var days: [String: DayUsage] = [:]
}

@MainActor
final class TrafficStore: ObservableObject {
    static let shared = TrafficStore()

    /// 倒序（最新在前）。
    @Published private(set) var days: [DayUsage] = []
    @Published private(set) var live = InterfaceSnapshot.zero
    @Published private(set) var rxRate: Double = 0
    @Published private(set) var txRate: Double = 0
    @Published private(set) var proxyActive = false
    @Published private(set) var lastSample = Date.distantPast
    /// 首次运行时间：早于此时刻的用量无法统计，UI 要如实说明。
    @Published private(set) var trackingSince: Date?

    private var state = PersistedState()
    private var lastLive = InterfaceSnapshot.zero
    private var lastLiveAt = Date.distantPast
    private let cal = Calendar.current
    private let fileURL: URL

    private static let keyFormatter: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd"
        return f
    }()

    private init() {
        let fm = FileManager.default
        let base = fm.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        let dir = base.appendingPathComponent("TrafficMeter", isDirectory: true)
        try? fm.createDirectory(at: dir, withIntermediateDirectories: true)
        fileURL = dir.appendingPathComponent("state.json")
        load()
    }

    // MARK: - 采样

    /// 采样一次。前台每 2 秒调用，后台由 BGTask 尽力调用。
    ///
    /// 关键性质：接口计数是内核累计的，所以**无论调用多稀疏，总量都准确**；
    /// 稀疏只影响「哪天用的」这个时间维度。这也是为什么后台刷新不被保证
    /// 也不会让数据出错。
    func sample(now: Date = Date()) {
        let cur = InterfaceStats.read()
        proxyActive = InterfaceStats.isProxyActive()
        live = cur

        // 瞬时速率：只有两次采样间隔足够短才有意义（后台回来时 dt 可能是几小时，
        // 那时算出来的"速率"毫无意义，直接不更新）。
        if lastLiveAt != .distantPast {
            let dt = now.timeIntervalSince(lastLiveAt)
            if dt >= 0.5 && dt <= 15 {
                rxRate = Double(diff(cur.rxTotal, lastLive.rxTotal)) / dt
                txRate = Double(diff(cur.txTotal, lastLive.txTotal)) / dt
            }
        }
        if now.timeIntervalSince(lastLiveAt) >= 0.5 {
            lastLive = cur
            lastLiveAt = now
        }

        let boot = InterfaceStats.bootTime()
        guard state.baselineAt > 0, state.bootTime > 0 else {
            // 首次运行：只建立基线，不产生任何用量。
            state.baseline = cur
            state.baselineAt = now.timeIntervalSince1970
            state.bootTime = boot.timeIntervalSince1970
            trackingSince = now
            lastSample = now
            save()
            return
        }

        let storedBoot = Date(timeIntervalSince1970: state.bootTime)
        let start = Date(timeIntervalSince1970: state.baselineAt)
        let rebooted = abs(boot.timeIntervalSince(storedBoot)) > 5

        var delta = InterfaceSnapshot.zero
        var from = start
        if rebooted {
            // 重启后计数从 0 起算，当前读数就是「自开机以来」的全部用量，
            // 不能和重启前的基线相减。
            delta = cur
            state.bootTime = boot.timeIntervalSince1970
            from = max(start, boot)
        } else {
            delta = InterfaceSnapshot(
                wifiRx: diff(cur.wifiRx, state.baseline.wifiRx),
                wifiTx: diff(cur.wifiTx, state.baseline.wifiTx),
                cellRx: diff(cur.cellRx, state.baseline.cellRx),
                cellTx: diff(cur.cellTx, state.baseline.cellTx)
            )
        }

        if delta != .zero {
            accumulate(delta, from: from, to: now)
        }

        state.baseline = cur
        state.baselineAt = now.timeIntervalSince1970
        lastSample = now
        prune()
        rebuildDays()
        save()
    }

    /// 计数只会增长；变小说明接口被销毁重建，这段无法归因，宁可少记也不重复记。
    private func diff(_ current: UInt64, _ base: UInt64) -> UInt64 {
        current >= base ? current - base : 0
    }

    /// 把一段增量按时间比例摊到各天。
    ///
    /// 为什么必须摊：App 可能两天没打开，一次采样得到 3 GB，如果全记在
    /// 「打开那天」，日报就是错的。按秒切分是最省事且误差可控的做法。
    private func accumulate(_ delta: InterfaceSnapshot, from start: Date, to end: Date) {
        let span = end.timeIntervalSince(start)
        guard span > 1 else {
            add(delta, to: key(end))
            return
        }

        var cursor = start
        var elapsed: TimeInterval = 0
        var previous = InterfaceSnapshot.zero
        var guardCount = 0

        while cursor < end && guardCount < 800 {
            guardCount += 1
            let nextMidnight = cal.startOfDay(for: cursor.addingTimeInterval(86_400))
            let sliceEnd = min(nextMidnight, end)
            elapsed += sliceEnd.timeIntervalSince(cursor)

            // 用累积值做差，避免逐段四舍五入导致总量漂移。
            let cumulative = scaled(delta, min(1, elapsed / span))
            let part = InterfaceSnapshot(
                wifiRx: cumulative.wifiRx &- previous.wifiRx,
                wifiTx: cumulative.wifiTx &- previous.wifiTx,
                cellRx: cumulative.cellRx &- previous.cellRx,
                cellTx: cumulative.cellTx &- previous.cellTx
            )
            add(part, to: key(cursor))
            previous = cumulative
            cursor = sliceEnd
        }

        // 跨天过多（>800 天，实际上不会发生）时把余量一次性记到最后一天，
        // 宁可粗一点也不要凭空丢流量。
        if cursor < end && delta != previous {
            let rest = InterfaceSnapshot(
                wifiRx: delta.wifiRx &- previous.wifiRx,
                wifiTx: delta.wifiTx &- previous.wifiTx,
                cellRx: delta.cellRx &- previous.cellRx,
                cellTx: delta.cellTx &- previous.cellTx
            )
            add(rest, to: key(end))
        }
    }

    private func scaled(_ s: InterfaceSnapshot, _ factor: Double) -> InterfaceSnapshot {
        func part(_ v: UInt64) -> UInt64 { UInt64((Double(v) * factor).rounded()) }
        return InterfaceSnapshot(
            wifiRx: part(s.wifiRx), wifiTx: part(s.wifiTx),
            cellRx: part(s.cellRx), cellTx: part(s.cellTx)
        )
    }

    private func add(_ s: InterfaceSnapshot, to dayKey: String) {
        guard s != .zero else { return }
        var d = state.days[dayKey] ?? DayUsage(day: dayKey)
        d.cellRx &+= s.cellRx
        d.cellTx &+= s.cellTx
        d.wifiRx &+= s.wifiRx
        d.wifiTx &+= s.wifiTx
        state.days[dayKey] = d
    }

    private func key(_ date: Date) -> String {
        Self.keyFormatter.string(from: date)
    }

    // MARK: - 查询

    var today: DayUsage {
        state.days[key(Date())] ?? DayUsage(day: key(Date()))
    }

    var monthCell: UInt64 {
        let prefix = String(key(Date()).prefix(7))
        return state.days.values.filter { $0.day.hasPrefix(prefix) }.reduce(0) { $0 &+ $1.cell }
    }

    var monthWifi: UInt64 {
        let prefix = String(key(Date()).prefix(7))
        return state.days.values.filter { $0.day.hasPrefix(prefix) }.reduce(0) { $0 &+ $1.wifi }
    }

    /// 最近 n 天，从旧到新，缺失的补 0（图表需要连续）。
    func recentDays(_ n: Int) -> [DayUsage] {
        let today0 = cal.startOfDay(for: Date())
        var out: [DayUsage] = []
        out.reserveCapacity(n)
        for i in stride(from: n - 1, through: 0, by: -1) {
            let d = cal.date(byAdding: .day, value: -i, to: today0) ?? today0
            let k = key(d)
            out.append(state.days[k] ?? DayUsage(day: k))
        }
        return out
    }

    /// 给快捷指令 / Siri 用的一句话。
    func summaryText() -> String {
        sample()
        let t = today
        var text = "今日合计 \(ByteFormat.string(t.total))。"
        text += "蜂窝 \(ByteFormat.string(t.cell))（下行 \(ByteFormat.string(t.cellRx))，上行 \(ByteFormat.string(t.cellTx))）；"
        text += "Wi-Fi \(ByteFormat.string(t.wifi))（下行 \(ByteFormat.string(t.wifiRx))，上行 \(ByteFormat.string(t.wifiTx))）。"
        text += "本月蜂窝 \(ByteFormat.string(monthCell))，Wi-Fi \(ByteFormat.string(monthWifi))。"
        return text
    }

    // MARK: - 维护

    /// 清空全部历史并重建基线（用户主动操作）。
    func clearAll() {
        state = PersistedState()
        state.baseline = InterfaceStats.read()
        state.baselineAt = Date().timeIntervalSince1970
        state.bootTime = InterfaceStats.bootTime().timeIntervalSince1970
        trackingSince = Date()
        days = []
        save()
    }

    /// 保留 400 天，防止 JSON 无限膨胀。
    private func prune() {
        let cutoff = cal.date(byAdding: .day, value: -400, to: Date()) ?? Date()
        let cutoffKey = key(cutoff)
        state.days = state.days.filter { $0.key >= cutoffKey }
    }

    private func rebuildDays() {
        days = state.days.values.sorted { $0.day > $1.day }
    }

    private func load() {
        guard let data = try? Data(contentsOf: fileURL),
              let decoded = try? JSONDecoder().decode(PersistedState.self, from: data) else {
            return
        }
        state = decoded
        if state.baselineAt > 0 {
            trackingSince = Date(timeIntervalSince1970: state.baselineAt)
        }
        rebuildDays()
    }

    private func save() {
        guard let data = try? JSONEncoder().encode(state) else { return }
        // 原子写：后台任务被系统随时杀掉时不会留下半截 JSON。
        try? data.write(to: fileURL, options: .atomic)
    }
}
