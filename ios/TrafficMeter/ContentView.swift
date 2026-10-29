import SwiftUI
import Combine

/// 与面板同一套水墨配色，放在一起看不出是两个东西。
enum Ink {
    static let paper = Color(red: 0.945, green: 0.929, blue: 0.890)      // #F1EDE3
    static let surface = Color(red: 0.980, green: 0.969, blue: 0.937)    // #FAF7EF
    static let ink = Color(red: 0.165, green: 0.145, blue: 0.110)        // #2A251C
    static let ink2 = Color(red: 0.361, green: 0.333, blue: 0.275)
    static let ink3 = Color(red: 0.541, green: 0.514, blue: 0.459)
    static let seal = Color(red: 0.694, green: 0.227, blue: 0.188)       // #B13A30
    static let line = Color(red: 0.165, green: 0.145, blue: 0.110).opacity(0.10)
}

@MainActor
struct ContentView: View {
    @ObservedObject private var store = TrafficStore.shared
    @Environment(\.scenePhase) private var phase
    @State private var showClear = false
    // 用 @State 持有，避免每次重绘都新建一个 Timer 导致采样频率漂移。
    @State private var ticker = Timer.publish(every: 2, on: .main, in: .common).autoconnect()

    var body: some View {
        ZStack {
            Ink.paper.ignoresSafeArea()

            ScrollView(.vertical, showsIndicators: false) {
                VStack(alignment: .leading, spacing: 16) {
                    HeaderBar(proxyActive: store.proxyActive)

                    TodayBlock(today: store.today)

                    HStack(spacing: 10) {
                        StatCard(
                            title: "蜂窝数据",
                            symbol: "antenna.radiowaves.left.and.right",
                            total: store.today.cell,
                            rx: store.today.cellRx,
                            tx: store.today.cellTx,
                            accent: Ink.seal
                        )
                        StatCard(
                            title: "Wi-Fi",
                            symbol: "wifi",
                            total: store.today.wifi,
                            rx: store.today.wifiRx,
                            tx: store.today.wifiTx,
                            accent: Ink.ink
                        )
                    }

                    LiveRow(rx: store.rxRate, tx: store.txRate)

                    WeekChart(days: store.recentDays(7))

                    MonthRow(cell: store.monthCell, wifi: store.monthWifi)

                    Footnotes(trackingSince: store.trackingSince, lastSample: store.lastSample)

                    Button(role: .destructive) {
                        showClear = true
                    } label: {
                        Text("清空统计")
                            .font(.system(size: 13, weight: .medium))
                            .frame(maxWidth: .infinity)
                            .padding(.vertical, 11)
                    }
                    .buttonStyle(.plain)
                    .foregroundStyle(Ink.seal)
                    .background(
                        RoundedRectangle(cornerRadius: 11)
                            .stroke(Ink.seal.opacity(0.35), lineWidth: 1)
                    )
                }
                .padding(.horizontal, 18)
                .padding(.top, 8)
                .padding(.bottom, 28)
            }
        }
        .preferredColorScheme(.light)
        .confirmationDialog("清空后历史用量无法恢复，当前基线会重新建立。", isPresented: $showClear, titleVisibility: .visible) {
            Button("清空", role: .destructive) { store.clearAll() }
            Button("取消", role: .cancel) {}
        }
        .onReceive(ticker) { now in
            // 前台每 2 秒采一次，用于实时速率；进后台时不采（系统会停掉 Timer）。
            guard phase == .active else { return }
            store.sample(now: now)
        }
    }
}

// MARK: - 头部

private struct HeaderBar: View {
    let proxyActive: Bool

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 3) {
                Text("流 量")
                    .font(.system(size: 15, weight: .semibold, design: .serif))
                    .tracking(4)
                    .foregroundStyle(Ink.ink)
                Text("本机网卡口径 · 与代理共存")
                    .font(.system(size: 10.5))
                    .foregroundStyle(Ink.ink3)
            }
            Spacer()
            if proxyActive {
                HStack(spacing: 4) {
                    Circle().fill(Ink.seal).frame(width: 5, height: 5)
                    Text("代理已连接")
                        .font(.system(size: 10.5, weight: .medium))
                        .foregroundStyle(Ink.seal)
                }
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(
                    Capsule().stroke(Ink.seal.opacity(0.3), lineWidth: 1)
                )
            }
        }
    }
}

// MARK: - 今日总览

