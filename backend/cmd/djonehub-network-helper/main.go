// djonehub-network-helper：提权守护进程，静默调整网络服务顺序/启停。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const serviceProtocolVersion = "3-network-order"

type options struct {
	SocketPath string
	UserUID    int
	UserGID    int
}

func main() {
	opts := options{UserUID: -1, UserGID: -1}
	flag.StringVar(&opts.SocketPath, "socket", "", "control socket path")
	flag.IntVar(&opts.UserUID, "user-uid", -1, "owning user uid")
	flag.IntVar(&opts.UserGID, "user-gid", -1, "owning user gid")
	flag.Parse()

	if err := run(opts); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "DJOneHub network helper:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if os.Geteuid() != 0 {
		return errors.New("administrator privileges are required")
	}
	if opts.UserUID < 0 || opts.UserGID < 0 {
		return errors.New("invalid owning user metadata")
	}
	if !filepath.IsAbs(opts.SocketPath) {
		return errors.New("socket path must be absolute")
	}
	dir := filepath.Dir(opts.SocketPath)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("protected directory is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return errors.New("protected directory ownership or permissions are unsafe")
	}

	_ = os.Remove(opts.SocketPath)
	listener, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(opts.SocketPath)
	}()
	if err := os.Chmod(opts.SocketPath, 0o600); err != nil {
		return err
	}
	if err := os.Chown(opts.SocketPath, opts.UserUID, opts.UserGID); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		_ = listener.Close()
	}()

	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if errors.Is(acceptErr, net.ErrClosed) {
				return nil
			}
			return acceptErr
		}
		go serve(conn)
	}
}

func serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		_, _ = fmt.Fprintln(conn, "ERROR invalid request")
		return
	}
	line = strings.TrimSpace(line)
	switch {
	case line == "STATUS":
		_, _ = fmt.Fprintf(conn, "OK %s running\n", serviceProtocolVersion)
	case strings.HasPrefix(line, "ORDER "):
		var names []string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "ORDER ")), &names); err != nil {
			_, _ = fmt.Fprintln(conn, "ERROR invalid order payload")
			return
		}
		if err := applyOrder(names); err != nil {
			_, _ = fmt.Fprintf(conn, "ERROR %s\n", err.Error())
			return
		}
		_, _ = fmt.Fprintln(conn, "OK")
	case strings.HasPrefix(line, "SETENABLED "):
		var body struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "SETENABLED ")), &body); err != nil {
			_, _ = fmt.Fprintln(conn, "ERROR invalid setenabled payload")
			return
		}
		if err := setServiceEnabled(body.Name, body.Enabled); err != nil {
			_, _ = fmt.Fprintf(conn, "ERROR %s\n", err.Error())
			return
		}
		_, _ = fmt.Fprintln(conn, "OK")
	case strings.HasPrefix(line, "RENEW "):
		device := strings.TrimSpace(strings.TrimPrefix(line, "RENEW "))
		if err := renewDHCP(device); err != nil {
			_, _ = fmt.Fprintf(conn, "ERROR %s\n", err.Error())
			return
		}
		_, _ = fmt.Fprintln(conn, "OK")
	case strings.HasPrefix(line, "IFACE "):
		var body struct {
			Device string `json:"device"`
			Action string `json:"action"` // down | up | bounce
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "IFACE ")), &body); err != nil {
			_, _ = fmt.Fprintln(conn, "ERROR invalid iface payload")
			return
		}
		if err := ifaceAction(body.Device, body.Action); err != nil {
			_, _ = fmt.Fprintf(conn, "ERROR %s\n", err.Error())
			return
		}
		_, _ = fmt.Fprintln(conn, "OK")
	default:
		_, _ = fmt.Fprintln(conn, "ERROR unknown command")
	}
}

func applyOrder(names []string) error {
	if len(names) < 2 {
		return errors.New("至少需要两个网络服务")
	}
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	valid := currentServiceNames(string(out))
	cleaned := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || !valid[name] {
			return fmt.Errorf("无效服务：%s", name)
		}
		if seen[name] {
			return fmt.Errorf("重复服务：%s", name)
		}
		seen[name] = true
		cleaned = append(cleaned, name)
	}
	args := append([]string{"-ordernetworkservices"}, cleaned...)
	cmdOut, err := exec.Command("networksetup", args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(cmdOut))
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

func setServiceEnabled(name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("服务名为空")
	}
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return err
	}
	if !currentServiceNames(string(out))[name] {
		return fmt.Errorf("无效服务：%s", name)
	}
	flag := "off"
	if enabled {
		flag = "on"
	}
	cmdOut, err := exec.Command("networksetup", "-setnetworkserviceenabled", name, flag).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(cmdOut))
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

func renewDHCP(device string) error {
	device = strings.TrimSpace(device)
	if device == "" || strings.ContainsAny(device, " \t\n/;|&") {
		return errors.New("无效网卡名")
	}
	cmdOut, err := exec.Command("ipconfig", "set", device, "DHCP").CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(cmdOut))
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

func ifaceAction(device, action string) error {
	device = strings.TrimSpace(device)
	action = strings.ToLower(strings.TrimSpace(action))
	if device == "" || strings.ContainsAny(device, " \t\n/;|&") {
		return errors.New("无效网卡名")
	}
	run := func(args ...string) error {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			detail := strings.TrimSpace(string(out))
			if detail == "" {
				detail = err.Error()
			}
			return errors.New(detail)
		}
		return nil
	}
	switch action {
	case "down":
		return run("ifconfig", device, "down")
	case "up":
		if err := run("ifconfig", device, "up"); err != nil {
			return err
		}
		time.Sleep(400 * time.Millisecond)
		_ = renewDHCP(device)
		return nil
	case "bounce":
		_ = run("ifconfig", device, "down")
		time.Sleep(500 * time.Millisecond)
		if err := run("ifconfig", device, "up"); err != nil {
			return err
		}
		time.Sleep(700 * time.Millisecond)
		_ = renewDHCP(device)
		return nil
	default:
		return fmt.Errorf("未知 action：%s", action)
	}
}

func currentServiceNames(out string) map[string]bool {
	nameRe := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "(") {
			continue
		}
		idx := strings.Index(line, ")")
		if idx < 0 || idx+1 >= len(line) {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[idx+1:]), "*"))
		if name != "" {
			nameRe[name] = true
		}
	}
	return nameRe
}
