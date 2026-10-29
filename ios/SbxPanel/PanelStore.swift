import Foundation

/// 一个面板实例（可以有多台服务器）。
struct PanelEndpoint: Codable, Identifiable, Equatable {
    var id: UUID = UUID()
    var name: String
    var address: String

    /// 规范化成可加载的 URL。
    ///
    /// 允许用户只输入 `1.2.3.4:8080`——这是最常见的粘贴形式，强制补 scheme
    /// 比报错让用户自己补要友好。默认补 http 而不是 https：面板自身不内置
    /// TLS，绝大多数部署是明文直连+反代二选一，猜错的代价是白屏。
    var url: URL? {
        var s = address.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !s.isEmpty else { return nil }
        s = s.replacingOccurrences(of: " ", with: "")
        let lower = s.lowercased()
        if !lower.hasPrefix("http://") && !lower.hasPrefix("https://") {
            s = "http://" + s
        }
        guard let u = URL(string: s), let host = u.host, !host.isEmpty else { return nil }
        return u
    }

    var isValid: Bool { url != nil }
}

@MainActor
final class PanelStore: ObservableObject {
    @Published private(set) var endpoints: [PanelEndpoint] = []
    @Published var currentID: UUID?

    private let listKey = "sbx.endpoints.v1"
    private let currentKey = "sbx.current.v1"

    init() { load() }

    var current: PanelEndpoint? {
        if let id = currentID, let hit = endpoints.first(where: { $0.id == id }) {
            return hit
        }
        return endpoints.first
    }

    var hasEndpoints: Bool { !endpoints.isEmpty }

    @discardableResult
    func add(name: String, address: String) -> PanelEndpoint {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        let ep = PanelEndpoint(
            name: trimmed.isEmpty ? defaultName(for: address) : trimmed,
            address: address.trimmingCharacters(in: .whitespacesAndNewlines)
        )
        endpoints.append(ep)
        currentID = ep.id
        save()
        return ep
    }

    func update(_ ep: PanelEndpoint) {
        guard let i = endpoints.firstIndex(where: { $0.id == ep.id }) else { return }
        endpoints[i] = ep
        save()
    }

    func remove(_ ep: PanelEndpoint) {
        endpoints.removeAll { $0.id == ep.id }
        if currentID == ep.id {
            currentID = endpoints.first?.id
        }
        save()
    }

    func select(_ ep: PanelEndpoint) {
        currentID = ep.id
        save()
    }

    /// 从地址里猜一个显示名，避免列表全是「未命名」。
    private func defaultName(for address: String) -> String {
        let s = address.trimmingCharacters(in: .whitespacesAndNewlines)
        if let u = URL(string: s.lowercased().hasPrefix("http") ? s : "http://" + s),
           let host = u.host {
            return host
        }
        return s.isEmpty ? "面板" : s
    }

    // MARK: - 持久化

    private func load() {
        let d = UserDefaults.standard
        if let data = d.data(forKey: listKey),
           let list = try? JSONDecoder().decode([PanelEndpoint].self, from: data) {
            endpoints = list
        }
        if let s = d.string(forKey: currentKey), let id = UUID(uuidString: s) {
            currentID = id
        }
        if currentID == nil { currentID = endpoints.first?.id }
    }

    private func save() {
        let d = UserDefaults.standard
        if let data = try? JSONEncoder().encode(endpoints) {
            d.set(data, forKey: listKey)
        }
        d.set(currentID?.uuidString, forKey: currentKey)
    }
}
