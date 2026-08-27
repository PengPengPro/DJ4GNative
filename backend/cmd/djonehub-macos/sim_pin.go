package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/modem"
)

const simPINFileName = "sim-pins.json"

var simPINDigitsPattern = regexp.MustCompile(`^\d{4,8}$`)

// simPINFile is the on-disk map of module/SIM keys to saved PIN codes.
type simPINFile struct {
	Pins map[string]string `json:"pins"`
}

type simPINState struct {
	Raw          string // e.g. READY / SIM PIN / SIM PUK / NOT INSERTED
	Normalized   string // ready / sim_pin / sim_puk / not_inserted / unknown
	Inserted     bool
	PinRequired  bool
	PukRequired  bool
}

func parseCPINState(resp string) simPINState {
	upper := strings.ToUpper(resp)
	raw := ""
	for _, line := range splitATLines(resp) {
		lineUp := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(lineUp, "+CPIN:") {
			raw = strings.TrimSpace(line[len("+CPIN:"):])
			raw = strings.Trim(raw, "\"")
			break
		}
	}
	if raw == "" {
		// Fall back to whole response scan for compact stubs / tests.
		switch {
		case strings.Contains(upper, "NOT INSERTED"):
			raw = "NOT INSERTED"
		case strings.Contains(upper, "SIM PUK"):
			raw = "SIM PUK"
		case strings.Contains(upper, "SIM PIN"):
			raw = "SIM PIN"
		case strings.Contains(upper, "READY"):
			raw = "READY"
		}
	}
	rawUp := strings.ToUpper(strings.TrimSpace(raw))
	state := simPINState{Raw: rawUp}
	switch {
	case rawUp == "READY":
		state.Normalized = "ready"
		state.Inserted = true
	case strings.HasPrefix(rawUp, "SIM PUK"):
		state.Normalized = "sim_puk"
		state.Inserted = true
		state.PukRequired = true
	case strings.HasPrefix(rawUp, "SIM PIN"):
		state.Normalized = "sim_pin"
		state.Inserted = true
		state.PinRequired = true
	case strings.Contains(rawUp, "NOT INSERTED"):
		state.Normalized = "not_inserted"
	case rawUp == "":
		state.Normalized = "unknown"
	default:
		state.Normalized = "unknown"
		// Unknown CPIN replies usually still mean a card is present.
		state.Inserted = true
	}
	return state
}

func validateSIMPIN(pin string) error {
	pin = strings.TrimSpace(pin)
	if !simPINDigitsPattern.MatchString(pin) {
		return fmt.Errorf("PIN 须为 4–8 位数字")
	}
	return nil
}

func simPINKeyIMEI(imei string) string {
	imei = strings.TrimSpace(imei)
	if imei == "" {
		return ""
	}
	return "imei:" + imei
}

func simPINKeyICCID(iccid string) string {
	iccid = strings.TrimSpace(iccid)
	if iccid == "" {
		return ""
	}
	return "iccid:" + iccid
}

func (a *app) simPINPath() string {
	if a.dataDir == "" {
		return ""
	}
	return filepath.Join(a.dataDir, simPINFileName)
}

func (a *app) ensureSIMPINStore() {
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
}

func (a *app) ensureSIMPINStoreLocked() {
	if a.simPINPins != nil {
		return
	}
	a.simPINPins = make(map[string]string)
	a.simPINAutoFailed = make(map[string]bool)
	path := a.simPINPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file simPINFile
	if json.Unmarshal(data, &file) != nil || file.Pins == nil {
		return
	}
	a.simPINPins = file.Pins
}

