package main

import "testing"

func TestPromoteService(t *testing.T) {
	got := promoteService([]string{"Wi-Fi", "Baiwang", "USB 10/100/1000 LAN"}, "Baiwang")
	want := []string{"Baiwang", "Wi-Fi", "USB 10/100/1000 LAN"}
	if !sameStringSlice(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestIntersectPreferredKeepsOrderAndAppendsNew(t *testing.T) {
	services := []networkService{
		{Name: "Wi-Fi"},
		{Name: "Baiwang"},
		{Name: "Thunderbolt Bridge"},
	}
	got := intersectPreferred([]string{"Baiwang", "Missing", "Wi-Fi"}, services)
	want := []string{"Baiwang", "Wi-Fi", "Thunderbolt Bridge"}
	if !sameStringSlice(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSameStringSlice(t *testing.T) {
	if !sameStringSlice([]string{"a", "b"}, []string{"a", "b"}) {
		t.Fatal("expected equal")
	}
	if sameStringSlice([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("expected unequal")
	}
}

func TestClassifyNetworkPath(t *testing.T) {
	kind, _ := classifyNetworkPath(networkService{Name: "Wi-Fi", Device: "en0"})
	if kind != "wifi" {
		t.Fatalf("wifi kind=%s", kind)
	}
	kind, _ = classifyNetworkPath(networkService{Name: "DJ-4G", Module: true, Device: "en8"})
	if kind != "cellular" {
		t.Fatalf("cellular kind=%s", kind)
	}
	kind, _ = classifyNetworkPath(networkService{Name: "Ethernet", USB: true, Device: "en7"})
	if kind != "ethernet" {
		t.Fatalf("ethernet kind=%s", kind)
	}
	kind, _ = classifyNetworkPath(networkService{Name: "Quantumult X"})
	if kind != "vpn" {
		t.Fatalf("vpn kind=%s", kind)
	}
}

func TestInterfaceFailoverCandidateModuleLinkLocal(t *testing.T) {
	if interfaceFailoverCandidate(networkService{Name: "Quantumult X"}) {
		t.Fatal("vpn without device should not be candidate")
	}
}

func TestPromoteUnderlayKeepingOverlay(t *testing.T) {
	services := []networkService{
		{Name: "Wi-Fi", Device: "en1"},
		{Name: "DJ-4G", Device: "en8", Module: true},
		{Name: "Quantumult X"},
	}
	got := promoteUnderlayKeepingOverlay(
		[]string{"Wi-Fi", "DJ-4G", "Quantumult X"},
		"DJ-4G",
		services,
	)
	want := []string{"DJ-4G", "Wi-Fi", "Quantumult X"}
	if !sameStringSlice(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLooksLikeCellularService(t *testing.T) {
	if !looksLikeCellularService(networkService{Name: "DJ-4G", Port: "EG25G-QDC507"}) {
		t.Fatal("DJ-4G should look cellular")
	}
	if looksLikeCellularService(networkService{Name: "Wi-Fi", Device: "en1"}) {
		t.Fatal("Wi-Fi should not look cellular")
	}
}

func TestUnderlayPreferredOrderPinsOverlay(t *testing.T) {
	services := []networkService{
		{Name: "Wi-Fi", Device: "en1"},
		{Name: "DJ-4G", Device: "en8", Module: true},
		{Name: "Quantumult X"},
	}
	got := underlayPreferredOrder([]string{"Quantumult X", "Wi-Fi", "DJ-4G"}, services)
	want := []string{"Wi-Fi", "DJ-4G", "Quantumult X"}
	if !sameStringSlice(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
