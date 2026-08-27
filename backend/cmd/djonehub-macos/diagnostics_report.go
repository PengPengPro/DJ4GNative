package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// GET /api/diagnostics/report — 纯文本诊断包，前端一键复制。
func (a *app) diagnosticsReport(w http.ResponseWriter, _ *http.Request) {
	report := a.buildDiagnosticsReport()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(report))
}

// POST /api/diagnostics/clear — 清空诊断日志（内存环 + 文件）。
func (a *app) clearDiagnosticsLog(w http.ResponseWriter, _ *http.Request) {
	if a.diagLog != nil {
		a.diagLog.Clear()
		a.diagLog.Log("diagnostic log cleared by user")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "诊断日志已清空"})
}

func (a *app) buildDiagnosticsReport() string {
	var b strings.Builder
	fmt.Fprintf(&b, "DJOneHub 诊断报告\n")
	fmt.Fprintf(&b, "生成时间: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "========================================\n\n")

	fmt.Fprintf(&b, "## 自动故障切换\n")
	if a.networkFailover != nil {
		st := a.failoverStatusSnapshot()
		fmt.Fprintf(&b, "enabled=%v helper_ready=%v\n", st.Enabled, st.HelperReady)
		fmt.Fprintf(&b, "preferred=%v\n", st.Preferred)
		fmt.Fprintf(&b, "current=%v\n", st.Current)
		fmt.Fprintf(&b, "active=%s (%s) path=%s / %s\n", st.ActiveService, st.ActiveDevice, st.PathKind, st.PathLabel)
		fmt.Fprintf(&b, "message=%s\n", st.Message)
		if st.PrimaryOnline != nil {
			fmt.Fprintf(&b, "primary_online=%v\n", *st.PrimaryOnline)
		}
	} else {
		fmt.Fprintf(&b, "(未初始化)\n")
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "## networksetup 服务顺序\n")
	if out, err := exec.Command("networksetup", "-listnetworkserviceorder").CombinedOutput(); err != nil {
		fmt.Fprintf(&b, "error: %v\n%s\n", err, out)
	} else {
		b.Write(out)
		if !strings.HasSuffix(string(out), "\n") {
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "## 默认路由\n")
	if out, err := exec.Command("route", "-n", "get", "default").CombinedOutput(); err != nil {
		fmt.Fprintf(&b, "error: %v\n%s\n", err, out)
	} else {
		b.Write(out)
	}
	fmt.Fprintf(&b, "\n")

	moduleProduct := ""
	if usb := a.currentUSBDevice(); usb != nil {
		moduleProduct = usb.Product
	}
	cellularDev := ""
	if services, err := macNetworkServiceOrder(moduleProduct); err == nil {
		for _, s := range services {
			if looksLikeCellularService(s) || s.Module {
				cellularDev = s.Device
				fmt.Fprintf(&b, "## 模块网卡服务 %s → %s\n", s.Name, s.Device)
				break
			}
		}
	}
	if cellularDev != "" {
		fmt.Fprintf(&b, "### ifconfig %s\n", cellularDev)
		if out, err := exec.Command("ifconfig", cellularDev).CombinedOutput(); err != nil {
			fmt.Fprintf(&b, "error: %v\n%s\n", err, out)
		} else {
			b.Write(out)
		}
		fmt.Fprintf(&b, "\n### gateway check\n")
		gw := interfaceIPv4Gateway(cellularDev)
		fmt.Fprintf(&b, "ipv4_usable=%v gateway=%v media_active=%v\n",
			deviceHasUsableIPv4(cellularDev), gw, interfaceMediaActive(cellularDev))
		if gw != nil {
			fmt.Fprintf(&b, "ping_gateway=%v\n", pingViaInterface(cellularDev, gw.String()))
		}
		fmt.Fprintf(&b, "internet_probe=%v\n", probeInterfaceInternet(context.Background(), cellularDev))
		fmt.Fprintf(&b, "\n### arp (225)\n")
		if out, err := exec.Command("arp", "-an").CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "192.168.225") || strings.Contains(line, cellularDev) {
					fmt.Fprintf(&b, "%s\n", line)
				}
			}
		}
		fmt.Fprintf(&b, "\n")
	}

	fmt.Fprintf(&b, "## 模块 AT 摘要\n")
	skipAT := false
	if a.networkFailover != nil {
		a.networkFailover.mu.Lock()
		if !a.networkFailover.lastCellularRepair.IsZero() &&
			time.Since(a.networkFailover.lastCellularRepair) < 40*time.Second {
			skipAT = true
		}
		a.networkFailover.mu.Unlock()
	}
	if skipAT {
		fmt.Fprintf(&b, "(模块刚软重启，跳过 AT 以免超时)\n")
	} else {
		for _, cmd := range []string{`AT+CGACT?`, `AT+CGPADDR=1`, `AT+COPS?`, `AT+CSQ`, `AT+QCFG="usbnet"`} {
			resp, err := a.runATCommand(cmd, 2*time.Second)
			if err != nil {
				fmt.Fprintf(&b, "%s => ERROR %v\n", cmd, err)
			} else {
				fmt.Fprintf(&b, "%s => %s\n", cmd, strings.ReplaceAll(strings.TrimSpace(resp), "\r", ""))
			}
		}
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "## 最近运行日志（failover / 网络）\n")
	if a.diagLog != nil {
		recent := a.diagLog.Recent(200)
		if recent == "" {
			fmt.Fprintf(&b, "(暂无)\n")
		} else {
			b.WriteString(recent)
			b.WriteByte('\n')
		}
	} else {
		fmt.Fprintf(&b, "(日志未启用)\n")
	}
	fmt.Fprintf(&b, "\n## 结束\n")
	return b.String()
}