private struct TodayBlock: View {
    let today: DayUsage

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .firstTextBaseline) {
                Text("今日")
                    .font(.system(size: 11, weight: .semibold))
                    .tracking(2)
                    .foregroundStyle(Ink.ink3)
                Spacer()
                Text("\(ByteFormat.string(today.cell)) 蜂窝 + \(ByteFormat.string(today.wifi)) Wi-Fi")
                    .font(.system(size: 10.5))
                    .foregroundStyle(Ink.ink3)
            }

            HStack(alignment: .firstTextBaseline, spacing: 5) {
                let (value, unit) = ByteFormat.split(today.total)
                Text(value)
                    .font(.system(size: 42, weight: .light, design: .serif))
                    .foregroundStyle(Ink.ink)
                    .monospacedDigit()
                Text(unit)
                    .font(.system(size: 15, weight: .medium))
                    .foregroundStyle(Ink.ink2)
            }

            SplitBar(cell: today.cell, wifi: today.wifi)

            HStack(spacing: 14) {
                LegendDot(color: Ink.seal, text: "蜂窝")
                LegendDot(color: Ink.ink.opacity(0.72), text: "Wi-Fi")
                Spacer()
            }
        }
        .padding(16)
        .background(
            RoundedRectangle(cornerRadius: 15)
                .fill(Ink.surface)
                .overlay(RoundedRectangle(cornerRadius: 15).stroke(Ink.line, lineWidth: 1))
        )
    }
}

private struct LegendDot: View {
    let color: Color
    let text: String

    var body: some View {
        HStack(spacing: 5) {
            RoundedRectangle(cornerRadius: 1.5).fill(color).frame(width: 8, height: 5)
            Text(text).font(.system(size: 10.5)).foregroundStyle(Ink.ink3)
        }
    }
}

private struct SplitBar: View {
    let cell: UInt64
    let wifi: UInt64

    var body: some View {
        GeometryReader { geo in
            let width = geo.size.width
            let total = Double(cell) + Double(wifi)
            let cellWidth: CGFloat = total > 0 ? width * CGFloat(Double(cell) / total) : 0

            ZStack(alignment: .leading) {
                Capsule().fill(Ink.line)
                HStack(spacing: 0) {
                    if cellWidth > 0.5 {
                        Capsule().fill(Ink.seal).frame(width: max(0, cellWidth - 1))
                    }
                    if width - cellWidth > 0.5 {
                        Capsule().fill(Ink.ink.opacity(0.72))
                    }
                }
            }
        }
        .frame(height: 6)
    }
}

// MARK: - 分项卡片

private struct StatCard: View {
    let title: String
    let symbol: String
    let total: UInt64
    let rx: UInt64
    let tx: UInt64
    let accent: Color

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 5) {
                Image(systemName: symbol)
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundStyle(accent)
                Text(title)
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(Ink.ink2)
            }

            HStack(alignment: .firstTextBaseline, spacing: 3) {
                let (value, unit) = ByteFormat.split(total)
                Text(value)
                    .font(.system(size: 21, weight: .regular, design: .serif))
                    .foregroundStyle(Ink.ink)
                    .monospacedDigit()
                Text(unit)
                    .font(.system(size: 10.5, weight: .medium))
                    .foregroundStyle(Ink.ink3)
            }

            HStack(spacing: 8) {
                Text("↓ \(ByteFormat.string(rx))")
                Text("↑ \(ByteFormat.string(tx))")
            }
            .font(.system(size: 10))
            .foregroundStyle(Ink.ink3)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(13)
        .background(
            RoundedRectangle(cornerRadius: 13)
                .fill(Ink.surface)
                .overlay(RoundedRectangle(cornerRadius: 13).stroke(Ink.line, lineWidth: 1))
        )
    }
}

// MARK: - 实时速率

private struct LiveRow: View {
    let rx: Double
    let tx: Double

    var body: some View {
        HStack(spacing: 10) {
            Text("此刻")
                .font(.system(size: 10.5, weight: .medium))
                .foregroundStyle(Ink.ink3)
            Spacer()
            Text("↓ " + ByteFormat.rate(rx))
                .font(.system(size: 12, weight: .medium, design: .monospaced))
                .foregroundStyle(Ink.ink2)
            Text("↑ " + ByteFormat.rate(tx))
                .font(.system(size: 12, weight: .medium, design: .monospaced))
                .foregroundStyle(Ink.ink2)
        }
        .padding(.horizontal, 13)
        .padding(.vertical, 9)
        .background(
            RoundedRectangle(cornerRadius: 10).fill(Ink.ink.opacity(0.035))
        )
    }
}

// MARK: - 七日图

private struct WeekChart: View {
    let days: [DayUsage]

