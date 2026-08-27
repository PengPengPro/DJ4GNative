package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	networkFailoverStateFile    = "network-failover.json"
	networkFailoverInterval     = 3 * time.Second
	networkFailoverProbeTimeout = 2 * time.Second
	networkFailoverFailNeed     = 2 // 主网卡仍在线但无外网时，连续失败次数后再切
	// 软重启慢且会占用 AT 口；优先快修，软重启冷却要更长。
	cellularSoftRebootCooldown = 5 * time.Minute
	cellularFastRepairCooldown = 25 * time.Second
)

type networkFailoverState struct {
	Enabled   bool     `json:"enabled"`
	Preferred []string `json:"preferred"`
}

type networkFailoverStatus struct {
	Enabled        bool     `json:"enabled"`
	Preferred      []string `json:"preferred"`
	Current        []string `json:"current"`
	ActiveService  string   `json:"active_service,omitempty"`
	ActiveDevice   string   `json:"active_device,omitempty"`
	PathKind       string   `json:"path_kind,omitempty"`  // wifi | cellular | ethernet | vpn | unknown
	PathLabel      string   `json:"path_label,omitempty"` // 展示名
	UsingPreferred bool     `json:"using_preferred"`
	HelperReady    bool     `json:"helper_ready"`
	Message        string   `json:"message,omitempty"`
	PrimaryOnline  *bool    `json:"primary_online,omitempty"`
}

type networkFailoverController struct {
	mu sync.Mutex

	path      string
	enabled   bool
	preferred []string

	primaryFailStreak int
	lastMessage       string
	primaryOnline     *bool
	activeService     string
	activeDevice      string
	pathKind          string
	pathLabel         string

	lastCellularRepair     time.Time // 软重启时间戳
	lastCellularFastRepair time.Time // ARP/PDP/射频快修时间戳
	lastOverlayBounce      time.Time
	// 当前是否处于「已切到备选底层」模式；用于避免每轮重复弹代理 / 重复修复
	usingBackup bool

	cancel context.CancelFunc
}

