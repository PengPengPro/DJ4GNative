import AppKit
import SwiftUI

struct TrafficStatsView: View {
    @StateObject private var store = TrafficStatsStore()

    private let byteFormatter: ByteCountFormatter = {
        let formatter = ByteCountFormatter()
        formatter.countStyle = .binary
        return formatter
    }()

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                header
                filters
                summaryCard
                appsCard
            }
            .padding(18)
            .frame(maxWidth: 900, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .top)
        }
        .navigationTitle("流量统计")
        .onAppear {
            store.load()
            store.beginPolling()
        }
        .onDisappear {
            store.endPolling()
        }
        .onChange(of: store.rangeMode) { _ in
            store.load()
        }
        .onChange(of: store.rangeStart) { _ in
            if store.rangeMode == .custom {
                store.load()
            }
        }
        .onChange(of: store.rangeEnd) { _ in
            if store.rangeMode == .custom {
                store.load()
            }
        }
        .alert("读取失败", isPresented: Binding(
            get: { store.errorMessage != nil },
            set: { if !$0 { store.errorMessage = nil } }
        )) {
            Button("好", role: .cancel) { store.errorMessage = nil }
        } message: {
            Text(store.errorMessage ?? "未知错误")
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("应用流量统计")
                .font(.title2.bold())
            Text("按应用汇总 4G 模块流量。启用应用分流时按进程精确统计；未启用时在 4G 为当前上网通道时按系统进程采样。")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
    }

    private var filters: some View {
        VStack(alignment: .leading, spacing: 10) {
            Picker("时间范围", selection: $store.rangeMode) {
                ForEach(TrafficStatsStore.RangeMode.allCases) { mode in
                    Text(mode.title).tag(mode)
                }
            }
            .pickerStyle(.segmented)

            if store.rangeMode == .custom {
                HStack(spacing: 12) {
                    DatePicker("开始", selection: $store.rangeStart, displayedComponents: .date)
                    DatePicker("结束", selection: $store.rangeEnd, displayedComponents: .date)
                }
                .labelsHidden()
            }
        }
    }

    private var summaryCard: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("汇总")
                    .font(.headline)
                Spacer()
                if store.isLoading {
                    ProgressView().controlSize(.small)
                }
            }
            HStack(spacing: 16) {
                metricItem("下载", usage?.totalRXBytes)
                Divider().frame(height: 26)
                metricItem("上传", usage?.totalTXBytes)
                Divider().frame(height: 26)
                metricItem("总流量", usage?.totalBytes)
            }
            if let updatedAt = store.usage?.updatedAt {
                Text("最近更新：\(updatedAt.formatted(date: .abbreviated, time: .standard))")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(14)
        .background(cardBackground)
    }

    private var appsCard: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("各应用用量")
                .font(.headline)

            if let apps = store.usage?.apps, !apps.isEmpty {
                ForEach(apps) { app in
                    appRow(app)
                    if app.id != apps.last?.id {
                        Divider()
                    }
                }
            } else if store.isLoading && store.usage == nil {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small)
                    Text("读取流量统计…")
                        .foregroundStyle(.secondary)
                }
                .padding(.vertical, 8)
            } else {
                Text(emptyMessage)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .padding(.vertical, 8)
            }
        }
        .padding(14)
        .background(cardBackground)
    }

    private var usage: AppTrafficUsageResponse? { store.usage }

    private var emptyMessage: String {
        switch store.rangeMode {
        case .today:
            return "今天还没有记录到应用流量。请确认 4G 模块已连接，并在使用 4G 时产生网络流量。"
        case .allTime:
            return "暂无应用流量记录。"
        case .custom:
            return "所选日期范围内没有应用流量记录。"
        }
    }

    private func appRow(_ app: AppTrafficUsageItem) -> some View {
        HStack(alignment: .top, spacing: 12) {
            appIcon(for: app)
            VStack(alignment: .leading, spacing: 4) {
                Text(app.name)
                    .font(.body.weight(.medium))
                if let bundlePath = app.bundlePath, !bundlePath.isEmpty {
                    Text(bundlePath)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                HStack(spacing: 12) {
                    Text("↓ \(formatBytes(app.rxBytes))")
                    Text("↑ \(formatBytes(app.txBytes))")
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            Spacer(minLength: 8)
            Text(formatBytes(app.totalBytes))
                .font(.body.monospacedDigit())
        }
        .padding(.vertical, 4)
    }

    @ViewBuilder
    private func appIcon(for app: AppTrafficUsageItem) -> some View {
        if let bundlePath = app.bundlePath {
            Image(nsImage: NSWorkspace.shared.icon(forFile: bundlePath))
                .resizable()
                .frame(width: 32, height: 32)
        } else {
            Image(systemName: "app.fill")
                .font(.title2)
                .foregroundStyle(.secondary)
                .frame(width: 32, height: 32)
        }
    }

    private func metricItem(_ title: String, _ bytes: UInt64?) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(.caption)
                .foregroundStyle(.secondary)
            Text(formatBytes(bytes))
                .font(.body.monospacedDigit())
        }
    }

    private func formatBytes(_ bytes: UInt64?) -> String {
        guard let bytes else { return "-" }
        return byteFormatter.string(fromByteCount: Int64(bytes))
    }

    private var cardBackground: some View {
        RoundedRectangle(cornerRadius: 10)
            .fill(Color(nsColor: .controlBackgroundColor))
            .overlay(
                RoundedRectangle(cornerRadius: 10)
                    .stroke(Color(nsColor: .separatorColor), lineWidth: 1)
            )
    }
}