    var body: some View {
        let peak = max(days.map { $0.total }.max() ?? 0, 1)
        let todayKey = days.last?.day ?? ""

        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("近七日")
                    .font(.system(size: 11, weight: .semibold))
                    .tracking(2)
                    .foregroundStyle(Ink.ink3)
                Spacer()
                Text("峰值 \(ByteFormat.string(peak))")
                    .font(.system(size: 10))
                    .foregroundStyle(Ink.ink3)
            }

            HStack(alignment: .bottom, spacing: 4) {
                ForEach(days) { day in
                    DayBar(day: day, peak: peak, isToday: day.day == todayKey)
                }
            }
            .frame(height: 104, alignment: .bottom)
        }
        .padding(14)
        .background(
            RoundedRectangle(cornerRadius: 15)
                .fill(Ink.surface)
                .overlay(RoundedRectangle(cornerRadius: 15).stroke(Ink.line, lineWidth: 1))
        )
    }
}

private struct DayBar: View {
    let day: DayUsage
    let peak: UInt64
    let isToday: Bool

    var body: some View {
        let height = day.total == 0 ? 3.0 : max(6.0, 84 * Double(day.total) / Double(peak))
        let cellHeight = day.total == 0 ? 0.0 : height * Double(day.cell) / Double(day.total)
        let wifiHeight = max(0, height - cellHeight)

        VStack(spacing: 6) {
            VStack(spacing: 1) {
                Spacer(minLength: 0)
                if cellHeight > 0.5 {
                    UnevenRoundedRectangle(topLeadingRadius: 2.5, bottomLeadingRadius: 0, bottomTrailingRadius: 0, topTrailingRadius: 2.5)
                        .fill(Ink.seal.opacity(0.9))
                        .frame(width: 14, height: CGFloat(cellHeight))
                }
                if wifiHeight > 0.5 {
                    UnevenRoundedRectangle(topLeadingRadius: 0, bottomLeadingRadius: 2.5, bottomTrailingRadius: 2.5, topTrailingRadius: 0)
                        .fill(Ink.ink.opacity(0.72))
                        .frame(width: 14, height: CGFloat(wifiHeight))
                }
            }
            .frame(height: 84, alignment: .bottom)
            .frame(maxWidth: .infinity)

            Text(String(day.day.suffix(2)))
                .font(.system(size: 9.5, weight: isToday ? .bold : .regular))
                .foregroundStyle(isToday ? Ink.seal : Ink.ink3)
        }
        .frame(maxWidth: .infinity)
    }
}

// MARK: - 月合计 / 说明

private struct MonthRow: View {
    let cell: UInt64
    let wifi: UInt64

    var body: some View {
        HStack {
            Text("本月")
                .font(.system(size: 11, weight: .semibold))
                .tracking(2)
                .foregroundStyle(Ink.ink3)
            Spacer()
            Text("蜂窝 \(ByteFormat.string(cell))")
                .font(.system(size: 11.5))
                .foregroundStyle(Ink.seal)
            Text("·").foregroundStyle(Ink.ink3)
            Text("Wi-Fi \(ByteFormat.string(wifi))")
                .font(.system(size: 11.5))
                .foregroundStyle(Ink.ink2)
        }
        .padding(.horizontal, 4)
    }
}

private struct Footnotes: View {
    let trackingSince: Date?
    let lastSample: Date

    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "MM-dd HH:mm"
        return f
    }()

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            Text("统计口径")
                .font(.system(size: 10.5, weight: .semibold))
                .foregroundStyle(Ink.ink2)

            Text("读的是网卡累计字节（Wi-Fi 走 en0，蜂窝走 pdp_ip0–3），不区分 App——按 App 拆分的那份数据在系统里，没有公开接口，任何第三方 App 都读不到。")
            Text("开着代理同样准确：隧道（utun）的流量最终仍经过物理网卡，这里只读物理层，不会重复计数。与代理软件自身面板差 3–8% 是正常的，它们多半只报载荷，这里含 TCP/IP 与 TLS 头。")
            Text("后台不常驻，系统调度不可控，所以「哪天用了多少」可能有小时级偏差；总量不受影响，因为计数是内核累计的。")
            if let since = trackingSince {
                Text("统计起点 \(Self.timeFormatter.string(from: since))，此前用量无法回溯。最近采样 \(Self.timeFormatter.string(from: lastSample))。")
                    .foregroundStyle(Ink.ink3)
            }
        }
        .font(.system(size: 10.5))
        .lineSpacing(2.5)
        .foregroundStyle(Ink.ink3)
        .padding(.leading, 10)
        .overlay(alignment: .leading) {
            Rectangle()
                .fill(Ink.line)
                .frame(width: 2)
        }
    }
}