func (a *app) failoverLog(format string, args ...any) {
	if a != nil && a.diagLog != nil {
		a.diagLog.Log(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (a *app) initNetworkFailover() {
	if a.dataDir == "" {
		return
	}
	ctrl := &networkFailoverController{
		path: filepath.Join(a.dataDir, networkFailoverStateFile),
	}
	ctrl.load()
	// 自动故障切换始终开启，不提供关闭入口。
	ctrl.mu.Lock()
	ctrl.enabled = true
	ctrl.mu.Unlock()
	ctrl.save()
	a.networkFailover = ctrl
	a.startNetworkFailoverLocked()
}

func (c *networkFailoverController) load() {
	data, err := os.ReadFile(c.path)
	if err != nil {
		c.enabled = true
		return
	}
	var state networkFailoverState
	if json.Unmarshal(data, &state) != nil {
		c.enabled = true
		return
	}
	c.enabled = true
	c.preferred = append([]string(nil), state.Preferred...)
}

func (c *networkFailoverController) save() {
	state := networkFailoverState{
		Enabled:   c.enabled,
		Preferred: append([]string(nil), c.preferred...),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.path, data, 0o600)
}

func (c *networkFailoverController) setPreferred(names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.preferred = append([]string(nil), names...)
	c.save()
}

func (a *app) startNetworkFailoverLocked() {
	ctrl := a.networkFailover
	if ctrl == nil {
		return
	}
	if ctrl.cancel != nil {
		ctrl.cancel()
		ctrl.cancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl.cancel = cancel
	go a.runNetworkFailover(ctx)
}

func (a *app) stopNetworkFailoverLocked() {
	ctrl := a.networkFailover
	if ctrl == nil || ctrl.cancel == nil {
		return
	}
	ctrl.cancel()
	ctrl.cancel = nil
}

func (a *app) runNetworkFailover(ctx context.Context) {
	log.Printf("network failover monitor started")
	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("network failover monitor stopped")
			return
		case <-timer.C:
			a.networkFailoverTick(ctx)
			timer.Reset(networkFailoverInterval)
		}
	}
}

func (a *app) networkFailoverTick(ctx context.Context) {
	ctrl := a.networkFailover
	if ctrl == nil {
		return
	}
	ctrl.mu.Lock()
	enabled := ctrl.enabled
	preferred := append([]string(nil), ctrl.preferred...)
	ctrl.mu.Unlock()
	if !enabled || len(preferred) < 2 {
		return
	}

	moduleProduct := ""
	if usb := a.currentUSBDevice(); usb != nil {
		moduleProduct = usb.Product
	}
	services, err := macNetworkServiceOrder(moduleProduct)
	if err != nil {
		a.setFailoverMessage("读取网卡顺序失败：" + err.Error())
		return
	}
	preferred = intersectPreferred(preferred, services)
	if len(preferred) < 2 {
		a.setFailoverMessage("首选顺序中可用网卡不足，请重新「应用顺序」")
		return
	}
	ctrl.mu.Lock()
	ctrl.preferred = preferred
	ctrl.save()
	ctrl.mu.Unlock()

	byName := map[string]networkService{}
	for _, s := range services {
		byName[s.Name] = s
	}
	primary := ""
	for _, name := range preferred {
		svc := byName[name]
		if isOverlayNetworkService(svc) {
			continue
		}
		primary = name
		break
	}
	if primary == "" {
		a.setFailoverMessage("首选顺序里没有可用的底层网卡（Wi‑Fi/4G/有线）")
		return
	}
	primarySvc := byName[primary]
	primaryReady := interfaceLinkReady(primarySvc.Device)
	primaryOK := underlayHealthy(ctx, primarySvc)
	if !primaryReady {
		primaryOK = false
	}

	ctrl.mu.Lock()
	ctrl.primaryOnline = boolPtr(primaryOK)
	if primaryOK {
		ctrl.primaryFailStreak = 0
	} else {
		ctrl.primaryFailStreak++
		// 主网卡链路已断：立刻满足切换门槛，不必再等第二次
		if !primaryReady {
			ctrl.primaryFailStreak = networkFailoverFailNeed
		}
	}
	failStreak := ctrl.primaryFailStreak
	ctrl.mu.Unlock()

	currentNames := serviceNames(services)
	if primaryOK {
		msg := "主网卡可用，保持首选顺序"
		target := underlayPreferredOrder(preferred, services)
		restored := false
		if !sameStringSlice(currentNames, target) {
			if err := applyNetworkServicesOrderSilent(ctx, target); err != nil {
				a.setFailoverMessage("切回主网卡失败：" + err.Error())
				a.failoverLog("network failover: restore preferred failed: %v", err)
				return
			}
			restored = true
			msg = "主网卡已恢复，已切回第一优先级（VPN/代理继续使用）"
			a.failoverLog("network failover: restored preferred underlay, primary=%s", primary)
		}
		ctrl.mu.Lock()
		wasBackup := ctrl.usingBackup
		ctrl.usingBackup = false
		ctrl.mu.Unlock()
		// 只在真正从备选切回时弹一次代理；平时主网卡健康不要动 Quantumult
		if restored || wasBackup {
			if bounceOverlayNetworkServices(ctx, services) {
				msg += "（已触发代理重绑底层）"
				a.noteOverlayBounce()
			}
		}
		a.setFailoverPathWithOverlay(services, primarySvc, msg)
		a.maybeMaintainCellularBackup(preferred, byName)
		return
	}

	if failStreak < networkFailoverFailNeed {
		a.setFailoverPathWithOverlay(services, primarySvc,
			fmt.Sprintf("主网卡暂不可用（%d/%d），继续观察", failStreak, networkFailoverFailNeed))
		return
	}

	if !networkOrderServiceInstalled() {
		a.setFailoverMessage("自动切换助手需更新：请重新「应用顺序」并完成一次管理员授权")
		return
	}

	// 只在底层网卡中选备选；Quantumult 等 VPN 是覆盖层，不是备选出口。
	var backup networkService
	var backupProbed bool
	for _, name := range preferred[1:] {
		svc, ok := byName[name]
		if !ok || isOverlayNetworkService(svc) || !interfaceFailoverCandidate(svc) {
			continue
		}
		// 已在备选模式且 USB 仍通时，不要每 3 秒都折腾修复
		ctrl.mu.Lock()
		alreadyBackup := ctrl.usingBackup
		ctrl.mu.Unlock()
		needRepair := looksLikeCellularService(svc) || svc.Module
		if needRepair && (!alreadyBackup || !cellularUSBPathReady(svc.Device)) {
			a.ensureCellularUnderlayAddress(svc.Device)
			if updated, err := macNetworkServiceOrder(moduleProduct); err == nil {
				services = updated
				byName = map[string]networkService{}
				for _, s := range services {
					byName[s.Name] = s
				}
				if s, ok := byName[name]; ok {
					svc = s
				}
			}
		}
		if underlayHealthy(ctx, svc) {
			backup = svc
			backupProbed = true
			break
		}
		if backup.Name == "" && interfaceLinkReady(svc.Device) {
			backup = svc
		}
	}
	if backup.Name == "" {
		detail := describeUnusableBackups(preferred[1:], byName)
		msg := "主网卡与备选底层网卡均无法使用，保持当前顺序"
		if detail != "" {
			msg = "无法切换：" + detail
		}
		if defaultRouteIsVPN(discoverMacDefaultRoute().Interface) {
			msg += "；Quantumult 需要底层先有可用 IPv4（请确认首页已打开 USB 网卡，且 4G 已注册数据）"
		}
		a.setFailoverPathWithOverlay(services, primarySvc, msg)
		log.Printf("network failover: no usable underlay backup (primary=%s): %s", primary, detail)
		return
	}

	// 若选中的备选仍无「可用 USB 上网路径」（IPv4 + 网关可达），再强拉一次后仍失败则不要假装切换成功
	if looksLikeCellularService(backup) || backup.Module {
		if !cellularUSBPathReady(backup.Device) {
			a.ensureCellularUnderlayAddress(backup.Device)
		}
		if !cellularUSBPathReady(backup.Device) {
			a.setFailoverMessage("无法用 " + backup.Name + " 上网：模块 USB 网关不可达（假活 IP / ARP 失败）。可在「调试与诊断」复制日志，或点首页重启模块")
			a.failoverLog("network failover: backup %s USB path not ready (ipv4=%v)", backup.Name, deviceHasUsableIPv4(backup.Device))
			return
		}
	}

	target := promoteUnderlayKeepingOverlay(preferred, backup.Name, services)
	promoted := false
	if !sameStringSlice(currentNames, target) {
		if err := applyNetworkServicesOrderSilent(ctx, target); err != nil {
			a.setFailoverMessage("切换备选网卡失败：" + err.Error())
			a.failoverLog("network failover: apply order failed: %v", err)
			return
		}
		promoted = true
		a.failoverLog("network failover: promoted underlay=%s probed=%v (primary=%s offline); overlay VPN untouched",
			backup.Name, backupProbed, primary)
	}

	ctrl.mu.Lock()
	wasBackup := ctrl.usingBackup
	ctrl.usingBackup = true
	ctrl.mu.Unlock()

	msg := "主网卡不可用，底层已切到：" + backup.Name
	if !backupProbed {
		msg += "（链路可用，健康探测未完全确认）"
	}
	if overlay := firstOverlayService(services); overlay.Name != "" {
		msg += "；" + overlay.Name + " 继续作为上层代理"
		// 关键：只在刚提升顺序或首次进入备选时弹一次代理。
		// 之前每个 tick 都 bounce，会把 Quantumult / USB 网关打挂。
		if promoted || !wasBackup {
			if bounceOverlayNetworkServices(ctx, services) {
				msg += "（已触发代理重绑底层）"
				a.noteOverlayBounce()
				a.failoverLog("network failover: bounced overlay once after promote to %s", backup.Name)
			}
		}
	}

	a.setFailoverPathWithOverlay(services, backup, msg)
}

func (a *app) noteOverlayBounce() {
	if a.networkFailover == nil {
		return
	}
	a.networkFailover.mu.Lock()
	a.networkFailover.lastOverlayBounce = time.Now()
	a.networkFailover.mu.Unlock()
}

func (a *app) setFailoverMessage(msg string) {
	if a.networkFailover == nil {
		return
	}
	a.networkFailover.mu.Lock()
	defer a.networkFailover.mu.Unlock()
	a.networkFailover.lastMessage = msg
}

func (a *app) setFailoverPath(service networkService, msg string) {
	a.setFailoverPathWithOverlay(nil, service, msg)
}

func (a *app) setFailoverPathWithOverlay(services []networkService, underlay networkService, msg string) {
	if a.networkFailover == nil {
		return
	}
	kind, label := classifyNetworkPath(underlay)
	route := discoverMacDefaultRoute()
	if defaultRouteIsVPN(route.Interface) {
		overlayName := ""
		if ov := firstOverlayService(services); ov.Name != "" {
			overlayName = ov.Name
		} else {
			overlayName = "VPN"
		}
		kind = "vpn"
		if underlay.Name != "" {
			label = overlayName + " · 经 " + underlay.Name
		} else {
			label = overlayName
		}
	}
	a.networkFailover.mu.Lock()
	defer a.networkFailover.mu.Unlock()
	a.networkFailover.activeService = underlay.Name
	if a.networkFailover.activeService == "" {
		a.networkFailover.activeService = label
	}
	a.networkFailover.activeDevice = underlay.Device
	if a.networkFailover.activeDevice == "" {
		a.networkFailover.activeDevice = route.Interface
	}
	a.networkFailover.pathKind = kind
	a.networkFailover.pathLabel = label
	a.networkFailover.lastMessage = msg
}

func (a *app) failoverStatusSnapshot() networkFailoverStatus {
	status := networkFailoverStatus{
		HelperReady: networkOrderServiceInstalled(),
	}
	ctrl := a.networkFailover
	if ctrl != nil {
		ctrl.mu.Lock()
		status.Enabled = true
		status.Preferred = append([]string(nil), ctrl.preferred...)
		status.Message = ctrl.lastMessage
		status.ActiveService = ctrl.activeService
		status.ActiveDevice = ctrl.activeDevice
		status.PathKind = ctrl.pathKind
		status.PathLabel = ctrl.pathLabel
		status.PrimaryOnline = cloneBoolPtr(ctrl.primaryOnline)
		ctrl.mu.Unlock()
	}

	moduleProduct := ""
	if usb := a.currentUSBDevice(); usb != nil {
		moduleProduct = usb.Product
	}
	services, err := macNetworkServiceOrder(moduleProduct)
	if err != nil {
		return status
	}
	status.Current = serviceNames(services)
	status.UsingPreferred = len(status.Preferred) > 0 && sameStringSlice(status.Current, status.Preferred)

	route := discoverMacDefaultRoute()
	underlay := firstUnderlayService(services)
	routeSvc := serviceForDevice(services, route.Interface)
	if status.ActiveService == "" {
		if underlay.Name != "" {
			status.ActiveService = underlay.Name
			status.ActiveDevice = underlay.Device
		} else {
			status.ActiveService = routeSvc.Name
			status.ActiveDevice = route.Interface
		}
	}
	if status.ActiveDevice == "" {
		status.ActiveDevice = route.Interface
	}
	if status.PathKind == "" || status.PathLabel == "" {
		display := underlay
		if display.Name == "" {
			display = routeSvc
		}
		kind, label := classifyNetworkPath(display)
		if defaultRouteIsVPN(route.Interface) {
			overlay := firstOverlayService(services)
			overlayName := overlay.Name
			if overlayName == "" {
				overlayName = "VPN"
			}
			kind = "vpn"
			if display.Name != "" {
				label = overlayName + " · 经 " + display.Name
			} else {
				label = overlayName
			}
		} else if display.Name == "" && route.Interface != "" {
			kind, label = classifyDevicePath(route.Interface)
		}
		status.PathKind = kind
		status.PathLabel = label
		if status.ActiveService == "" {
			status.ActiveService = label
		}
	}
	return status
}

// GET /api/network/failover
func (a *app) getNetworkFailover(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.failoverStatusSnapshot())
}

// PUT /api/network/failover — 始终开启；请求体 enabled 仅保留兼容，忽略 false。
func (a *app) setNetworkFailover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	_ = body.Enabled
	if a.networkFailover == nil {
		a.initNetworkFailover()
	}
	ctrl := a.networkFailover
	if ctrl == nil {
		writeError(w, http.StatusServiceUnavailable, "网卡自动切换未初始化")
		return
	}

	ctrl.mu.Lock()
	preferred := append([]string(nil), ctrl.preferred...)
	ctrl.mu.Unlock()

	if len(preferred) < 2 {
		moduleProduct := ""
		if usb := a.currentUSBDevice(); usb != nil {
			moduleProduct = usb.Product
		}
		services, err := macNetworkServiceOrder(moduleProduct)
		if err != nil || len(services) < 2 {
			writeError(w, http.StatusBadRequest, "请先在网卡优先级中排列并「应用顺序」")
			return
		}
		preferred = serviceNames(services)
	}
	// Quantumult 等代理必须排在底层网卡之后
	moduleProduct := ""
	if usb := a.currentUSBDevice(); usb != nil {
		moduleProduct = usb.Product
	}
	if services, err := macNetworkServiceOrder(moduleProduct); err == nil {
		preferred = sanitizePreferredKeepingOverlayLast(preferred, services)
	}
	ctrl.setPreferred(preferred)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := ensureNetworkOrderService(ctx); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctrl.mu.Lock()
	ctrl.enabled = true
	ctrl.primaryFailStreak = 0
	ctrl.usingBackup = false
	ctrl.lastMessage = "自动切换已开启"
	ctrl.save()
	ctrl.mu.Unlock()
	a.failoverLog("network failover: enabled preferred=%v", preferred)
	a.startNetworkFailoverLocked()
	writeJSON(w, http.StatusOK, a.failoverStatusSnapshot())
}

