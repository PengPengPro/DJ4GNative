package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const networkTrafficStateFile = "network-traffic.json"

// networkTrafficPersistedState 持久化累计流量：跨进程重启续计。
// 用上次采样的内核计数做差分；网卡重命名时保留累计、重新打基线；
// 内核计数回绕/清零时把当前值当作新增量。
type networkTrafficPersistedState struct {
	TotalRX       uint64 `json:"total_rx"`
	TotalTX       uint64 `json:"total_tx"`
	LastInterface string `json:"last_interface,omitempty"`
	LastRX        uint64 `json:"last_rx"`
	LastTX        uint64 `json:"last_tx"`
}

func (a *app) initTrafficStats() {
	if a.dataDir == "" {
		return
	}
	path := filepath.Join(a.dataDir, networkTrafficStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state networkTrafficPersistedState
	if json.Unmarshal(data, &state) != nil {
		return
	}
	a.trafficMu.Lock()
	a.trafficStats = state
	a.trafficMu.Unlock()
}

func (a *app) saveTrafficStatsLocked() {
	if a.dataDir == "" {
		return
	}
	data, err := json.Marshal(a.trafficStats)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(a.dataDir, networkTrafficStateFile), data, 0o600)
}

// accumulateModuleTraffic 按网卡内核计数差分累加，并写回磁盘。
// 返回持久化后的累计 RX / TX / 合计。
func (a *app) accumulateModuleTraffic(iface string, current networkByteCounters) (rx, tx, total uint64) {
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()

	a.trafficStats = applyTrafficSample(a.trafficStats, iface, current)
	a.saveTrafficStatsLocked()
	return a.trafficStats.TotalRX, a.trafficStats.TotalTX, a.trafficStats.TotalRX + a.trafficStats.TotalTX
}

func applyTrafficSample(state networkTrafficPersistedState, iface string, current networkByteCounters) networkTrafficPersistedState {
	if iface != "" && state.LastInterface == iface {
		state.TotalRX += counterDelta(state.LastRX, current.RX)
		state.TotalTX += counterDelta(state.LastTX, current.TX)
	}
	state.LastInterface = iface
	state.LastRX = current.RX
	state.LastTX = current.TX
	return state
}

func counterDelta(previous, current uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	// 内核计数清零或回绕：当前值视为自重置以来的增量。
	return current
}
