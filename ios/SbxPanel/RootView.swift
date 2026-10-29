import SwiftUI
import UIKit

// 与面板 CSS 变量保持一致的配色（宣纸 / 浓墨 / 朱砂）。
enum Palette {
    static let paper = Color(red: 0.945, green: 0.929, blue: 0.890)   // #f1ede3
    static let surface = Color(red: 0.976, green: 0.969, blue: 0.941) // #f9f7f0
    static let ink = Color(red: 0.165, green: 0.145, blue: 0.110)     // #2a251c
    static let inkSoft = Color(red: 0.361, green: 0.329, blue: 0.259) // #5c5442
    static let line = Color(red: 0.871, green: 0.843, blue: 0.769)    // #ded7c4
    static let seal = Color(red: 0.694, green: 0.227, blue: 0.188)    // #b13a30
}

struct RootView: View {
    @EnvironmentObject private var store: PanelStore

    @State private var progress: Double = 0
    @State private var isLoading = false
    @State private var loadError: String?
    @State private var toast: String?
    @State private var reloadToken = 0
    @State private var showSettings = false

    var body: some View {
        ZStack {
            Palette.paper.ignoresSafeArea()

            if let endpoint = store.current {
                panel(endpoint)
            } else {
                OnboardingView()
            }

            if let toast {
                ToastView(text: toast)
                    // 连续两次提示时靠 id 强制重建，让 2.6 秒计时重新开始。
                    .id(toast)
                    .transition(.move(edge: .bottom).combined(with: .opacity))
                    .onAppear {
                        DispatchQueue.main.asyncAfter(deadline: .now() + 2.6) {
                            withAnimation(.easeOut(duration: 0.2)) { self.toast = nil }
                        }
                    }
            }
        }
        .sheet(isPresented: $showSettings) {
            SettingsView()
                .environmentObject(store)
        }
        .onChange(of: store.currentID) { _, _ in
            // 切服务器时清掉上一台的错误页，否则新页面加载前会闪现旧报错。
            loadError = nil
            progress = 0
        }
    }

    // MARK: - 主界面

    @ViewBuilder
    private func panel(_ endpoint: PanelEndpoint) -> some View {
        VStack(spacing: 0) {
            topBar(endpoint)
            ZStack(alignment: .top) {
                PanelWebView(
                    endpoint: endpoint,
                    reloadToken: reloadToken,
                    progress: $progress,
                    isLoading: $isLoading,
                    loadError: $loadError,
                    toast: $toast
                )
                .ignoresSafeArea(edges: .bottom)

                if isLoading && progress > 0 && progress < 1 {
                    ProgressView(value: progress)
                        .progressViewStyle(.linear)
                        .tint(Palette.inkSoft)
                        .scaleEffect(x: 1, y: 0.6, anchor: .top)
                }

                if let loadError {
                    ErrorOverlay(message: loadError) {
                        self.loadError = nil
                        reloadToken += 1
                    }
                }
            }
        }
    }

    private func topBar(_ endpoint: PanelEndpoint) -> some View {
        HStack(spacing: 14) {
            Menu {
                ForEach(store.endpoints) { ep in
                    Button {
                        store.select(ep)
                    } label: {
                        if ep.id == endpoint.id {
                            Label(ep.name, systemImage: "checkmark")
                        } else {
                            Text(ep.name)
                        }
                    }
                }
                Divider()
                Button {
                    showSettings = true
                } label: {
                    Label("管理服务器", systemImage: "slider.horizontal.3")
                }
            } label: {
                HStack(spacing: 6) {
                    Circle()
                        .fill(isLoading ? Palette.inkSoft : Palette.seal)
                        .frame(width: 6, height: 6)
                    Text(endpoint.name)
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(Palette.ink)
                        .lineLimit(1)
                    Image(systemName: "chevron.down")
                        .font(.system(size: 9, weight: .bold))
                        .foregroundStyle(Palette.inkSoft)
                }
            }

            Spacer(minLength: 0)

            Button {
                reloadToken += 1
            } label: {
                Image(systemName: "arrow.clockwise")
                    .font(.system(size: 15, weight: .medium))
                    .foregroundStyle(Palette.inkSoft)
                    .frame(width: 34, height: 34)
                    .contentShape(Rectangle())
            }
            .accessibilityLabel("刷新")

            Button {
                showSettings = true
            } label: {
                Image(systemName: "slider.horizontal.3")
                    .font(.system(size: 15, weight: .medium))
                    .foregroundStyle(Palette.inkSoft)
                    .frame(width: 34, height: 34)
                    .contentShape(Rectangle())
            }
            .accessibilityLabel("设置")
        }
        .padding(.horizontal, 16)
        .frame(height: 46)
        .background(Palette.paper)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.line).frame(height: 0.5)
        }
    }
}