func serviceNames(services []networkService) []string {
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name)
	}
	return names
}

func serviceForDevice(services []networkService, device string) networkService {
	if device == "" {
		if len(services) > 0 {
			return services[0]
		}
		return networkService{}
	}
	for _, s := range services {
		if s.Device == device {
			return s
		}
	}
	// VPN 服务常无 Device 字段，但默认路由落在 utun/ipsec 上
	if strings.HasPrefix(device, "utun") || strings.HasPrefix(device, "ipsec") {
		for _, s := range services {
			if kind, _ := classifyNetworkPath(s); kind == "vpn" {
				return s
			}
		}
		return networkService{Name: device, Device: device}
	}
	if len(services) > 0 {
		return services[0]
	}
	return networkService{}
}

func describeUnusableBackups(names []string, byName map[string]networkService) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		svc, ok := byName[name]
		if !ok {
			parts = append(parts, name+" 不存在")
			continue
		}
		if svc.Device == "" {
			parts = append(parts, name+" 无网卡设备（多为 VPN，无法作为备选出口）")
			continue
		}
		iface, err := net.InterfaceByName(svc.Device)
		if err != nil {
			parts = append(parts, name+" 网卡不存在")
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			parts = append(parts, name+" 未启用")
			continue
		}
		if ipv4OfInterface(iface) == nil && ipv6OfInterface(iface) == nil {
			if hasLinkLocalIPv4(iface) {
				parts = append(parts, name+" 无有效 IP（仅 169.254）")
			} else {
				parts = append(parts, name+" 无可用地址")
			}
			continue
		}
		parts = append(parts, name+" 外网探测失败")
	}
	return strings.Join(parts, "；")
}

