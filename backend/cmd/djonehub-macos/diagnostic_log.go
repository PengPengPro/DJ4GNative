package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	diagnosticLogFileName = "djonehub-diagnostic.log"
	diagnosticLogMaxBytes = 512 * 1024
	diagnosticLogRingSize = 400
)

// diagnosticLogger 同时写内存环与文件，便于一键复制发给调试。
type diagnosticLogger struct {
	mu       sync.Mutex
	path     string
	ring     []string
	ringNext int
	ringLen  int
}

func newDiagnosticLogger(dataDir string) *diagnosticLogger {
	if dataDir == "" {
		return &diagnosticLogger{}
	}
	return &diagnosticLogger{
		path: filepath.Join(dataDir, diagnosticLogFileName),
		ring: make([]string, diagnosticLogRingSize),
	}
}

func (l *diagnosticLogger) Log(format string, args ...any) {
	if l == nil {
		log.Printf(format, args...)
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg
	log.Print(msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) > 0 {
		l.ring[l.ringNext] = line
		l.ringNext = (l.ringNext + 1) % len(l.ring)
		if l.ringLen < len(l.ring) {
			l.ringLen++
		}
	}
	if l.path == "" {
		return
	}
	_ = appendDiagnosticLogFile(l.path, line+"\n")
}

func (l *diagnosticLogger) Recent(limit int) string {
	if l == nil {
		return ""
	}
	if limit <= 0 {
		limit = diagnosticLogRingSize
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ringLen == 0 {
		// 环为空时尝试读文件尾部
		if l.path != "" {
			if data, err := os.ReadFile(l.path); err == nil {
				return trimLogTail(string(data), limit)
			}
		}
		return ""
	}
	out := make([]string, 0, l.ringLen)
	start := l.ringNext - l.ringLen
	if start < 0 {
		start += len(l.ring)
	}
	for i := 0; i < l.ringLen; i++ {
		out = append(out, l.ring[(start+i)%len(l.ring)])
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return strings.Join(out, "\n")
}

func (l *diagnosticLogger) Clear() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) > 0 {
		for i := range l.ring {
			l.ring[i] = ""
		}
	}
	l.ringNext = 0
	l.ringLen = 0
	if l.path != "" {
		_ = os.WriteFile(l.path, nil, 0o600)
	}
}

func appendDiagnosticLogFile(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || info.Size() < diagnosticLogMaxBytes {
		return nil
	}
	_ = f.Close()
	// 简单截断：保留后半
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	keep := data[len(data)/2:]
	if idx := strings.IndexByte(string(keep), '\n'); idx >= 0 && idx+1 < len(keep) {
		keep = keep[idx+1:]
	}
	return os.WriteFile(path, keep, 0o600)
}

func trimLogTail(data string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n")
}
