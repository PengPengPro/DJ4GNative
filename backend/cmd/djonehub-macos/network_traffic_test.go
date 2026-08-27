package main

import "testing"

func TestParseMacNetworkServiceOrderMarksModuleInterface(t *testing.T) {
	const output = `An asterisk (*) denotes that a network service is disabled.
(1) DL-Dock
(Hardware Port: DL-Dock, Device: en7)

(2) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(3) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(4) Baiwang
(Hardware Port: Baiwang, Device: en8)
`

	services := parseMacNetworkServiceOrder(output, "Baiwang")
	if len(services) != 4 {
		t.Fatalf("len(services) = %d, want 4", len(services))
	}
	if services[0].Module {
		t.Fatal("DL-Dock must not be marked as the module interface")
	}
	module := services[3]
	if module.Name != "Baiwang" || module.Device != "en8" || !module.Module {
		t.Fatalf("module service = %+v, want Baiwang/en8 with module=true", module)
	}
}

func TestSelectModuleTrafficInterfaceIgnoresEarlierUSBInterfaces(t *testing.T) {
	interfaces := []macNetInterface{
		{Name: "en7", Status: "active", Kind: "ethernet"},
		{Name: "en0", Status: "active", Kind: "ethernet"},
		{Name: "en8", Status: "active", Kind: "ethernet"},
	}
	services := []networkService{
		{Name: "DL-Dock", Device: "en7", USB: true},
		{Name: "Wi-Fi", Device: "en0"},
		{Name: "Baiwang", Device: "en8", USB: true, Module: true},
	}

	name, active := selectModuleTrafficInterface(interfaces, services)
	if name != "en8" || !active {
		t.Fatalf("selectModuleTrafficInterface() = %q, %v; want en8, true", name, active)
	}
}

func TestSelectModuleTrafficInterfaceDoesNotFallback(t *testing.T) {
	interfaces := []macNetInterface{
		{Name: "en7", Status: "active", Kind: "ethernet"},
		{Name: "en8", Status: "inactive", Kind: "ethernet"},
	}

	t.Run("module inactive", func(t *testing.T) {
		services := []networkService{
			{Name: "DL-Dock", Device: "en7", USB: true},
			{Name: "Baiwang", Device: "en8", USB: true, Module: true},
		}
		name, active := selectModuleTrafficInterface(interfaces, services)
		if name != "en8" || active {
			t.Fatalf("selectModuleTrafficInterface() = %q, %v; want en8, false", name, active)
		}
	})

	t.Run("module unidentified", func(t *testing.T) {
		services := []networkService{
			{Name: "DL-Dock", Device: "en7", USB: true},
			{Name: "Baiwang", Device: "en8", USB: true},
		}
		name, active := selectModuleTrafficInterface(interfaces, services)
		if name != "" || active {
			t.Fatalf("selectModuleTrafficInterface() = %q, %v; want empty, false", name, active)
		}
	})
}

func TestResolveLiveTrafficInterfacePrefersActiveUnderlay(t *testing.T) {
	counters := map[string]networkByteCounters{
		"en0":   {RX: 10, TX: 1},
		"en8":   {RX: 20, TX: 2},
		"utun5": {RX: 30, TX: 3},
	}
	services := []networkService{
		{Name: "Wi-Fi", Device: "en0"},
		{Name: "Baiwang", Device: "en8", USB: true, Module: true},
		{Name: "Quantumult X", Device: ""},
	}

	if got := resolveLiveTrafficInterface("en8", services, counters); got != "en8" {
		t.Fatalf("resolveLiveTrafficInterface(active=en8) = %q, want en8", got)
	}
	if got := resolveLiveTrafficInterface("en0", services, counters); got != "en0" {
		t.Fatalf("resolveLiveTrafficInterface(active=en0) = %q, want en0", got)
	}
	if got := resolveLiveTrafficInterface("en0", nil, counters); got != "en0" {
		t.Fatalf("resolveLiveTrafficInterface(en0) = %q, want en0", got)
	}
}

func TestApplyTrafficSamplePersistsAcrossRestart(t *testing.T) {
	// 首次采样只打基线。
	state := applyTrafficSample(networkTrafficPersistedState{}, "en8", networkByteCounters{RX: 1000, TX: 100})
	if state.TotalRX != 0 || state.TotalTX != 0 {
		t.Fatalf("first sample totals = %+v, want zero", state)
	}

	// 进程内继续累加。
	state = applyTrafficSample(state, "en8", networkByteCounters{RX: 1500, TX: 140})
	if state.TotalRX != 500 || state.TotalTX != 40 {
		t.Fatalf("after growth totals = rx %d tx %d, want 500/40", state.TotalRX, state.TotalTX)
	}

	// 模拟重启后从磁盘恢复：累计与 last_* 仍在。
	restored := state
	restored = applyTrafficSample(restored, "en8", networkByteCounters{RX: 1800, TX: 200})
	if restored.TotalRX != 800 || restored.TotalTX != 100 {
		t.Fatalf("after restart totals = rx %d tx %d, want 800/100", restored.TotalRX, restored.TotalTX)
	}
}

func TestApplyTrafficSampleCounterReset(t *testing.T) {
	state := networkTrafficPersistedState{
		TotalRX:       5000,
		TotalTX:       900,
		LastInterface: "en8",
		LastRX:        2000,
		LastTX:        400,
	}
	state = applyTrafficSample(state, "en8", networkByteCounters{RX: 50, TX: 10})
	if state.TotalRX != 5050 || state.TotalTX != 910 {
		t.Fatalf("after counter reset totals = rx %d tx %d, want 5050/910", state.TotalRX, state.TotalTX)
	}
}

func TestApplyTrafficSampleInterfaceRenameKeepsTotals(t *testing.T) {
	state := networkTrafficPersistedState{
		TotalRX:       5000,
		TotalTX:       900,
		LastInterface: "en8",
		LastRX:        2000,
		LastTX:        400,
	}
	state = applyTrafficSample(state, "en9", networkByteCounters{RX: 30, TX: 5})
	if state.TotalRX != 5000 || state.TotalTX != 900 {
		t.Fatalf("after rename totals = rx %d tx %d, want 5000/900", state.TotalRX, state.TotalTX)
	}
	if state.LastInterface != "en9" || state.LastRX != 30 || state.LastTX != 5 {
		t.Fatalf("after rename baseline = %+v", state)
	}
}

func TestCounterDelta(t *testing.T) {
	if got := counterDelta(100, 150); got != 50 {
		t.Fatalf("counterDelta(100,150) = %d, want 50", got)
	}
	if got := counterDelta(100, 40); got != 40 {
		t.Fatalf("counterDelta(100,40) = %d, want 40", got)
	}
}