func hasLinkLocalIPv4(iface *net.Interface) bool {
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil && ip4.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func defaultRouteIsVPN(device string) bool {
	return strings.HasPrefix(device, "utun") || strings.HasPrefix(device, "ipsec")
}

func isOverlayNetworkService(svc networkService) bool {
	kind, _ := classifyNetworkPath(svc)
	return kind == "vpn" || svc.Device == ""
}

func firstOverlayService(services []networkService) networkService {
	for _, svc := range services {
		if isOverlayNetworkService(svc) {
			return svc
		}
	}
	return networkService{}
}

func firstUnderlayService(services []networkService) networkService {
	for _, svc := range services {
		if isOverlayNetworkService(svc) {
			continue
		}
		if svc.Device == "" {
			continue
		}
		if interfaceLinkReady(svc.Device) || interfaceFailoverCandidate(svc) {
			return svc
		}
	}
	for _, svc := range services {
		if !isOverlayNetworkService(svc) {
			return svc
		}
	}
	return networkService{}
}

// sanitizePreferredKeepingOverlayLast 保证 Quantumult 等覆盖层排在底层网卡之后。
func sanitizePreferredKeepingOverlayLast(preferred []string, services []networkService) []string {
	return underlayPreferredOrder(preferred, services)
}

// underlayPreferredOrder 保持用户首选的底层顺序，VPN/代理始终排在末尾。
func underlayPreferredOrder(preferred []string, services []networkService) []string {
	byName := map[string]networkService{}
	for _, svc := range services {
		byName[svc.Name] = svc
	}
	var underlay, overlay []string
	seen := map[string]bool{}
	for _, name := range preferred {
		svc, ok := byName[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		if isOverlayNetworkService(svc) {
			overlay = append(overlay, name)
		} else {
			underlay = append(underlay, name)
		}
	}
	for _, svc := range services {
		if seen[svc.Name] {
			continue
		}
		if isOverlayNetworkService(svc) {
			overlay = append(overlay, svc.Name)
		} else {
			underlay = append(underlay, svc.Name)
		}
	}
	return append(underlay, overlay...)
}

// promoteUnderlayKeepingOverlay 只提升底层备选，VPN/代理位置不变（始终在后）。
func promoteUnderlayKeepingOverlay(preferred []string, backup string, services []networkService) []string {
	byName := map[string]networkService{}
	for _, svc := range services {
		byName[svc.Name] = svc
	}
	var underlay, overlay []string
	seen := map[string]bool{}
	for _, name := range preferred {
		svc, ok := byName[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		if isOverlayNetworkService(svc) {
			overlay = append(overlay, name)
		} else {
			underlay = append(underlay, name)
		}
	}
	for _, svc := range services {
		if seen[svc.Name] {
			continue
		}
		if isOverlayNetworkService(svc) {
			overlay = append(overlay, svc.Name)
		} else {
			underlay = append(underlay, svc.Name)
		}
	}
	return append(promoteService(underlay, backup), overlay...)
}

func intersectPreferred(preferred []string, services []networkService) []string {
	valid := make(map[string]bool, len(services))
	for _, s := range services {
		valid[s.Name] = true
	}
	out := make([]string, 0, len(preferred))
	seen := make(map[string]bool)
	for _, name := range preferred {
		if !valid[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, s := range services {
		if seen[s.Name] {
			continue
		}
		out = append(out, s.Name)
	}
	return out
}

func promoteService(preferred []string, backup string) []string {
	out := make([]string, 0, len(preferred))
	out = append(out, backup)
	for _, name := range preferred {
		if name == backup {
			continue
		}
		out = append(out, name)
	}
	return out
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func classifyNetworkPath(service networkService) (kind, label string) {
	if service.Name == "" && service.Device == "" {
		return "unknown", "未知"
	}
	label = service.Name
	if label == "" {
		label = service.Device
	}
	if service.Module || looksLikeCellularService(service) {
		return "cellular", label
	}
	blob := strings.ToLower(strings.TrimSpace(service.Name + " " + service.Port + " " + service.Device))
	switch {
	case strings.Contains(blob, "wi-fi") || strings.Contains(blob, "wifi") || strings.Contains(blob, "airport"):
		return "wifi", label
	case strings.Contains(blob, "quantumult") || strings.Contains(blob, "vpn") ||
		strings.Contains(blob, "ipsec") || strings.HasPrefix(service.Device, "utun") ||
		strings.HasPrefix(service.Device, "ipsec"):
		return "vpn", label
	case service.USB || strings.HasPrefix(service.Device, "en") ||
		strings.Contains(blob, "ethernet") || strings.Contains(blob, "usb") ||
		strings.Contains(blob, "thunderbolt") || strings.Contains(blob, "lan"):
		return "ethernet", label
	default:
		return "unknown", label
	}
}

func classifyDevicePath(device string) (kind, label string) {
	switch {
	case device == "en0":
		return "wifi", "Wi-Fi"
	case strings.HasPrefix(device, "utun") || strings.HasPrefix(device, "ipsec"):
		return "vpn", device
	case strings.HasPrefix(device, "en"):
		return "ethernet", device
	default:
		return "unknown", device
	}
}

func boolPtr(v bool) *bool { return &v }

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func interfaceLinkReady(device string) bool {
	if device == "" {
		return false
	}
	iface, err := net.InterfaceByName(device)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return false
	}
	return ipv4OfInterface(iface) != nil || ipv6OfInterface(iface) != nil
}

// interfaceFailoverCandidate：可作为备选底层出口。
func interfaceFailoverCandidate(svc networkService) bool {
	if svc.Device == "" || isOverlayNetworkService(svc) {
		return false
	}
	iface, err := net.InterfaceByName(svc.Device)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return false
	}
	if interfaceLinkReady(svc.Device) {
		return true
	}
	// 模块/蜂窝 USB 网卡常常先落在 169.254，提升优先级后才会拿到正式地址
	if looksLikeCellularService(svc) || svc.Module || svc.USB {
		return hasLinkLocalIPv4(iface)
	}
	return false
}

func looksLikeCellularService(svc networkService) bool {
	blob := strings.ToLower(strings.TrimSpace(svc.Name + " " + svc.Port))
	for _, key := range []string{
		"dj-4g", "dj4g", "4g", "lte", "wwan", "cellular", "modem",
		"eg25", "rm500", "rm520", "quectel", "baiwang", "qdc507",
	} {
		if strings.Contains(blob, key) {
			return true
		}
	}
	return false
}

// underlayHealthy 判断底层网卡能否作为 VPN/系统上游。
// 开着 Quantumult 时不能用直连 1.1.1.1 判断；优先看 IPv4 + 网关可达。
func underlayHealthy(ctx context.Context, svc networkService) bool {
	if svc.Device == "" || isOverlayNetworkService(svc) {
		return false
	}
	iface, err := net.InterfaceByName(svc.Device)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return false
	}
	ip4 := ipv4OfInterface(iface)
	if ip4 == nil {
		// 仅有 IPv6 时：对依赖 IPv4 上游的代理通常不够
		return false
	}
	// 模块 USB 网：必须网关真实可达，否则会出现「有 192.168.225.x 但 ARP incomplete」假活
	if looksLikeCellularService(svc) || svc.Module {
		return cellularUSBPathReady(svc.Device)
	}
	gw := interfaceIPv4Gateway(svc.Device)
	if gw != nil {
		if probeTCPViaInterface(ctx, "tcp4", net.JoinHostPort(gw.String(), "53"), iface.Index, &net.TCPAddr{IP: ip4}) {
			return true
		}
		if pingViaInterface(svc.Device, gw.String()) {
			return true
		}
		// 普通 Wi‑Fi/有线：有 IPv4 + 网关路由也视为可用
		return true
	}
	if !defaultRouteIsVPN(discoverMacDefaultRoute().Interface) {
		return probeInterfaceInternet(ctx, svc.Device)
	}
	return true
}

func deviceHasIPv4(device string) bool {
	return deviceHasUsableIPv4(device)
}

func deviceHasUsableIPv4(device string) bool {
	if device == "" {
		return false
	}
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return false
	}
	return ipv4OfInterface(iface) != nil
}

// cellularUSBPathReady：Mac 侧 USB 网卡已拿到非链路本地 IPv4，且能 ping 通模块网关。
func cellularUSBPathReady(device string) bool {
	if !deviceHasUsableIPv4(device) || !interfaceMediaActive(device) {
		return false
	}
	gw := interfaceIPv4Gateway(device)
	if gw == nil {
		return false
	}
	return pingViaInterface(device, gw.String())
}

// ensureCellularUnderlayAddress 尝试让模块网卡重新拿到可用 USB 上网路径。
// 切勿对 AppleUserECM 做 ifconfig down/up。优先快修（ARP / PDP / 射频开关），软重启仅作最后手段且不阻塞 tick。
func (a *app) ensureCellularUnderlayAddress(device string) {
	a.ensureCellularUnderlayAddressOpts(device, true)
}

func (a *app) ensureCellularUnderlayAddressOpts(device string, allowSoftReboot bool) {
	if device == "" {
		return
	}
	if cellularUSBPathReady(device) {
		// 网关通但仍无外网：只做 PDP 快修，不软重启
		if !probeInterfaceInternet(context.Background(), device) {
			a.fastRepairCellularData(device)
		}
		return
	}

	a.failoverLog("network failover: %s USB path not ready (ipv4=%v media=%v), fast-repairing",
		device, deviceHasUsableIPv4(device), interfaceMediaActive(device))
	a.setFailoverMessage("正在快速修复 " + device + "（不重启模块）…")

	if a.fastRepairCellularUSB(device) {
		return
	}

	if !allowSoftReboot {
		a.setFailoverMessage("快速修复未恢复 " + device + "，主网卡仍可用，暂不软重启")
		return
	}
	if inCooldown, remaining := a.cellularSoftRebootInCooldown(); inCooldown {
		a.setFailoverMessage(fmt.Sprintf("快速修复未恢复；%.0f 秒内已软重启过，暂缓", remaining.Seconds()))
		return
	}
	// 软重启异步发出，不阻塞故障切换 tick / 诊断接口
	a.repairCellularViaModuleRebootAsync(device, "fast repair exhausted")
}

// fastRepairCellularUSB：不重启模块的快捷恢复（通常数秒到十几秒）。
func (a *app) fastRepairCellularUSB(device string) bool {
	ctrl := a.networkFailover
	if ctrl != nil {
		ctrl.mu.Lock()
		if !ctrl.lastCellularFastRepair.IsZero() && time.Since(ctrl.lastCellularFastRepair) < cellularFastRepairCooldown {
			ctrl.mu.Unlock()
			return cellularUSBPathReady(device)
		}
		ctrl.lastCellularFastRepair = time.Now()
		ctrl.mu.Unlock()
	}

	// 1) 清 ARP 假活 + 触发一次邻居发现
	if flushARPAndNudge(device) {
		a.failoverLog("network failover: %s recovered after ARP nudge", device)
		a.setFailoverMessage("已通过 ARP 刷新恢复 USB 网关")
		return true
	}

	// 2) 链路仍在但无可用 IPv4：温和 DHCP renew（勿对已有假活 IP 乱 renew）
	if interfaceMediaActive(device) && !deviceHasUsableIPv4(device) {
		renewInterfaceDHCPSilent(device)
		if waitCellularUSBPath(device, 8*time.Second) {
			a.failoverLog("network failover: %s recovered after DHCP renew", device)
			a.setFailoverMessage("已通过 DHCP 续约恢复 USB 地址")
			return true
		}
	}

	// 3) 重建 PDP（比软重启快很多）
	if a.recycleCellularPDP(device) {
		a.failoverLog("network failover: %s recovered after PDP recycle", device)
		a.setFailoverMessage("已通过重建数据承载恢复 USB 上网")
		return true
	}

	// 4) 射频开关（CFUN=0/1，不做 CFUN=1,1 整机软重启）
	if a.cycleCellularRadio(device) {
		a.failoverLog("network failover: %s recovered after radio cycle", device)
		a.setFailoverMessage("已通过射频开关恢复 USB 上网")
		return true
	}
	return false
}

func (a *app) fastRepairCellularData(device string) {
	ctrl := a.networkFailover
	if ctrl != nil {
		ctrl.mu.Lock()
		if !ctrl.lastCellularFastRepair.IsZero() && time.Since(ctrl.lastCellularFastRepair) < cellularFastRepairCooldown {
			ctrl.mu.Unlock()
			return
		}
		ctrl.lastCellularFastRepair = time.Now()
		ctrl.mu.Unlock()
	}
	a.setFailoverMessage("USB 网关可达但无外网，正在重建数据承载…")
	if a.recycleCellularPDP(device) && probeInterfaceInternet(context.Background(), device) {
		a.failoverLog("network failover: %s data path recovered after PDP recycle", device)
		a.setFailoverMessage("已重建数据承载，外网恢复")
	}
}

func flushARPAndNudge(device string) bool {
	gw := interfaceIPv4Gateway(device)
	if gw == nil {
		return false
	}
	gwStr := gw.String()
	_ = exec.Command("arp", "-d", gwStr, "ifscope", device).Run()
	_ = exec.Command("arp", "-d", gwStr).Run()
	// 触发 ARP：短 ping
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = exec.CommandContext(ctx, "ping", "-c", "1", "-W", "1000", "-b", device, gwStr).Run()
	time.Sleep(200 * time.Millisecond)
	return cellularUSBPathReady(device)
}

func (a *app) recycleCellularPDP(device string) bool {
	a.failoverLog("network failover: recycling PDP for %s", device)
	if _, err := a.runATCommand("AT+CGACT=0,1", 8*time.Second); err != nil {
		a.failoverLog("network failover: CGACT=0 failed: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	if _, err := a.runATCommand("AT+CGACT=1,1", 12*time.Second); err != nil {
		a.failoverLog("network failover: CGACT=1 failed: %v", err)
		return false
	}
	return waitCellularUSBPath(device, 10*time.Second)
}

func (a *app) cycleCellularRadio(device string) bool {
	a.failoverLog("network failover: radio cycle (CFUN 0→1) for %s", device)
	a.setFailoverMessage("正在开关射频以恢复（不整机重启）…")
	if _, err := a.runATCommand("AT+CFUN=0", 8*time.Second); err != nil {
		a.failoverLog("network failover: CFUN=0 failed: %v", err)
		return false
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := a.runATCommand("AT+CFUN=1", 12*time.Second); err != nil {
		a.failoverLog("network failover: CFUN=1 failed: %v", err)
		return false
	}
	return waitCellularUSBPath(device, 18*time.Second)
}

func (a *app) cellularSoftRebootInCooldown() (bool, time.Duration) {
	ctrl := a.networkFailover
	if ctrl == nil {
		return false, 0
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.lastCellularRepair.IsZero() {
		return false, 0
	}
	elapsed := time.Since(ctrl.lastCellularRepair)
	if elapsed >= cellularSoftRebootCooldown {
		return false, 0
	}
	return true, cellularSoftRebootCooldown - elapsed
}

// 主网卡正常时：只用快修维护蜂窝备选，绝不软重启（避免 AT 口长时间不可用）。
func (a *app) maybeMaintainCellularBackup(preferred []string, byName map[string]networkService) {
	for _, name := range preferred[1:] {
		svc, ok := byName[name]
		if !ok || isOverlayNetworkService(svc) {
			continue
		}
		if !(looksLikeCellularService(svc) || svc.Module) {
			continue
		}
		ready := cellularUSBPathReady(svc.Device)
		online := ready && probeInterfaceInternet(context.Background(), svc.Device)
		if online {
			continue
		}
		a.failoverLog("network failover: maintaining cellular backup %s (gateway=%v internet=%v)",
			svc.Name, ready, online)
		a.ensureCellularUnderlayAddressOpts(svc.Device, false)
		return
	}
}

func (a *app) repairCellularViaModuleRebootAsync(device, reason string) {
	ctrl := a.networkFailover
	if ctrl == nil {
		return
	}
	ctrl.mu.Lock()
	if !ctrl.lastCellularRepair.IsZero() && time.Since(ctrl.lastCellularRepair) < cellularSoftRebootCooldown {
		remaining := cellularSoftRebootCooldown - time.Since(ctrl.lastCellularRepair)
		ctrl.mu.Unlock()
		a.setFailoverMessage(fmt.Sprintf("模块 USB 仍不可用（%s）；%.0f 秒内已软重启过，暂缓", reason, remaining.Seconds()))
		return
	}
	ctrl.lastCellularRepair = time.Now()
	ctrl.mu.Unlock()

	a.setFailoverMessage("快修无效，正在软重启模块（后台进行，不阻塞）…")
	a.failoverLog("network failover: module soft reboot async for %s (%s)", device, reason)
	go func() {
		if _, err := a.runATCommand("AT+CFUN=1,1", 5*time.Second); err != nil {
			a.failoverLog("network failover: module reboot failed: %v", err)
			a.setFailoverMessage("模块软重启失败：" + err.Error())
			return
		}
		if waitCellularUSBPath(device, 45*time.Second) {
			a.setFailoverMessage("模块软重启完成，USB 路径已恢复")
			a.failoverLog("network failover: soft reboot recovered %s", device)
			return
		}
		a.setFailoverMessage("模块软重启后 USB 仍未恢复，可尝试手动插拔或检查 SIM 数据")
		a.failoverLog("network failover: soft reboot did not recover %s", device)
	}()
}

func (a *app) repairCellularViaModuleReboot(device, reason string) bool {
	a.repairCellularViaModuleRebootAsync(device, reason)
	return true
}

func waitCellularUSBPath(device string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cellularUSBPathReady(device) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return cellularUSBPathReady(device)
}

// interfaceMediaActive：ifconfig 显示 status: active（FlagUp 不足以区分 ECM 假活）。
func interfaceMediaActive(device string) bool {
	device = strings.TrimSpace(device)
	if device == "" {
		return false
	}
	out, err := exec.Command("ifconfig", device).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "status: active")
}

func isLinkLocalOnlyIPv4(device string) bool {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return false
	}
	return ipv4OfInterface(iface) == nil && hasLinkLocalIPv4(iface)
}

func waitUsableIPv4(device string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if deviceHasIPv4(device) && !isLinkLocalOnlyIPv4(device) {
			return true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return deviceHasIPv4(device) && !isLinkLocalOnlyIPv4(device)
}

func waitDeviceIPv4(device string, budget time.Duration) bool {
	return waitUsableIPv4(device, budget)
}

// bounceOverlayNetworkServices 短暂开关 VPN 网络服务，促使代理重选底层出口。
func bounceOverlayNetworkServices(ctx context.Context, services []networkService) bool {
	if !networkOrderServiceInstalled() {
		return false
	}
	var bounced bool
	for _, svc := range services {
		if !isOverlayNetworkService(svc) {
			continue
		}
		if err := setNetworkServiceEnabledSilent(ctx, svc.Name, false); err != nil {
			log.Printf("network failover: bounce disable %s failed: %v", svc.Name, err)
			continue
		}
		time.Sleep(600 * time.Millisecond)
		if err := setNetworkServiceEnabledSilent(ctx, svc.Name, true); err != nil {
			log.Printf("network failover: bounce enable %s failed: %v", svc.Name, err)
			continue
		}
		log.Printf("network failover: bounced overlay service %s", svc.Name)
		bounced = true
	}
	if bounced {
		time.Sleep(500 * time.Millisecond)
	}
	return bounced
}

func interfaceIPv4Gateway(device string) net.IP {
	device = strings.TrimSpace(device)
	if device == "" {
		return nil
	}
	out, err := exec.Command("route", "-n", "get", "-ifscope", device, "default").Output()
	if err != nil {
		// ipconfig getoption <dev> router
		if opt, err2 := exec.Command("ipconfig", "getoption", device, "router").Output(); err2 == nil {
			ip := net.ParseIP(strings.TrimSpace(string(opt)))
			if ip4 := ip.To4(); ip4 != nil {
				return ip4
			}
		}
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			ip := net.ParseIP(strings.TrimSpace(strings.TrimPrefix(line, "gateway:")))
			if ip4 := ip.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}

func pingViaInterface(device, gateway string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-t", "2", "-b", device, gateway)
	return cmd.Run() == nil
}

var networkFailoverProbeTargets4 = []string{
	"1.1.1.1:443",
	"8.8.8.8:443",
	"9.9.9.9:53",
}

var networkFailoverProbeTargets6 = []string{
	"[2606:4700:4700::1111]:443",
	"[2001:4860:4860::8888]:443",
}

// probeInterfaceInternet 绑定到指定网卡，用公网 IP 做 TCP 探测（不依赖 DNS）。
func probeInterfaceInternet(ctx context.Context, device string) bool {
	if device == "" {
		return false
	}
	iface, err := net.InterfaceByName(device)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return false
	}
	if ip4 := ipv4OfInterface(iface); ip4 != nil {
		for _, target := range networkFailoverProbeTargets4 {
			if probeTCPViaInterface(ctx, "tcp4", target, iface.Index, &net.TCPAddr{IP: ip4}) {
				return true
			}
		}
	}
	if ip6 := ipv6OfInterface(iface); ip6 != nil {
		for _, target := range networkFailoverProbeTargets6 {
			if probeTCPViaInterface(ctx, "tcp6", target, iface.Index, &net.TCPAddr{IP: ip6}) {
				return true
			}
		}
	}
	return false
}

func ipv4OfInterface(iface *net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsLinkLocalUnicast() {
			return ip4
		}
	}
	return nil
}

func ipv6OfInterface(iface *net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.To4() != nil {
			continue
		}
		if ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsMulticast() {
			continue
		}
		return ip
	}
	return nil
}

func probeTCPViaInterface(ctx context.Context, network, address string, ifIndex int, local *net.TCPAddr) bool {
	probeCtx, cancel := context.WithTimeout(ctx, networkFailoverProbeTimeout)
	defer cancel()

	dialer := &net.Dialer{
		Timeout:   networkFailoverProbeTimeout,
		LocalAddr: local,
		Control: func(network, address string, c syscall.RawConn) error {
			var ctrlErr error
			if err := c.Control(func(fd uintptr) {
				if strings.Contains(network, "6") {
					ctrlErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifIndex)
				} else {
					ctrlErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifIndex)
				}
			}); err != nil {
				return err
			}
			return ctrlErr
		},
	}
	conn, err := dialer.DialContext(probeCtx, network, address)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