func (a *app) persistSIMPINStoreLocked() error {
	path := a.simPINPath()
	if path == "" {
		return fmt.Errorf("data directory unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload := simPINFile{Pins: a.simPINPins}
	if payload.Pins == nil {
		payload.Pins = map[string]string{}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (a *app) lookupSavedSIMPIN(imei, iccid string) (pin, key string) {
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
	for _, candidate := range []string{simPINKeyICCID(iccid), simPINKeyIMEI(imei)} {
		if candidate == "" {
			continue
		}
		if saved := strings.TrimSpace(a.simPINPins[candidate]); saved != "" {
			return saved, candidate
		}
	}
	return "", ""
}

func (a *app) hasSavedSIMPIN(imei, iccid string) bool {
	pin, _ := a.lookupSavedSIMPIN(imei, iccid)
	return pin != ""
}

func (a *app) saveSIMPIN(imei, iccid, pin string) error {
	if err := validateSIMPIN(pin); err != nil {
		return err
	}
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
	if a.simPINPins == nil {
		a.simPINPins = make(map[string]string)
	}
	keys := []string{simPINKeyIMEI(imei), simPINKeyICCID(iccid)}
	wrote := false
	for _, key := range keys {
		if key == "" {
			continue
		}
		a.simPINPins[key] = pin
		delete(a.simPINAutoFailed, key)
		wrote = true
	}
	if !wrote {
		// No stable identity yet — keep a fallback for the current module session.
		a.simPINPins["default"] = pin
		delete(a.simPINAutoFailed, "default")
	}
	return a.persistSIMPINStoreLocked()
}

func (a *app) clearSavedSIMPIN(imei, iccid string) error {
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
	changed := false
	for _, key := range []string{simPINKeyICCID(iccid), simPINKeyIMEI(imei), "default"} {
		if key == "" {
			continue
		}
		if _, ok := a.simPINPins[key]; ok {
			delete(a.simPINPins, key)
			changed = true
		}
		delete(a.simPINAutoFailed, key)
	}
	if !changed {
		return nil
	}
	return a.persistSIMPINStoreLocked()
}

func (a *app) markSIMPINAutoFailed(key string) {
	if key == "" {
		return
	}
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
	if a.simPINAutoFailed == nil {
		a.simPINAutoFailed = make(map[string]bool)
	}
	a.simPINAutoFailed[key] = true
}

func (a *app) isSIMPINAutoFailed(key string) bool {
	if key == "" {
		return false
	}
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
	return a.simPINAutoFailed[key]
}

func applySIMPINFields(status *modem.DeviceStatus, state simPINState, saved bool) {
	if status == nil {
		return
	}
	status.SimPinState = state.Normalized
	status.SimPinRequired = state.PinRequired
	status.SimPinSaved = saved
	if state.Normalized != "unknown" {
		status.SimInserted = state.Inserted
	}
}

func (a *app) enterSIMPIN(pin string) (string, error) {
	if err := validateSIMPIN(pin); err != nil {
		return "", err
	}
	a.simPINCommandMu.Lock()
	defer a.simPINCommandMu.Unlock()
	cmd := fmt.Sprintf(`AT+CPIN="%s"`, pin)
	resp, err := a.runSensitiveATCommand(cmd, `AT+CPIN="<redacted>"`, 8*time.Second)
	if err != nil {
		return resp, err
	}
	upper := strings.ToUpper(resp)
	if strings.Contains(upper, "ERROR") {
		return resp, fmt.Errorf("PIN 解锁失败：%s", summarizeATError(resp))
	}
	return resp, nil
}

func summarizeATError(resp string) string {
	for _, line := range splitATLines(resp) {
		up := strings.ToUpper(line)
		if strings.HasPrefix(up, "+CME ERROR") || up == "ERROR" {
			return line
		}
	}
	trimmed := strings.TrimSpace(resp)
	if trimmed == "" {
		return "模块返回错误"
	}
	return trimmed
}

func (a *app) queryCPINState() (simPINState, error) {
	a.simPINCommandMu.Lock()
	defer a.simPINCommandMu.Unlock()
	resp, err := a.runATCommand("AT+CPIN?", 3*time.Second)
	if err != nil {
		return simPINState{}, err
	}
	return parseCPINState(resp), nil
}

func formatSIMPINError(action string, err error) string {
	if err == nil {
		return action
	}
	if isUSBTransferRecoverable(err) {
		return action + "：USB 通信异常，请稍后重试或重新插拔模块"
	}
	return action + "：" + err.Error()
}

func (a *app) completeSIMPINUnlock(w http.ResponseWriter, pin string, save bool) {
	imei, iccid := a.currentSIMIdentity()
	saved := false
	if save {
		if err := a.saveSIMPIN(imei, iccid, pin); err != nil {
			writeError(w, http.StatusInternalServerError, "解锁成功，但保存 PIN 失败："+err.Error())
			return
		}
		saved = true
	}
	after, _ := a.queryCPINState()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"message":       "SIM PIN 解锁成功",
		"sim_pin_state": after.Normalized,
		"sim_pin_saved": saved,
		"imei":          imei,
		"iccid":         iccid,
	})
}

// maybeAutoUnlockSIM attempts one automatic unlock when the SIM asks for PIN
// and a saved code exists. Failed saved codes are dropped to avoid burning retries.
func (a *app) maybeAutoUnlockSIM(status *modem.DeviceStatus) {
	if status == nil || !status.SimPinRequired {
		return
	}
	pin, key := a.lookupSavedSIMPIN(status.IMEI, status.ICCID)
	if pin == "" {
		if fallback := a.lookupDefaultSIMPIN(); fallback != "" {
			pin, key = fallback, "default"
		}
	}
	if pin == "" || a.isSIMPINAutoFailed(key) {
		return
	}
	log.Printf("SIM PIN required; attempting automatic unlock")
	if _, err := a.enterSIMPIN(pin); err != nil {
		log.Printf("automatic SIM PIN unlock failed: %v", err)
		a.markSIMPINAutoFailed(key)
		_ = a.clearSavedSIMPIN(status.IMEI, status.ICCID)
		status.SimPinSaved = false
		return
	}
	// Refresh identity after unlock so ICCID can be bound to the saved PIN.
	if iccidResp, err := a.runATCommand("AT+QCCID", 3*time.Second); err == nil {
		if iccid := parseUSBATPrefixed(iccidResp, "+QCCID:"); iccid != "" {
			status.ICCID = iccid
		}
	}
	if imsiResp, err := a.runATCommand("AT+CIMI", 3*time.Second); err == nil {
		if imsi := parseUSBATIMSI(imsiResp); imsi != "" {
			status.IMSI = imsi
		}
	}
	_ = a.saveSIMPIN(status.IMEI, status.ICCID, pin)
	if state, err := a.queryCPINState(); err == nil {
		applySIMPINFields(status, state, true)
	} else {
		status.SimPinRequired = false
		status.SimPinState = "ready"
		status.SimInserted = true
		status.SimPinSaved = true
	}
}

func (a *app) lookupDefaultSIMPIN() string {
	a.simPINMu.Lock()
	defer a.simPINMu.Unlock()
	a.ensureSIMPINStoreLocked()
	return strings.TrimSpace(a.simPINPins["default"])
}

func (a *app) enrichStatusWithSIMPIN(status *modem.DeviceStatus, cpinResp string) {
	state := parseCPINState(cpinResp)
	saved := a.hasSavedSIMPIN(status.IMEI, status.ICCID) || a.lookupDefaultSIMPIN() != ""
	applySIMPINFields(status, state, saved)
	a.maybeAutoUnlockSIM(status)
}

// POST /api/sim/unlock {"pin":"1234","save":true}
func (a *app) unlockSIM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PIN  string `json:"pin"`
		Save *bool  `json:"save"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pin := strings.TrimSpace(body.PIN)
	if err := validateSIMPIN(pin); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	save := true
	if body.Save != nil {
		save = *body.Save
	}

	state, err := a.queryCPINState()
	if err != nil {
		log.Printf("CPIN query failed before unlock, attempting direct CPIN: %v", err)
		if _, unlockErr := a.enterSIMPIN(pin); unlockErr != nil {
			writeError(w, http.StatusBadGateway, formatSIMPINError("SIM PIN 解锁失败", unlockErr))
			return
		}
		a.completeSIMPINUnlock(w, pin, save)
		return
	}
	if state.PukRequired {
		writeError(w, http.StatusConflict, "SIM 已要求 PUK，请使用运营商提供的 PUK 在其他设备解锁后再试")
		return
	}
	if !state.PinRequired && state.Normalized == "ready" {
		imei, iccid := a.currentSIMIdentity()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"message":       "SIM 已处于解锁状态",
			"sim_pin_state": state.Normalized,
			"sim_pin_saved": a.hasSavedSIMPIN(imei, iccid) || a.lookupDefaultSIMPIN() != "",
		})
		return
	}
	if !state.PinRequired {
		writeError(w, http.StatusConflict, fmt.Sprintf("当前 SIM 状态为 %s，无需输入 PIN", displaySIMPINState(state)))
		return
	}

	if _, err := a.enterSIMPIN(pin); err != nil {
		writeError(w, http.StatusBadGateway, formatSIMPINError("SIM PIN 解锁失败", err))
		return
	}

	a.completeSIMPINUnlock(w, pin, save)
}

// DELETE /api/sim/pin — forget saved PIN for the current module/SIM.
func (a *app) clearSIMPIN(w http.ResponseWriter, r *http.Request) {
	imei, iccid := a.currentSIMIdentity()
	if err := a.clearSavedSIMPIN(imei, iccid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "已清除保存的 SIM PIN",
	})
}

func (a *app) currentSIMIdentity() (imei, iccid string) {
	if gsnResp, err := a.runATCommand("AT+CGSN", 3*time.Second); err == nil {
		imei = parseUSBATIMEI(gsnResp)
	}
	if iccidResp, err := a.runATCommand("AT+QCCID", 3*time.Second); err == nil {
		iccid = parseUSBATPrefixed(iccidResp, "+QCCID:")
	}
	return imei, iccid
}

func displaySIMPINState(state simPINState) string {
	switch state.Normalized {
	case "ready":
		return "已解锁"
	case "sim_pin":
		return "需要 PIN"
	case "sim_puk":
		return "需要 PUK"
	case "not_inserted":
		return "未插入"
	default:
		if state.Raw != "" {
			return state.Raw
		}
		return "未知"
	}
}