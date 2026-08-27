package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	networkOrderServiceLabel           = "com.djonehub.network-order"
	networkOrderServiceProtocolVersion = "3-network-order"
	networkOrderServiceDirectory       = "/Library/PrivilegedHelperTools/com.djonehub.network-order"
	networkOrderServiceHelperPath      = networkOrderServiceDirectory + "/djonehub-network-helper"
	networkOrderServiceSocketPath      = networkOrderServiceDirectory + "/control.sock"
	networkOrderServicePlistPath       = "/Library/LaunchDaemons/com.djonehub.network-order.plist"
)

func networkOrderHelperBundlePath() string {
	backendPath, _ := os.Executable()
	return filepath.Join(filepath.Dir(backendPath), "djonehub-network-helper")
}

func networkOrderServiceInstalled() bool {
	status, err := queryNetworkOrderService()
	return err == nil && status == networkOrderServiceProtocolVersion
}

func networkOrderServiceArtifactsPresent() bool {
	for _, path := range []string{networkOrderServiceDirectory, networkOrderServicePlistPath} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

func queryNetworkOrderService() (string, error) {
	response, err := sendNetworkOrderCommand("STATUS")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(response)
	if len(fields) < 1 {
		return "", fmt.Errorf("invalid status %q", response)
	}
	return fields[0], nil
}

func sendNetworkOrderCommand(command string) (string, error) {
	connection, err := net.DialTimeout("unix", networkOrderServiceSocketPath, time.Second)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprintln(connection, command); err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	n, err := connection.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}
	response := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(response, "OK") {
		msg := strings.TrimSpace(strings.TrimPrefix(response, "ERROR"))
		if msg == "" {
			msg = response
		}
		return response, errors.New(msg)
	}
	return strings.TrimSpace(strings.TrimPrefix(response, "OK")), nil
}

func ensureNetworkOrderService(ctx context.Context) error {
	if networkOrderServiceInstalled() {
		return nil
	}
	return installNetworkOrderService(ctx)
}

func installNetworkOrderService(ctx context.Context) error {
	helperPath := networkOrderHelperBundlePath()
	info, err := os.Lstat(helperPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("应用包中的网卡顺序助手不可用，请重新安装应用")
	}
	plist, err := networkOrderServicePlist()
	if err != nil {
		return err
	}
	plistBase64 := base64.StdEncoding.EncodeToString(plist)
	script := strings.Join([]string{
		"set -eu",
		"/bin/launchctl bootout system/" + networkOrderServiceLabel + " >/dev/null 2>&1 || true",
		"/bin/rm -f " + shellQuote(networkOrderServiceSocketPath),
		"/bin/mkdir -p " + shellQuote(networkOrderServiceDirectory),
		"/usr/bin/install -o root -g wheel -m 0555 " + shellQuote(helperPath) + " " + shellQuote(networkOrderServiceHelperPath),
		"/bin/chmod 0755 " + shellQuote(networkOrderServiceDirectory),
		"/usr/sbin/chown -R root:wheel " + shellQuote(networkOrderServiceDirectory),
		"/bin/echo " + shellQuote(plistBase64) + " | /usr/bin/base64 -D > " + shellQuote(networkOrderServicePlistPath),
		"/bin/chmod 0644 " + shellQuote(networkOrderServicePlistPath),
		"/usr/sbin/chown root:wheel " + shellQuote(networkOrderServicePlistPath),
		"/bin/launchctl bootstrap system " + shellQuote(networkOrderServicePlistPath),
	}, "\n")
	if err := runRoutingAdministratorScript(ctx, script); err != nil {
		return fmt.Errorf("安装网卡自动切换服务失败：%w", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if networkOrderServiceInstalled() {
			return nil
		}
		if ver, err := queryNetworkOrderService(); err != nil {
			last = err
		} else {
			last = fmt.Errorf("协议版本不匹配：%s", ver)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if last == nil {
		last = errors.New("服务未响应")
	}
	return fmt.Errorf("网卡自动切换服务安装后未能启动：%w", last)
}

func networkOrderServicePlist() ([]byte, error) {
	uid := os.Getuid()
	gid := os.Getgid()
	if uid < 0 || gid < 0 {
		return nil, errors.New("无法获取当前用户身份")
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + networkOrderServiceLabel + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + networkOrderServiceHelperPath + `</string>
        <string>-socket</string>
        <string>` + networkOrderServiceSocketPath + `</string>
        <string>-user-uid</string>
        <string>` + strconv.Itoa(uid) + `</string>
        <string>-user-gid</string>
        <string>` + strconv.Itoa(gid) + `</string>
    </array>
    <key>WorkingDirectory</key>
    <string>` + networkOrderServiceDirectory + `</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>ThrottleInterval</key>
    <integer>2</integer>
    <key>Umask</key>
    <integer>63</integer>
</dict>
</plist>
`
	return []byte(plist), nil
}

// applyNetworkServicesOrderSilent 优先通过提权助手改顺序；助手不可用时回退到 osascript（会弹密码）。
func applyNetworkServicesOrderSilent(ctx context.Context, names []string) error {
	if len(names) < 2 {
		return errors.New("至少需要两个网络服务")
	}
	payload, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if networkOrderServiceInstalled() {
		if _, err := sendNetworkOrderCommand("ORDER " + string(payload)); err != nil {
			return err
		}
		return nil
	}
	return applyNetworkServicesOrderViaOSAS(ctx, names)
}

func setNetworkServiceEnabledSilent(ctx context.Context, name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("服务名为空")
	}
	payload, err := json.Marshal(map[string]any{"name": name, "enabled": enabled})
	if err != nil {
		return err
	}
	if networkOrderServiceInstalled() {
		if _, err := sendNetworkOrderCommand("SETENABLED " + string(payload)); err != nil {
			return err
		}
		return nil
	}
	flag := "off"
	if enabled {
		flag = "on"
	}
	command := "networksetup -setnetworkserviceenabled " + shellQuote(name) + " " + flag
	b64 := base64.StdEncoding.EncodeToString([]byte(command))
	script := fmt.Sprintf("do shell script \"echo %s | base64 -d | sh\" with administrator privileges", b64)
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

func renewInterfaceDHCPSilent(device string) {
	device = strings.TrimSpace(device)
	if device == "" || !networkOrderServiceInstalled() {
		return
	}
	_, _ = sendNetworkOrderCommand("RENEW " + device)
}

func bounceInterfaceSilent(device string) {
	// 故意 no-op：对 AppleUserECM / 模块 USB 网做 ifconfig down/up 会弄死链路，
	// 只能靠模块软重启恢复。保留函数以免旧调用路径编译失败。
	device = strings.TrimSpace(device)
	if device == "" {
		return
	}
	log.Printf("network order: skip IFACE bounce on %s (unsafe for USB ECM)", device)
}

func applyNetworkServicesOrderViaOSAS(ctx context.Context, names []string) error {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, shellQuote(name))
	}
	command := "networksetup -ordernetworkservices " + strings.Join(quoted, " ")
	b64 := base64.StdEncoding.EncodeToString([]byte(command))
	script := fmt.Sprintf("do shell script \"echo %s | base64 -d | sh\" with administrator privileges", b64)
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		if strings.Contains(strings.ToLower(detail), "user canceled") || strings.Contains(detail, "-128") {
			return errors.New("已取消管理员授权")
		}
		return errors.New(detail)
	}
	return nil
}

func networkOrderServiceFilesLookValid() bool {
	info, err := os.Stat(networkOrderServiceHelperPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && info.Mode().Perm()&0o022 == 0
}