// MARK: - 首次启动引导

struct OnboardingView: View {
    @EnvironmentObject private var store: PanelStore
    @State private var address = ""
    @State private var name = ""
    @State private var showError = false

    private var canSubmit: Bool {
        PanelEndpoint(name: "", address: address).isValid
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                VStack(alignment: .leading, spacing: 6) {
                    HStack(spacing: 10) {
                        Text("墨")
                            .font(.system(size: 17, weight: .bold, design: .serif))
                            .foregroundStyle(Palette.paper)
                            .frame(width: 34, height: 34)
                            .background(Palette.seal, in: RoundedRectangle(cornerRadius: 5))
                        Text("SBX")
                            .font(.system(size: 26, weight: .bold, design: .serif))
                            .foregroundStyle(Palette.ink)
                    }
                    Text("流量面板")
                        .font(.system(size: 13))
                        .foregroundStyle(Palette.inkSoft)
                }
                .padding(.top, 48)

                Text("填写面板地址")
                    .font(.system(size: 20, weight: .semibold, design: .serif))
                    .foregroundStyle(Palette.ink)

                VStack(alignment: .leading, spacing: 14) {
                    field("地址", placeholder: "1.2.3.4:8080", text: $address, keyboard: .URL)
                    field("备注（可选）", placeholder: "香港面板", text: $name, keyboard: .default)
                }

                if showError {
                    Text("地址格式不对，至少要有主机名或 IP，例如 1.2.3.4:8080")
                        .font(.system(size: 12))
                        .foregroundStyle(Palette.seal)
                }

                Button {
                    guard canSubmit else { showError = true; return }
                    store.add(name: name, address: address)
                } label: {
                    Text("进入面板")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(canSubmit ? Palette.paper : Palette.inkSoft)
                        .frame(maxWidth: .infinity)
                        .frame(height: 46)
                        .background(canSubmit ? Palette.ink : Palette.line,
                                    in: RoundedRectangle(cornerRadius: 10))
                }
                .disabled(!canSubmit)
                .opacity(canSubmit ? 1 : 0.7)

                Text("地址会保存在本机，不会上传到任何服务器。\n面板本身不内置 HTTPS，直连请用 http://")
                    .font(.system(size: 11.5))
                    .foregroundStyle(Palette.inkSoft)
                    .lineSpacing(3)
                    .padding(.top, 2)
            }
            .padding(.horizontal, 24)
            .frame(maxWidth: 520)
            .frame(maxWidth: .infinity)
        }
        .background(Palette.paper)
    }

    @ViewBuilder
    private func field(_ label: String, placeholder: String, text: Binding<String>, keyboard: UIKeyboardType) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(label)
                .font(.system(size: 11.5))
                .foregroundStyle(Palette.inkSoft)
            TextField(placeholder, text: text)
                .font(.system(size: 15))
                .foregroundStyle(Palette.ink)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .keyboardType(keyboard)
                .padding(.horizontal, 12)
                .frame(height: 44)
                .background(Palette.surface, in: RoundedRectangle(cornerRadius: 9))
                .overlay {
                    RoundedRectangle(cornerRadius: 9).stroke(Palette.line, lineWidth: 1)
                }
        }
    }
}

