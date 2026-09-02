package main

import (
	"context"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

type nettopProcessCounters struct {
	RX uint64
	TX uint64
}

type clashConnectionCounters struct {
	RX uint64
	TX uint64
}

type clashConnectionSnapshot struct {
	ID       string   `json:"id"`
	Upload   uint64   `json:"upload"`
	Download uint64   `json:"download"`
	Chains   []string `json:"chains"`
	Metadata struct {
		ProcessPath string `json:"processPath"`
	} `json:"metadata"`
}

func (a *app) sampleNettopAppTraffic() ([]appTrafficDelta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		ctx,
		"/usr/bin/nettop",
		"-P",
		"-L", "1",
		"-x",
		"-J", "bytes_in,bytes_out",
	).CombinedOutput()
	if err != nil {
		return nil, err
	}

	current := parseNettopProcessCounters(string(output))
	a.appTrafficMu.Lock()
	defer a.appTrafficMu.Unlock()
	if a.appTrafficCollector.lastNettop == nil {
		a.appTrafficCollector.lastNettop = map[int]nettopProcessCounters{}
	}

	deltas := make([]appTrafficDelta, 0, len(current))
	for pid, counters := range current {
		previous, seen := a.appTrafficCollector.lastNettop[pid]
		a.appTrafficCollector.lastNettop[pid] = counters
		if !seen {
			continue
		}
		rx := counterDeltaUint64(previous.RX, counters.RX)
		tx := counterDeltaUint64(previous.TX, counters.TX)
		if rx == 0 && tx == 0 {
			continue
		}
		key, name, bundlePath := appTrafficIdentityFromPID(pid)
		if key == "" {
			continue
		}
		deltas = append(deltas, appTrafficDelta{
			Key:        key,
			Name:       name,
			BundlePath: bundlePath,
			RX:         rx,
			TX:         tx,
		})
	}
	return deltas, nil
}

func parseNettopProcessCounters(output string) map[int]nettopProcessCounters {
	lines := strings.Split(output, "\n")
	if len(lines) < 2 {
		return nil
	}
	result := map[int]nettopProcessCounters{}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 5 {
			continue
		}
		_, pid, ok := parseNettopProcessField(fields[1])
		if !ok {
			continue
		}
		rx, err := strconv.ParseUint(strings.TrimSpace(fields[4]), 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(strings.TrimSpace(fields[5]), 10, 64)
		if err != nil {
			continue
		}
		result[pid] = nettopProcessCounters{RX: rx, TX: tx}
	}
	return result
}

func (a *app) sampleClashAppTraffic(snapshot routingSnapshot) []appTrafficDelta {
	address, secret, ok := a.routing.clashAPIEndpoint()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	connections, err := a.fetchClashConnections(ctx, address, secret)
	if err != nil {
		return nil
	}

	countModuleOnly := snapshot.Config.Mode == routingModeIndependent
	current := map[string]clashConnectionCounters{}
	identities := map[string]clashConnectionSnapshot{}

	for _, connection := range connections {
		if connection.ID == "" {
			continue
		}
		if countModuleOnly && !slices.Contains(connection.Chains, moduleDirectOutboundTag) {
			continue
		}
		current[connection.ID] = clashConnectionCounters{
			RX: connection.Download,
			TX: connection.Upload,
		}
		identities[connection.ID] = connection
	}

	a.appTrafficMu.Lock()
	defer a.appTrafficMu.Unlock()
	if a.appTrafficCollector.lastClashConnections == nil {
		a.appTrafficCollector.lastClashConnections = map[string]clashConnectionCounters{}
	}

	deltas := make([]appTrafficDelta, 0, len(current))
	seen := map[string]struct{}{}
	for id, counters := range current {
		seen[id] = struct{}{}
		previous, hadPrevious := a.appTrafficCollector.lastClashConnections[id]
		a.appTrafficCollector.lastClashConnections[id] = counters
		if !hadPrevious {
			continue
		}
		appendClashDelta(&deltas, identities[id], snapshot, counterDeltaUint64(previous.RX, counters.RX), counterDeltaUint64(previous.TX, counters.TX))
	}

	for id := range a.appTrafficCollector.lastClashConnections {
		if _, exists := seen[id]; exists {
			continue
		}
		delete(a.appTrafficCollector.lastClashConnections, id)
	}
	return deltas
}

func appendClashDelta(deltas *[]appTrafficDelta, connection clashConnectionSnapshot, snapshot routingSnapshot, rx, tx uint64) {
	if rx == 0 && tx == 0 {
		return
	}
	key, name, bundlePath := clashConnectionIdentity(connection, snapshot)
	if key == "" {
		return
	}
	*deltas = append(*deltas, appTrafficDelta{
		Key:        key,
		Name:       name,
		BundlePath: bundlePath,
		RX:         rx,
		TX:         tx,
	})
}

func clashConnectionIdentity(connection clashConnectionSnapshot, snapshot routingSnapshot) (key, name, bundlePath string) {
	processPath := strings.TrimSpace(connection.Metadata.ProcessPath)
	if processPath != "" {
		return appTrafficIdentityFromProcessPath(processPath)
	}
	if snapshot.Config.Mode == routingModeClash {
		return "clash-managed", clashManagedAggregateName, ""
	}
	return "", "", ""
}

func (m *routingManager) clashAPIEndpoint() (address string, secret string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtime.State != "running" {
		return "", "", false
	}
	return "127.0.0.1:" + strconv.Itoa(routingClashAPIPort), m.clashAPISecret, true
}
