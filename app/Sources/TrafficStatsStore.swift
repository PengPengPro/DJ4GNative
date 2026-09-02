import Foundation

@MainActor
final class TrafficStatsStore: ObservableObject {
    enum RangeMode: String, CaseIterable, Identifiable {
        case today
        case allTime
        case custom

        var id: String { rawValue }

        var title: String {
            switch self {
            case .today: return "今天"
            case .allTime: return "全部"
            case .custom: return "自定义"
            }
        }
    }

    @Published private(set) var usage: AppTrafficUsageResponse?
    @Published private(set) var isLoading = false
    @Published var errorMessage: String?
    @Published var rangeMode: RangeMode = .today
    @Published var rangeStart = Calendar.current.date(byAdding: .day, value: -6, to: Date()) ?? Date()
    @Published var rangeEnd = Date()

    private let client = APIClient(timeoutInterval: 20)
    private var pollTask: Task<Void, Never>?

    deinit {
        pollTask?.cancel()
    }

    func load() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            await self?.refresh()
        }
    }

    func beginPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refresh()
                try? await Task.sleep(nanoseconds: 20_000_000_000)
            }
        }
    }

    func endPolling() {
        pollTask?.cancel()
        pollTask = nil
    }

    func refresh() async {
        guard !isLoading else { return }
        isLoading = true
        defer { isLoading = false }

        do {
            let path = queryPath()
            usage = try await client.get(path, as: AppTrafficUsageResponse.self)
            errorMessage = nil
        } catch {
            if !Task.isCancelled {
                errorMessage = error.localizedDescription
            }
        }
    }

    private func queryPath() -> String {
        let formatter = DateFormatter()
        formatter.calendar = Calendar.current
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = .current
        formatter.dateFormat = "yyyy-MM-dd"

        switch rangeMode {
        case .today:
            return "/api/network/traffic/apps?date=\(formatter.string(from: Date()))"
        case .allTime:
            return "/api/network/traffic/apps"
        case .custom:
            let from = formatter.string(from: rangeStart)
            let to = formatter.string(from: rangeEnd)
            return "/api/network/traffic/apps?from=\(from)&to=\(to)"
        }
    }
}
