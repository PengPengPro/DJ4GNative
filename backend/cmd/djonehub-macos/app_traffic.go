package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	appTrafficStateFile       = "app-traffic-usage.json"
	appTrafficSampleInterval  = 15 * time.Second
	routingClashAPIPort       = 17892
	moduleDirectOutboundTag   = "module-direct"
	clashManagedAggregateName = "Clash 代理"
)

type appTrafficDayUsage struct {
	RXBytes    uint64 `json:"rx_bytes"`
	TXBytes    uint64 `json:"tx_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

type appTrafficAppRecord struct {
	ID         string                        `json:"id"`
	Name       string                        `json:"name"`
	BundlePath string                        `json:"bundle_path,omitempty"`
	TotalRX    uint64                        `json:"total_rx_bytes"`
	TotalTX    uint64                        `json:"total_tx_bytes"`
	Days       map[string]appTrafficDayUsage `json:"days"`
}

type appTrafficPersistedState struct {
	Apps      map[string]*appTrafficAppRecord `json:"apps"`
	UpdatedAt time.Time                       `json:"updated_at"`
}

type appTrafficDelta struct {
	Key        string
	Name       string
	BundlePath string
	RX         uint64
	TX         uint64
}

type appTrafficCollectorScratch struct {
	lastNettop           map[int]nettopProcessCounters
	lastClashConnections map[string]clashConnectionCounters
}

type appTrafficUsageApp struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BundlePath string `json:"bundle_path,omitempty"`
	RXBytes    uint64 `json:"rx_bytes"`
	TXBytes    uint64 `json:"tx_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

type appTrafficUsageResponse struct {
	Date         string               `json:"date,omitempty"`
	From         string               `json:"from,omitempty"`
	To           string               `json:"to,omitempty"`
	Apps         []appTrafficUsageApp `json:"apps"`
	TotalRXBytes uint64               `json:"total_rx_bytes"`
	TotalTXBytes uint64               `json:"total_tx_bytes"`
	TotalBytes   uint64               `json:"total_bytes"`
	UpdatedAt    time.Time            `json:"updated_at"`
	Sampling     string               `json:"sampling,omitempty"`
}

func randomRoutingClashAPISecret() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "djonehub-clash-api"
	}
	return hex.EncodeToString(raw[:])
}

func appendRoutingClashAPIConfig(coreConfig map[string]any, secret string) {
	if strings.TrimSpace(secret) == "" {
		secret = "djonehub-clash-api"
	}
	coreConfig["experimental"] = map[string]any{
		"clash_api": map[string]any{
			"external_controller": fmt.Sprintf("127.0.0.1:%d", routingClashAPIPort),
			"secret":              secret,
		},
	}
}

func (a *app) initAppTrafficStats() {
	if a.dataDir == "" {
		return
	}
	path := filepath.Join(a.dataDir, appTrafficStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		a.appTrafficStats = appTrafficPersistedState{Apps: map[string]*appTrafficAppRecord{}}
		return
	}
	var state appTrafficPersistedState
	if json.Unmarshal(data, &state) != nil || state.Apps == nil {
		a.appTrafficStats = appTrafficPersistedState{Apps: map[string]*appTrafficAppRecord{}}
		return
	}
	a.appTrafficStats = state
}

func (a *app) saveAppTrafficStatsLocked() {
	if a.dataDir == "" {
		return
	}
	data, err := json.Marshal(a.appTrafficStats)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(a.dataDir, appTrafficStateFile), data, 0o600)
}

func (a *app) startAppTrafficCollector() {
	go func() {
		ticker := time.NewTicker(appTrafficSampleInterval)
		defer ticker.Stop()
		for range ticker.C {
			a.sampleAppTraffic()
		}
	}()
}

