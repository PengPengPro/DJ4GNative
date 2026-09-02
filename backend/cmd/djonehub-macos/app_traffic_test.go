package main

import (
	"testing"
	"time"
)

func TestBuildAppTrafficUsageResponseSingleDay(t *testing.T) {
	state := appTrafficPersistedState{
		UpdatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.Local),
		Apps: map[string]*appTrafficAppRecord{
			"/Applications/Safari.app": {
				ID:   "/Applications/Safari.app",
				Name: "Safari",
				Days: map[string]appTrafficDayUsage{
					"2026-09-02": {RXBytes: 100, TXBytes: 50, TotalBytes: 150},
					"2026-09-01": {RXBytes: 10, TXBytes: 5, TotalBytes: 15},
				},
				TotalRX: 110,
				TotalTX: 55,
			},
		},
	}

	response := buildAppTrafficUsageResponse(state, "2026-09-02", "", "")
	if response.Date != "2026-09-02" {
		t.Fatalf("date=%q, want 2026-09-02", response.Date)
	}
	if len(response.Apps) != 1 || response.Apps[0].TotalBytes != 150 {
		t.Fatalf("apps=%+v, want single day total 150", response.Apps)
	}
	if response.TotalBytes != 150 {
		t.Fatalf("total=%d, want 150", response.TotalBytes)
	}
}

func TestBuildAppTrafficUsageResponseRange(t *testing.T) {
	state := appTrafficPersistedState{
		Apps: map[string]*appTrafficAppRecord{
			"/Applications/Safari.app": {
				ID:   "/Applications/Safari.app",
				Name: "Safari",
				Days: map[string]appTrafficDayUsage{
					"2026-09-01": {RXBytes: 10, TXBytes: 5, TotalBytes: 15},
					"2026-09-02": {RXBytes: 100, TXBytes: 50, TotalBytes: 150},
				},
			},
		},
	}

	response := buildAppTrafficUsageResponse(state, "", "2026-09-01", "2026-09-02")
	if len(response.Apps) != 1 || response.Apps[0].TotalBytes != 165 {
		t.Fatalf("apps=%+v, want range total 165", response.Apps)
	}
}

func TestBuildAppTrafficUsageResponseAllTime(t *testing.T) {
	state := appTrafficPersistedState{
		Apps: map[string]*appTrafficAppRecord{
			"/Applications/Safari.app": {
				ID:      "/Applications/Safari.app",
				Name:    "Safari",
				TotalRX: 1000,
				TotalTX: 500,
				Days: map[string]appTrafficDayUsage{
					"2026-09-02": {RXBytes: 100, TXBytes: 50, TotalBytes: 150},
				},
			},
		},
	}

	response := buildAppTrafficUsageResponse(state, "", "", "")
	if len(response.Apps) != 1 || response.Apps[0].TotalBytes != 1500 {
		t.Fatalf("apps=%+v, want all-time total 1500", response.Apps)
	}
}

func TestParseNettopProcessField(t *testing.T) {
	name, pid, ok := parseNettopProcessField("Safari.1234")
	if !ok || name != "Safari" || pid != 1234 {
		t.Fatalf("parse Safari.1234 = %q %d %v", name, pid, ok)
	}
	name, pid, ok = parseNettopProcessField("Cursor Helper (.28060")
	if !ok || name != "Cursor Helper (" || pid != 28060 {
		t.Fatalf("parse helper = %q %d %v", name, pid, ok)
	}
}

func TestParseNettopProcessCounters(t *testing.T) {
	output := "time,,interface,state,bytes_in,bytes_out\n" +
		"19:17:15.529763,Safari.1234,,,16656,46979\n" +
		"19:17:15.529764,WeChat.2891,,,3779762,2026883\n"
	counters := parseNettopProcessCounters(output)
	if counters[1234].RX != 16656 || counters[1234].TX != 46979 {
		t.Fatalf("safari=%+v", counters[1234])
	}
	if counters[2891].RX != 3779762 {
		t.Fatalf("wechat=%+v", counters[2891])
	}
}

func TestAppTrafficIdentityFromProcessPath(t *testing.T) {
	key, name, bundle := appTrafficIdentityFromProcessPath("/Applications/Safari.app/Contents/MacOS/Safari")
	if key != "/Applications/Safari.app" || name != "Safari.app" || bundle != "/Applications/Safari.app" {
		t.Fatalf("identity=%q %q %q", key, name, bundle)
	}
}

func TestEnumerateAppTrafficDays(t *testing.T) {
	days := enumerateAppTrafficDays("2026-09-01", "2026-09-03")
	if len(days) != 3 || days[0] != "2026-09-01" || days[2] != "2026-09-03" {
		t.Fatalf("days=%v", days)
	}
}
