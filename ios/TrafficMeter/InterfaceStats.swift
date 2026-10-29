import Foundation
import Darwin
import CFNetwork

/// 网卡累计计数快照，单位字节。
///
/// 这是**内核累计值**，不是增量：设备开机后一直累加，重启才归零。
/// 因此我们的统计方式是「存基线 + 做差」，App 不在运行时流量照样在涨，
/// 下次启动一次性补上，总量不会丢。
struct InterfaceSnapshot: Codable, Equatable {
    var wifiRx: UInt64 = 0
    var wifiTx: UInt64 = 0
    var cellRx: UInt64 = 0
    var cellTx: UInt64 = 0

    static let zero = InterfaceSnapshot()

    var wifiTotal: UInt64 { wifiRx &+ wifiTx }
    var cellTotal: UInt64 { cellRx &+ cellTx }
    var total: UInt64 { wifiTotal &+ cellTotal }
    var rxTotal: UInt64 { wifiRx &+ cellRx }
    var txTotal: UInt64 { wifiTx &+ cellTx }
}

enum InterfaceStats {

    /// 只累加物理接口的计数。
    ///
    /// - `en0`：iPhone 上的 Wi-Fi 主接口，稳定不变。
    /// - `pdp_ip0…3`：蜂窝数据上下文（主数据、IMS、MMS 等），全部累加才等于
    ///   运营商侧看到的用量。
    ///
    /// **刻意跳过 `utun*`**：代理软件（sing-box / Shadowrocket / Surge / Loon）
    /// 建立的隧道接口，其字节同时也会记在底层物理接口上。把 utun 算进来
    /// 就是双倍计数——这是同类工具最常见的错。
    /// 也跳过 `lo0`（本机回环）、`awdl0` / `llw0`（AirDrop 与 AWDL，不算上网流量）。
    static func read() -> InterfaceSnapshot {
        var snapshot = InterfaceSnapshot()
        var head: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&head) == 0, let first = head else { return snapshot }
        defer { freeifaddrs(head) }

        var node: UnsafeMutablePointer<ifaddrs>? = first
        while let p = node {
            node = p.pointee.ifa_next

            guard let addr = p.pointee.ifa_addr,
                  addr.pointee.sa_family == UInt8(AF_LINK),
                  let raw = p.pointee.ifa_data else { continue }

            let data = raw.assumingMemoryBound(to: if_data.self).pointee
            let name = String(cString: p.pointee.ifa_name)
            let rx = UInt64(data.ifi_ibytes)
            let tx = UInt64(data.ifi_obytes)

            if name == "en0" {
                // 同一接口可能挂多个地址（IPv4/IPv6），计数相同，覆盖即可。
                snapshot.wifiRx = rx
                snapshot.wifiTx = tx
            } else if name.hasPrefix("pdp_ip") {
                snapshot.cellRx &+= rx
                snapshot.cellTx &+= tx
            }
        }
        return snapshot
    }

    /// 系统当前是否挂着一个 VPN / 代理隧道。
    ///
    /// 用 `__SCOPED__` 而不是「有没有 utun 接口」：iOS 即使没开代理也常驻
    /// utun0/utun1（系统内部用），拿它判断会永远返回 true。
    static func isProxyActive() -> Bool {
        guard let unmanaged = CFNetworkCopySystemProxySettings() else { return false }
        let settings = unmanaged.takeRetainedValue() as NSDictionary
        guard let scoped = settings["__SCOPED__"] as? [String: Any] else { return false }
        for key in scoped.keys {
            if key.hasPrefix("utun") || key.hasPrefix("ipsec") || key.hasPrefix("tap")
                || key.hasPrefix("tun") || key.hasPrefix("ppp") {
                return true
            }
        }
        return false
    }

    /// 开机时刻。用来判断「计数归零是因为重启，还是因为接口重建」——
    /// 这两种情况的处理方式完全相反，靠「计数变小」是猜不准的。
    static func bootTime() -> Date {
        var tv = timeval()
        var size = MemoryLayout<timeval>.size
        if sysctlbyname("kern.boottime", &tv, &size, nil, 0) == 0, tv.tv_sec > 0 {
            return Date(timeIntervalSince1970: TimeInterval(tv.tv_sec) + TimeInterval(tv.tv_usec) / 1_000_000)
        }
        return Date().addingTimeInterval(-ProcessInfo.processInfo.systemUptime)
    }
}

/// 字节格式化。
///
/// 用 1000 进制，和 iOS「设置 → 蜂窝网络」的口径一致；换成 1024 进制会与
/// 系统数字差 7%，用户第一眼就会觉得「对不上」。
enum ByteFormat {
    static let units = ["B", "KB", "MB", "GB", "TB"]

    static func split(_ bytes: UInt64) -> (String, String) {
        var value = Double(bytes)
        var idx = 0
        while value >= 1000, idx < units.count - 1 {
            value /= 1000
            idx += 1
        }
        if idx == 0 { return ("\(bytes)", "B") }
        let digits = value >= 100 ? 0 : (value >= 10 ? 1 : 2)
        return (String(format: "%.\(digits)f", value), units[idx])
    }

    static func string(_ bytes: UInt64) -> String {
        let (v, u) = split(bytes)
        return "\(v) \(u)"
    }

    /// 速率：B/s 用整数，其余保留一位。
    static func rate(_ bytesPerSecond: Double) -> String {
        if bytesPerSecond < 1000 { return "\(Int(bytesPerSecond)) B/s" }
        var value = bytesPerSecond
        var idx = 1
        while value >= 1000, idx < units.count - 1 {
            value /= 1000
            idx += 1
        }
        return String(format: "%.1f %@/s", value, units[idx])
    }
}