// MARK: - 设置

struct SettingsView: View {
    @EnvironmentObject private var store: PanelStore
    @Environment(\.dismiss) private var dismiss

    @State private var newAddress = ""
    @State private var newName = ""
    @State private var showAddError = false

    var body: some View {
        NavigationStack {
            List {
                Section("已保存的服务器") {
                    ForEach(store.endpoints) { ep in
                        Button {
                            store.select(ep)
                        } label: {
                            HStack(spacing: 10) {
                                Image(systemName: ep.id == store.current?.id ? "largecircle.fill.circle" : "circle")
                                    .foregroundStyle(ep.id == store.current?.id ? Palette.seal : Palette.inkSoft)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(ep.name)
                                        .font(.system(size: 15, weight: .medium))
                                        .foregroundStyle(Palette.ink)
                                    Text(ep.address)
                                        .font(.system(size: 11.5))
                                        .foregroundStyle(Palette.inkSoft)
                                        .lineLimit(1)
                                }
                            }
                        }
                    }
                    .onDelete { idx in
                        idx.map { store.endpoints[$0] }.forEach(store.remove)
                    }
                }

                Section("添加") {
                    TextField("地址，例如 1.2.3.4:8080", text: $newAddress)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                    TextField("备注（可选）", text: $newName)
                    Button("添加并切换") {
                        let ep = PanelEndpoint(name: "", address: newAddress)
                        guard ep.isValid else { showAddError = true; return }
                        store.add(name: newName, address: newAddress)
                        newAddress = ""
                        newName = ""
                        showAddError = false
                        dismiss()
                    }
                    .disabled(newAddress.trimmingCharacters(in: .whitespaces).isEmpty)
                }

                if showAddError {
                    Section {
                        Text("地址格式不对")
                            .font(.system(size: 12))
                            .foregroundStyle(Palette.seal)
                    }
                }

                Section {
                    Text("登录状态保存在 App 的 WebView 里，面板 Cookie 有效期 7 天。要彻底退出，请在面板里点「登出」。")
                        .font(.system(size: 12))
                        .foregroundStyle(Palette.inkSoft)
                }
            }
            .scrollContentBackground(.hidden)
            .background(Palette.paper)
            .navigationTitle("服务器")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("完成") { dismiss() }
                        .foregroundStyle(Palette.ink)
                }
            }
        }
        .presentationDetents([.medium, .large])
    }
}

// MARK: - 错误页与提示

struct ErrorOverlay: View {
    let message: String
    let retry: () -> Void

    var body: some View {
        VStack(spacing: 14) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 26, weight: .light))
                .foregroundStyle(Palette.seal)
            Text("打不开面板")
                .font(.system(size: 16, weight: .semibold, design: .serif))
                .foregroundStyle(Palette.ink)
            Text(message)
                .font(.system(size: 13))
                .foregroundStyle(Palette.inkSoft)
                .multilineTextAlignment(.center)
                .lineSpacing(3)
                .padding(.horizontal, 36)
            Button(action: retry) {
                Text("重试")
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(Palette.paper)
                    .padding(.horizontal, 26)
                    .frame(height: 40)
                    .background(Palette.ink, in: Capsule())
            }
            .padding(.top, 4)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Palette.paper)
    }
}

struct ToastView: View {
    let text: String

    var body: some View {
        VStack {
            Spacer()
            Text(text)
                .font(.system(size: 12.5))
                .foregroundStyle(Palette.paper)
                .multilineTextAlignment(.center)
                .padding(.horizontal, 16)
                .padding(.vertical, 10)
                .background(Palette.ink.opacity(0.92), in: RoundedRectangle(cornerRadius: 10))
                .padding(.horizontal, 32)
                .padding(.bottom, 34)
        }
        .allowsHitTesting(false)
    }
}