func (a *app) sampleAppTraffic() {
	if a.routing != nil {
		snapshot := a.routing.Snapshot()
		if snapshot.Runtime.State == "running" {
			deltas := a.sampleClashAppTraffic(snapshot)
			a.applyAppTrafficDeltas(deltas)
			return
		}
	}
	if !a.shouldSampleNettopAppTraffic() {
		return
	}
	deltas, err := a.sampleNettopAppTraffic()
	if err != nil || len(deltas) == 0 {
		return
	}
	a.applyAppTrafficDeltas(deltas)
}

func (a *app) shouldSampleNettopAppTraffic() bool {
	counters, err := discoverMacInterfaceCounters()
	if err != nil {
		return false
	}
	moduleProduct := ""
	if usbDevice := a.currentUSBDevice(); usbDevice != nil {
		moduleProduct = strings.TrimSpace(usbDevice.Product)
	}
	services, svcErr := macNetworkServiceOrder(moduleProduct)
	if svcErr != nil {
		return false
	}
	live := resolveLiveTrafficInterface(a.failoverActiveDevice(), services, counters)
	if live == "" {
		return false
	}
	interfaces := discoverMacNetworkInterfaces()
	moduleName, active := selectModuleTrafficInterface(interfaces, services)
	return active && moduleName != "" && live == moduleName
}

func (a *app) applyAppTrafficDeltas(deltas []appTrafficDelta) {
	if len(deltas) == 0 {
		return
	}
	day := time.Now().Format("2006-01-02")
	a.appTrafficMu.Lock()
	defer a.appTrafficMu.Unlock()
	if a.appTrafficStats.Apps == nil {
		a.appTrafficStats.Apps = map[string]*appTrafficAppRecord{}
	}
	for _, delta := range deltas {
		if delta.RX == 0 && delta.TX == 0 {
			continue
		}
		record := a.appTrafficStats.Apps[delta.Key]
		if record == nil {
			record = &appTrafficAppRecord{
				ID:   delta.Key,
				Name: delta.Name,
				Days: map[string]appTrafficDayUsage{},
			}
			a.appTrafficStats.Apps[delta.Key] = record
		}
		if delta.Name != "" {
			record.Name = delta.Name
		}
		if delta.BundlePath != "" {
			record.BundlePath = delta.BundlePath
		}
		record.TotalRX += delta.RX
		record.TotalTX += delta.TX
		dayUsage := record.Days[day]
		dayUsage.RXBytes += delta.RX
		dayUsage.TXBytes += delta.TX
		dayUsage.TotalBytes = dayUsage.RXBytes + dayUsage.TXBytes
		record.Days[day] = dayUsage
	}
	a.appTrafficStats.UpdatedAt = time.Now()
	a.saveAppTrafficStatsLocked()
}

func (a *app) networkAppTraffic(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	date := strings.TrimSpace(query.Get("date"))
	from := strings.TrimSpace(query.Get("from"))
	to := strings.TrimSpace(query.Get("to"))

	a.appTrafficMu.Lock()
	state := cloneAppTrafficState(a.appTrafficStats)
	a.appTrafficMu.Unlock()

	response := buildAppTrafficUsageResponse(state, date, from, to)
	writeJSON(w, http.StatusOK, response)
}

func cloneAppTrafficState(state appTrafficPersistedState) appTrafficPersistedState {
	cloned := appTrafficPersistedState{
		UpdatedAt: state.UpdatedAt,
		Apps:      make(map[string]*appTrafficAppRecord, len(state.Apps)),
	}
	for key, record := range state.Apps {
		if record == nil {
			continue
		}
		copyRecord := &appTrafficAppRecord{
			ID:         record.ID,
			Name:       record.Name,
			BundlePath: record.BundlePath,
			TotalRX:    record.TotalRX,
			TotalTX:    record.TotalTX,
			Days:       make(map[string]appTrafficDayUsage, len(record.Days)),
		}
		for day, usage := range record.Days {
			copyRecord.Days[day] = usage
		}
		cloned.Apps[key] = copyRecord
	}
	return cloned
}

func buildAppTrafficUsageResponse(state appTrafficPersistedState, date, from, to string) appTrafficUsageResponse {
	response := appTrafficUsageResponse{
		UpdatedAt: state.UpdatedAt,
		Apps:      []appTrafficUsageApp{},
	}
	if date != "" {
		response.Date = date
	}
	if from != "" {
		response.From = from
	}
	if to != "" {
		response.To = to
	}

	var days []string
	switch {
	case date != "":
		days = []string{date}
	case from != "" || to != "":
		if from == "" {
			from = to
		}
		if to == "" {
			to = from
		}
		days = enumerateAppTrafficDays(from, to)
	default:
		days = nil
	}

	usageByApp := map[string]appTrafficUsageApp{}
	for _, record := range state.Apps {
		if record == nil {
			continue
		}
		usage := appTrafficUsageApp{
			ID:         record.ID,
			Name:       record.Name,
			BundlePath: record.BundlePath,
		}
		if len(days) == 0 {
			usage.RXBytes = record.TotalRX
			usage.TXBytes = record.TotalTX
		} else {
			for _, day := range days {
				dayUsage, ok := record.Days[day]
				if !ok {
					continue
				}
				usage.RXBytes += dayUsage.RXBytes
				usage.TXBytes += dayUsage.TXBytes
			}
		}
		usage.TotalBytes = usage.RXBytes + usage.TXBytes
		if usage.TotalBytes == 0 {
			continue
		}
		usageByApp[record.ID] = usage
	}

	apps := make([]appTrafficUsageApp, 0, len(usageByApp))
	for _, usage := range usageByApp {
		apps = append(apps, usage)
		response.TotalRXBytes += usage.RXBytes
		response.TotalTXBytes += usage.TXBytes
		response.TotalBytes += usage.TotalBytes
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].TotalBytes == apps[j].TotalBytes {
			return apps[i].Name < apps[j].Name
		}
		return apps[i].TotalBytes > apps[j].TotalBytes
	})
	response.Apps = apps
	return response
}

func enumerateAppTrafficDays(from, to string) []string {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil
	}
	if end.Before(start) {
		start, end = end, start
	}
	var days []string
	for day := start; !day.After(end); day = day.Add(24 * time.Hour) {
		days = append(days, day.Format("2006-01-02"))
	}
	return days
}

func appTrafficIdentityFromProcessPath(path string) (key, name, bundlePath string) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" {
		return "", "", ""
	}
	lowerPath := strings.ToLower(path)
	if appIndex := strings.Index(lowerPath, ".app/"); appIndex >= 0 {
		bundlePath = path[:appIndex+len(".app")]
		name = filepath.Base(bundlePath)
		key = bundlePath
		return key, name, bundlePath
	}
	name = filepath.Base(path)
	return path, name, ""
}

func appTrafficIdentityFromPID(pid int) (key, name, bundlePath string) {
	if pid <= 1 {
		return "", "", ""
	}
	path, err := darwinProcessPath(pid)
	if err != nil {
		return fmt.Sprintf("pid:%d", pid), fmt.Sprintf("PID %d", pid), ""
	}
	return appTrafficIdentityFromProcessPath(path)
}

func parseNettopProcessField(field string) (name string, pid int, ok bool) {
	field = strings.TrimSpace(field)
	lastDot := strings.LastIndex(field, ".")
	if lastDot <= 0 || lastDot >= len(field)-1 {
		return "", 0, false
	}
	pid, err := strconv.Atoi(field[lastDot+1:])
	if err != nil || pid <= 1 {
		return "", 0, false
	}
	return field[:lastDot], pid, true
}

func counterDeltaUint64(previous, current uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	return current
}

func (a *app) fetchClashConnections(ctx context.Context, address, secret string) ([]clashConnectionSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/connections", nil)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clash api status %d", response.StatusCode)
	}
	var payload struct {
		Connections []clashConnectionSnapshot `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Connections, nil
}
