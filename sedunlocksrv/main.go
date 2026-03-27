// main.go — SED Unlock Server (sedunlocksrv)
//
// This is the core of the Pre-Boot Authentication (PBA) system. It runs inside
// a minimal TinyCore Linux environment loaded from the drive's shadow MBR before
// the real OS boots.
//
// It serves two parallel interfaces:
//   1. HTTPS web UI (port 443) — index.html communicates with the JSON API below
//   2. Interactive console TUI — runs in a goroutine on the physical terminal
//
// An SSH interface (ssh_sed_unlock.sh) also communicates with this server's
// JSON API over localhost using curl.
//
// Typical boot flow:
//   PBA image loads → this binary starts → user unlocks drive via web/SSH/console
//   → /boot is called → kexec loads the real Proxmox kernel → system continues

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// ============================================================
// DATA TYPES
// ============================================================

type DriveStatus struct {
	Device string `json:"device"`
	Locked bool   `json:"locked"`
	Opal   bool   `json:"opal"`
}

type NetworkInterfaceStatus struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	State     string   `json:"state"`
	Carrier   bool     `json:"carrier"`
	Loopback  bool     `json:"loopback"`
	Addresses []string `json:"addresses,omitempty"`
}

type StatusResponse struct {
	Drives            []DriveStatus            `json:"drives"`
	Interfaces        []NetworkInterfaceStatus `json:"interfaces"`
	BootReady         bool                     `json:"bootReady"`
	BootDrives        []string                 `json:"bootDrives,omitempty"`
	FailedAttempts    int                      `json:"failedAttempts"`
	MaxAttempts       int                      `json:"maxAttempts"`
	AttemptsRemaining int                      `json:"attemptsRemaining"`
	Build             string                   `json:"build,omitempty"`
}

type DriveDiagnostics struct {
	Device              string `json:"device"`
	Opal                bool   `json:"opal"`
	Locked              bool   `json:"locked"`
	LockingSupported    string `json:"lockingSupported"`
	LockingEnabled      string `json:"lockingEnabled"`
	MBREnabled          string `json:"mbrEnabled"`
	MBRDone             string `json:"mbrDone"`
	MediaEncrypt        string `json:"mediaEncrypt"`
	LockingRange0Locked string `json:"lockingRange0Locked"`
	QueryRaw            string `json:"queryRaw"`
}

type DiagnosticsResponse struct {
	GeneratedAt string             `json:"generatedAt"`
	Drives      []DriveDiagnostics `json:"drives"`
}

type UnlockResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
}

type UnlockResponse struct {
	Results []UnlockResult `json:"results"`
	Token   string         `json:"token,omitempty"`
}

type PasswordChangeResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type PasswordChangeResponse struct {
	Results []PasswordChangeResult `json:"results"`
}

type BootResult struct {
	Kernel        string        `json:"kernel"`
	Initrd        string        `json:"initrd"`
	Cmdline       string        `json:"cmdline"`
	Drives        []DriveStatus `json:"drives"`
	Warning       string        `json:"warning,omitempty"`
	FullyUnlocked bool          `json:"fullyUnlocked"`
	Debug         []string      `json:"debug,omitempty"`
}

type BootLaunchStatus struct {
	InProgress bool        `json:"inProgress"`
	Accepted   bool        `json:"accepted"`
	Error      string      `json:"error,omitempty"`
	Debug      []string    `json:"debug,omitempty"`
	Result     *BootResult `json:"result,omitempty"`
}

type BootAttemptError struct {
	Message string
	Debug   []string
}

func (e BootAttemptError) Error() string {
	return e.Message
}

type BootEntry struct {
	KernelRef    string
	KernelBase   string
	KernelSuffix string
	InitrdRefs   []string
	InitrdBases  []string
	InitrdSuffix []string
	Cmdline      string
	Source       string
}

// PasswordPolicy describes complexity requirements for setting a new password.
// It is NOT applied to unlock attempts — the drive may have been initialized
// with a password that doesn't meet these requirements.
type PasswordPolicy struct {
	MinLength      int  `json:"minLength"`
	RequireUpper   bool `json:"requireUpper"`
	RequireLower   bool `json:"requireLower"`
	RequireNumber  bool `json:"requireNumber"`
	RequireSpecial bool `json:"requireSpecial"`
}

// ============================================================
// GLOBALS
// ============================================================
// kexecReady is closed by BootSystem after kexec -l succeeds, signalling
// main() to shut down the HTTP server and execute kexec -e.
// kexecFailed receives the error if kexec -e returns (i.e. fails), so
// BootSystem can propagate it and main() can restart the HTTP server.
var (
	kexecReady  = make(chan struct{})
	kexecFailed = make(chan error, 1)
)

var (
	failedAttempts int
	maxAttempts    = 5
	mu             sync.Mutex
	unlockMu       sync.Mutex

	sessionMu         sync.RWMutex
	apiSessionToken   string
	expertSessionTok  string
	bootStateMu       sync.RWMutex
	startupLockedOpal = map[string]struct{}{}
	bootLaunchState   BootLaunchStatus

	passwordPolicy     = loadPolicy()
	expertPasswordHash = loadExpertPasswordHash()
)

var buildVersion = "dev"

type filteredHTTPLogWriter struct{}

func (filteredHTTPLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	benignSubstrings := []string{
		"TLS handshake error",
		"use of closed network connection",
		"broken pipe",
		"connection reset by peer",
		"EOF",
	}
	for _, s := range benignSubstrings {
		if strings.Contains(msg, s) {
			return len(p), nil
		}
	}
	log.Printf("[http] %s", msg)
	return len(p), nil
}

// ============================================================
// PASSWORD POLICY
// ============================================================

func loadPolicy() PasswordPolicy {
	getBool := func(k string, def bool) bool {
		v := os.Getenv(k)
		if v == "" {
			return def
		}
		return v == "true"
	}
	getInt := func(k string, def int) int {
		if i, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return i
		}
		return def
	}
	return PasswordPolicy{
		MinLength:      getInt("MIN_PASSWORD_LENGTH", 12),
		RequireUpper:   getBool("REQUIRE_UPPER", true),
		RequireLower:   getBool("REQUIRE_LOWER", true),
		RequireNumber:  getBool("REQUIRE_NUMBER", true),
		RequireSpecial: getBool("REQUIRE_SPECIAL", true),
	}
}

func loadExpertPasswordHash() string {
	return strings.TrimSpace(os.Getenv("EXPERT_PASSWORD_HASH"))
}

// validatePassword checks a proposed new password against the loaded policy.
// Called only when *setting* a password — NOT during unlock.
func validatePassword(password string) error {
	if len(password) < passwordPolicy.MinLength {
		return fmt.Errorf("minimum length %d", passwordPolicy.MinLength)
	}
	var hasUpper, hasLower, hasNumber, hasSpecial bool
	for _, c := range password {
		switch {
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsNumber(c):
			hasNumber = true
		case !unicode.IsLetter(c) && !unicode.IsNumber(c):
			hasSpecial = true
		}
	}
	if passwordPolicy.RequireUpper && !hasUpper {
		return fmt.Errorf("missing uppercase")
	}
	if passwordPolicy.RequireLower && !hasLower {
		return fmt.Errorf("missing lowercase")
	}
	if passwordPolicy.RequireNumber && !hasNumber {
		return fmt.Errorf("missing number")
	}
	if passwordPolicy.RequireSpecial && !hasSpecial {
		return fmt.Errorf("missing special character")
	}
	return nil
}

// ============================================================
// DRIVE SCANNING
// ============================================================

func scanDrives() []DriveStatus {
	var statuses []DriveStatus

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "sedutil-cli", "--scan").Output()
	if err != nil {
		return statuses
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}

		opal := strings.HasPrefix(fields[1], "2")
		locked := false
		if opal {
			query, _ := queryDrive(dev)
			locked = strings.Contains(query, "Locked = Y")
		}
		statuses = append(statuses, DriveStatus{Device: dev, Locked: locked, Opal: opal})
	}
	return statuses
}

func queryDrive(dev string) (string, error) {
	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer qcancel()
	query, err := exec.CommandContext(qctx, "sedutil-cli", "--query", dev).CombinedOutput()
	return string(query), err
}

func queryField(query, field string) string {
	prefix := field + " = "
	for _, line := range strings.Split(query, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(t, prefix))
		}
	}
	return "unknown"
}

func collectDriveDiagnostics() []DriveDiagnostics {
	drives := scanDrives()
	diag := make([]DriveDiagnostics, 0, len(drives))
	for _, d := range drives {
		raw, _ := queryDrive(d.Device)
		diag = append(diag, DriveDiagnostics{
			Device:              d.Device,
			Opal:                d.Opal,
			Locked:              d.Locked,
			LockingSupported:    queryField(raw, "LockingSupported"),
			LockingEnabled:      queryField(raw, "LockingEnabled"),
			MBREnabled:          queryField(raw, "MBREnable"),
			MBRDone:             queryField(raw, "MBRDone"),
			MediaEncrypt:        queryField(raw, "MediaEncrypt"),
			LockingRange0Locked: queryField(raw, "Locked"),
			QueryRaw:            strings.TrimSpace(raw),
		})
	}
	return diag
}

func scanNetworkInterfaces() []NetworkInterfaceStatus {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	statuses := make([]NetworkInterfaceStatus, 0, len(interfaces))
	for _, iface := range interfaces {
		addrs, _ := iface.Addrs()
		addresses := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addresses = append(addresses, addr.String())
		}
		sort.Strings(addresses)

		state := "unknown"
		if b, err := os.ReadFile(filepath.Join("/sys/class/net", iface.Name, "operstate")); err == nil {
			state = strings.TrimSpace(string(b))
		}
		carrier := false
		if b, err := os.ReadFile(filepath.Join("/sys/class/net", iface.Name, "carrier")); err == nil {
			carrier = strings.TrimSpace(string(b)) == "1"
		}
		statuses = append(statuses, NetworkInterfaceStatus{
			Name:      iface.Name,
			MAC:       iface.HardwareAddr.String(),
			State:     state,
			Carrier:   carrier,
			Loopback:  (iface.Flags & net.FlagLoopback) != 0,
			Addresses: addresses,
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Loopback != statuses[j].Loopback {
			return !statuses[i].Loopback
		}
		return statuses[i].Name < statuses[j].Name
	})
	return statuses
}

func initializeBootState() {
	drives := scanDrives()
	locked := make(map[string]struct{})
	for _, d := range drives {
		if d.Opal && d.Locked {
			locked[d.Device] = struct{}{}
		}
	}
	bootStateMu.Lock()
	startupLockedOpal = locked
	bootStateMu.Unlock()
}

func currentBootLaunchStatus() BootLaunchStatus {
	bootStateMu.RLock()
	defer bootStateMu.RUnlock()
	status := bootLaunchState
	if status.Debug != nil {
		status.Debug = append([]string(nil), status.Debug...)
	}
	if status.Result != nil {
		resultCopy := *status.Result
		if resultCopy.Debug != nil {
			resultCopy.Debug = append([]string(nil), resultCopy.Debug...)
		}
		if resultCopy.Drives != nil {
			resultCopy.Drives = append([]DriveStatus(nil), resultCopy.Drives...)
		}
		status.Result = &resultCopy
	}
	return status
}

func resetBootLaunchStateLocked() {
	bootLaunchState = BootLaunchStatus{}
}

func beginBootLaunch() error {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	if bootLaunchState.InProgress {
		return fmt.Errorf("boot is already in progress")
	}
	resetBootLaunchStateLocked()
	bootLaunchState.InProgress = true
	return nil
}

func finishBootLaunch(result *BootResult, err error) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootLaunchState.InProgress = false
	if err != nil {
		bootLaunchState.Accepted = false
		bootLaunchState.Result = nil
		if bootErr, ok := err.(BootAttemptError); ok {
			bootLaunchState.Error = bootErr.Message
			bootLaunchState.Debug = append([]string(nil), bootErr.Debug...)
			return
		}
		bootLaunchState.Error = err.Error()
		bootLaunchState.Debug = nil
		return
	}
	bootLaunchState.Accepted = true
	bootLaunchState.Error = ""
	bootLaunchState.Debug = nil
	if result != nil {
		resultCopy := *result
		if resultCopy.Debug != nil {
			resultCopy.Debug = append([]string(nil), resultCopy.Debug...)
		}
		if resultCopy.Drives != nil {
			resultCopy.Drives = append([]DriveStatus(nil), resultCopy.Drives...)
		}
		bootLaunchState.Result = &resultCopy
	}
}

func startBootLaunch() error {
	if err := beginBootLaunch(); err != nil {
		return err
	}
	go func() {
		result, err := BootSystem()
		// Only call finishBootLaunch if BootSystem didn't call it already (for success case)
		bootStateMu.RLock()
		alreadyFinished := bootLaunchState.Accepted
		bootStateMu.RUnlock()
		if !alreadyFinished {
			finishBootLaunch(result, err)
		}
	}()
	return nil
}

func recordBootLaunchDebug(line string) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	if !bootLaunchState.InProgress {
		return
	}
	bootLaunchState.Debug = append(bootLaunchState.Debug, line)
}

func startupLockedSet() map[string]struct{} {
	bootStateMu.RLock()
	defer bootStateMu.RUnlock()
	out := make(map[string]struct{}, len(startupLockedOpal))
	for dev := range startupLockedOpal {
		out[dev] = struct{}{}
	}
	return out
}

func bootCandidateDrives(drives []DriveStatus) []string {
	startupLocked := startupLockedSet()
	candidates := make([]string, 0)
	for _, d := range drives {
		if !d.Opal || d.Locked {
			continue
		}
		if _, ok := startupLocked[d.Device]; ok {
			candidates = append(candidates, d.Device)
		}
	}
	sort.Strings(candidates)
	return candidates
}

func currentStatus() StatusResponse {
	drives := scanDrives()
	failed, max, remaining := unlockAttemptStatus()
	return StatusResponse{
		Drives:            drives,
		Interfaces:        scanNetworkInterfaces(),
		BootReady:         len(bootCandidateDrives(drives)) > 0,
		BootDrives:        bootCandidateDrives(drives),
		FailedAttempts:    failed,
		MaxAttempts:       max,
		AttemptsRemaining: remaining,
		Build:             buildVersion,
	}
}

func unlockAttemptStatus() (failed, max, remaining int) {
	mu.Lock()
	defer mu.Unlock()
	remaining = maxAttempts - failedAttempts
	if remaining < 0 {
		remaining = 0
	}
	return failedAttempts, maxAttempts, remaining
}

// ============================================================
// SESSION TOKEN
// ============================================================

func mintSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	sessionMu.Lock()
	apiSessionToken = token
	sessionMu.Unlock()
	return token, nil
}

func mintExpertToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	sessionMu.Lock()
	expertSessionTok = token
	sessionMu.Unlock()
	return token, nil
}

func validSessionToken(token string) bool {
	if token == "" {
		return false
	}
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return apiSessionToken != "" && token == apiSessionToken
}

func requireSessionToken(w http.ResponseWriter, r *http.Request) bool {
	if !validSessionToken(r.Header.Get("X-Auth-Token")) {
		jsonResponse(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return true
	}
	return false
}

func anyUnlockedDrive() bool {
	return len(bootCandidateDrives(scanDrives())) > 0
}

func requireSessionTokenOrUnlockedDrive(w http.ResponseWriter, r *http.Request) bool {
	if validSessionToken(r.Header.Get("X-Auth-Token")) || anyUnlockedDrive() {
		return false
	}
	jsonResponse(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
	return true
}

func validExpertToken(token string) bool {
	if token == "" {
		return false
	}
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return expertSessionTok != "" && token == expertSessionTok
}

func requireExpertToken(w http.ResponseWriter, r *http.Request) bool {
	if !validExpertToken(r.Header.Get("X-Expert-Token")) {
		jsonResponse(w, http.StatusForbidden, map[string]string{"error": "expert authentication required"})
		return true
	}
	return false
}

func anySuccessfulUnlock(results []UnlockResult) bool {
	for _, r := range results {
		if r.Success {
			return true
		}
	}
	return false
}

// ============================================================
// CONSOLE HELPERS
// ============================================================

func printConsoleStatus(status StatusResponse) {
	if len(status.Drives) == 0 {
		fmt.Println("No OPAL drives detected.")
	} else {
		for _, d := range status.Drives {
			lockState := "UNLOCKED"
			if d.Locked {
				lockState = "LOCKED"
			}
			opalState := "NON-OPAL"
			if d.Opal {
				opalState = "OPAL"
			}
			marker := "✅"
			if d.Locked {
				marker = "❌"
			}
			fmt.Printf("%s %s  %s  %s\n", marker, d.Device, lockState, opalState)
		}
	}
	fmt.Printf("\nUnlock attempts: %d/%d failed (%d remaining)\n", status.FailedAttempts, status.MaxAttempts, status.AttemptsRemaining)
	fmt.Println("\nNetwork Interfaces:")
	for _, iface := range status.Interfaces {
		link := "no-link"
		if iface.Carrier {
			link = "link"
		}
		line := fmt.Sprintf("  %s  %s  %s", iface.Name, iface.State, link)
		if iface.MAC != "" {
			line += "  " + iface.MAC
		}
		if len(iface.Addresses) > 0 {
			line += "  " + strings.Join(iface.Addresses, ", ")
		}
		if iface.Loopback {
			line += "  loopback"
		}
		fmt.Println(line)
	}
}

// ============================================================
// UNLOCK
// ============================================================

func attemptUnlock(password string) ([]UnlockResult, error) {
	if strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("password cannot be blank")
	}

	unlockMu.Lock()
	defer unlockMu.Unlock()

	mu.Lock()
	if failedAttempts >= maxAttempts {
		mu.Unlock()
		return nil, fmt.Errorf("maximum failed attempts reached")
	}
	mu.Unlock()

	var results []UnlockResult
	successAny := false

	for _, d := range scanDrives() {
		if !d.Locked {
			continue
		}
		err1 := exec.Command("sedutil-cli", "--setlockingrange", "0", "rw", password, d.Device).Run()
		err2 := exec.Command("sedutil-cli", "--setmbrdone", "on", password, d.Device).Run()
		success := err1 == nil && err2 == nil
		if success {
			successAny = true
			rescanBlockDeviceLayout(d.Device)
		}
		results = append(results, UnlockResult{Device: d.Device, Success: success})
	}

	if successAny {
		mu.Lock()
		failedAttempts = 0
		mu.Unlock()
	} else {
		mu.Lock()
		failedAttempts++
		log.Printf("Failed unlock attempt %d/%d\n", failedAttempts, maxAttempts)
		if failedAttempts >= maxAttempts {
			mu.Unlock()
			log.Println("Max failed attempts reached. Powering off.")
			go func() {
				time.Sleep(500 * time.Millisecond)
				exec.Command("poweroff", "-nf").Run()
			}()
			return results, fmt.Errorf("maximum failed attempts reached")
		}
		mu.Unlock()
	}
	return results, nil
}

// ============================================================
// PASSWORD CHANGE
// ============================================================

func passwordPolicySummary() string {
	parts := []string{fmt.Sprintf("min %d chars", passwordPolicy.MinLength)}
	if passwordPolicy.RequireUpper {
		parts = append(parts, "uppercase")
	}
	if passwordPolicy.RequireLower {
		parts = append(parts, "lowercase")
	}
	if passwordPolicy.RequireNumber {
		parts = append(parts, "number")
	}
	if passwordPolicy.RequireSpecial {
		parts = append(parts, "special")
	}
	return strings.Join(parts, ", ")
}

func eligiblePasswordChangeTargets(drives []DriveStatus) []DriveStatus {
	startupLocked := startupLockedSet()
	targets := make([]DriveStatus, 0)
	for _, d := range drives {
		if !d.Opal || d.Locked {
			continue
		}
		if _, ok := startupLocked[d.Device]; ok {
			targets = append(targets, d)
		}
	}
	if len(targets) > 0 {
		return targets
	}
	for _, d := range drives {
		if d.Opal && !d.Locked {
			targets = append(targets, d)
		}
	}
	return targets
}

func selectPasswordChangeTargets(selected []string) ([]DriveStatus, error) {
	eligible := eligiblePasswordChangeTargets(scanDrives())
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no unlocked OPAL drives are eligible for password change")
	}

	if len(selected) == 0 {
		return nil, fmt.Errorf("select at least one target drive for password change")
	}

	eligibleByDevice := make(map[string]DriveStatus, len(eligible))
	for _, drive := range eligible {
		eligibleByDevice[drive.Device] = drive
	}

	seen := make(map[string]struct{}, len(selected))
	targets := make([]DriveStatus, 0, len(selected))
	for _, raw := range selected {
		device := strings.TrimSpace(raw)
		if device == "" {
			continue
		}
		if _, ok := seen[device]; ok {
			continue
		}
		drive, ok := eligibleByDevice[device]
		if !ok {
			return nil, fmt.Errorf("%s is not an unlocked OPAL drive eligible for password change", device)
		}
		seen[device] = struct{}{}
		targets = append(targets, drive)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("select at least one target drive for password change")
	}
	return targets, nil
}

func trimSedutilOutput(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > 220 {
		return out[:217] + "..."
	}
	return out
}

func changePassword(current, newPw string, selected []string) ([]PasswordChangeResult, error) {
	current = strings.TrimSpace(current)
	newPw = strings.TrimSpace(newPw)
	if current == "" {
		return nil, fmt.Errorf("current password is required")
	}
	if newPw == "" {
		return nil, fmt.Errorf("new password is required")
	}

	targets, err := selectPasswordChangeTargets(selected)
	if err != nil {
		return nil, err
	}

	var results []PasswordChangeResult
	for _, d := range targets {
		adminOut, adminErr := runSedutil(20*time.Second, "--setAdmin1Pwd", current, newPw, d.Device)
		sidOut, sidErr := runSedutil(20*time.Second, "--setSIDPassword", current, newPw, d.Device)

		success := adminErr == nil && sidErr == nil
		var detail, errMsg string
		switch {
		case adminErr == nil && sidErr == nil:
			detail = "updated Admin1 and SID passwords"
		case adminErr == nil && sidErr != nil:
			detail = "updated Admin1 password; SID update failed"
			if extra := trimSedutilOutput(sidOut); extra != "" {
				detail += ": " + extra
			}
		case adminErr != nil && sidErr == nil:
			errMsg = "Admin1 password update failed; unlock will still require the old password"
			if extra := trimSedutilOutput(adminOut); extra != "" {
				errMsg += ": " + extra
			}
			detail = "SID password updated"
		default:
			errMsg = "failed to update Admin1 and SID passwords"
			extras := make([]string, 0, 2)
			if extra := trimSedutilOutput(adminOut); extra != "" {
				extras = append(extras, "Admin1: "+extra)
			}
			if extra := trimSedutilOutput(sidOut); extra != "" {
				extras = append(extras, "SID: "+extra)
			}
			if len(extras) > 0 {
				errMsg += " (" + strings.Join(extras, " | ") + ")"
			}
		}

		results = append(results, PasswordChangeResult{
			Device:  d.Device,
			Success: success,
			Error:   errMsg,
			Detail:  detail,
		})
	}
	return results, nil
}

// ============================================================
// BOOT
// ============================================================

// listDevicePartitions returns all partition device paths for the given device.
//
// sedutil-cli reports NVMe drives as /dev/nvme0 (the controller), but the
// actual block device is the namespace, e.g. /dev/nvme0n1, and partitions are
// nvme0n1p1, nvme0n1p2, etc.
//
// We scan /sys/class/block/ (a flat directory of symlinks) and use the pkname
// file to identify which block device is the parent of each partition. For NVMe
// we must check pkname against both the controller name ("nvme0") AND any
// namespace names ("nvme0n1", "nvme0n2", etc.) derived from it.
// For SATA/SAS (sda, sdb, etc.) pkname will simply equal the base name directly.
func listDevicePartitions(device string) ([]string, error) {
	base := filepath.Base(device) // e.g. "nvme0" or "sda"

	// Build the set of block device names whose partitions we want.
	// For sda this is just {"sda"}.
	// For nvme0 this is {"nvme0", "nvme0n1", "nvme0n2", ...} — we find the
	// namespaces by scanning /sys/class/block for entries whose name starts
	// with base+"n" and that have a "dev" file but no "partition" file.
	owners := map[string]struct{}{base: {}}

	allEntries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}

	for _, entry := range allEntries {
		name := entry.Name()
		// Namespace names look like nvme0n1, nvme0n2 — starts with base
		// followed immediately by 'n' and a digit.
		if !strings.HasPrefix(name, base+"n") {
			continue
		}
		// Must have a "dev" file (it's a real block device).
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "dev")); err != nil {
			continue
		}
		// Must NOT have a "partition" file (namespaces are not partitions).
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err == nil {
			continue
		}
		owners[name] = struct{}{}
	}

	// Now collect every entry that has a "partition" file and whose pkname
	// is one of our owner names.
	var partitions []string
	seen := map[string]struct{}{}

	for _, entry := range allEntries {
		name := entry.Name()
		// Must be a partition.
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err != nil {
			continue
		}
		// Read its parent name.
		pkRaw, err := os.ReadFile(filepath.Join("/sys/class/block", name, "pkname"))
		if err != nil {
			// pkname unavailable — fall back to prefix match against all owners.
			for owner := range owners {
				if strings.HasPrefix(name, owner) {
					dev := "/dev/" + name
					if _, ok := seen[dev]; !ok {
						seen[dev] = struct{}{}
						partitions = append(partitions, dev)
					}
					break
				}
			}
			continue
		}
		parent := strings.TrimSpace(string(pkRaw))
		if _, ok := owners[parent]; ok {
			dev := "/dev/" + name
			if _, ok := seen[dev]; !ok {
				seen[dev] = struct{}{}
				partitions = append(partitions, dev)
			}
		}
	}

	sort.Strings(partitions)
	return partitions, nil
}

func listDeviceNodes(device string) ([]string, error) {
	base := filepath.Base(device)
	nodes := []string{"/dev/" + base}

	allEntries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{"/dev/" + base: {}}
	for _, entry := range allEntries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+"n") {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "dev")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err == nil {
			continue
		}
		dev := "/dev/" + name
		if _, ok := seen[dev]; ok {
			continue
		}
		seen[dev] = struct{}{}
		nodes = append(nodes, dev)
	}

	sort.Strings(nodes)
	return nodes, nil
}

func rescanBlockDeviceLayout(device string) {
	nodes, err := listDeviceNodes(device)
	if err != nil {
		return
	}

	for _, node := range nodes {
		if f, err := os.OpenFile(node, os.O_RDONLY, 0); err == nil {
			_ = unix.IoctlSetInt(int(f.Fd()), unix.BLKRRPART, 0)
			f.Close()
		}
		if haveRuntimeCommand("blockdev") {
			_ = exec.Command("blockdev", "--rereadpt", node).Run()
		}
		if haveRuntimeCommand("partprobe") {
			_ = exec.Command("partprobe", node).Run()
		}
		if haveRuntimeCommand("partx") {
			_ = exec.Command("partx", "-u", node).Run()
		}
	}

	if haveRuntimeCommand("udevadm") {
		_ = exec.Command("udevadm", "settle").Run()
	}

	time.Sleep(300 * time.Millisecond)
}

func availableRescanTools() []string {
	tools := []string{"ioctl(BLKRRPART)"}
	for _, name := range []string{"blockdev", "partprobe", "partx", "udevadm"} {
		if haveRuntimeCommand(name) {
			tools = append(tools, name)
		}
	}
	return tools
}

func availableLVMTools() []string {
	tools := make([]string, 0, 4)
	for _, name := range []string{"blkid", "pvscan", "vgscan", "vgchange"} {
		if haveRuntimeCommand(name) {
			tools = append(tools, name)
		}
	}
	return tools
}

func haveRuntimeCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func activateLVM() {
	if haveRuntimeCommand("pvscan") {
		_ = exec.Command("pvscan", "--cache").Run()
	}
	if haveRuntimeCommand("vgscan") {
		_ = exec.Command("vgscan", "--mknodes").Run()
	}
	if haveRuntimeCommand("vgchange") {
		_ = exec.Command("vgchange", "-ay").Run()
	}
}

func listLogicalVolumes() []string {
	matches, _ := filepath.Glob("/dev/mapper/*")
	lvs := make([]string, 0, len(matches))
	for _, path := range matches {
		if filepath.Base(path) == "control" {
			continue
		}
		lvs = append(lvs, path)
	}
	sort.Strings(lvs)
	return lvs
}

func listAllBlockPartitions() ([]string, error) {
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}

	partitions := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err != nil {
			continue
		}
		partitions = append(partitions, "/dev/"+name)
	}
	sort.Strings(partitions)
	return partitions, nil
}

func listMDDevices() ([]string, error) {
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}

	devices := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "md") {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "dev")); err != nil {
			continue
		}
		devices = append(devices, "/dev/"+name)
	}
	sort.Strings(devices)
	return devices, nil
}

func likelyLVMPhysicalVolume(device string) bool {
	f, err := os.Open(device)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return false
	}
	buf = buf[:n]
	return bytes.Contains(buf, []byte("LABELONE")) && bytes.Contains(buf, []byte("LVM2"))
}

func probeBlockType(device string) string {
	if haveRuntimeCommand("blkid") {
		if out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", device).Output(); err == nil {
			if t := strings.TrimSpace(string(out)); t != "" {
				return t
			}
		}
	}
	if likelyLVMPhysicalVolume(device) {
		return "LVM2_member"
	}
	return "unknown"
}

func buildBootSearchDevices(bootDrives []string) ([]string, error) {
	devices := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(path string) {
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		devices = append(devices, path)
	}

	for _, bootDrive := range bootDrives {
		partitions, err := listDevicePartitions(bootDrive)
		if err != nil {
			return nil, err
		}
		for _, part := range partitions {
			add(part)
		}
	}

	activateLVM()
	for _, lv := range listLogicalVolumes() {
		add(lv)
	}

	mds, err := listMDDevices()
	if err != nil {
		return nil, err
	}
	for _, md := range mds {
		add(md)
	}

	// Fall back to every visible partition so we can find a separate EFI or
	// /boot filesystem even when it lives on another disk or RAID member.
	partitions, err := listAllBlockPartitions()
	if err != nil {
		return nil, err
	}
	for _, part := range partitions {
		add(part)
	}

	return devices, nil
}

func collectBootFiles(mountPoint string) ([]string, []string, []string, []string) {
	// collectBootFiles scans for boot artifacts supporting major Linux distributions:
	// - Debian/Ubuntu: vmlinuz-*, initrd.img-*
	// - Fedora/RHEL/CentOS: vmlinuz-*, initramfs-*
	// - SUSE/openSUSE: linux, linux-*, initrd, initramfs-*
	// - Arch Linux: vmlinuz, vmlinuz-*, initramfs-*, initramfs-linux.img
	// - NixOS: bzImage, initrd (custom naming)
	// - Proxmox: bzImage, custom kernel naming
	// - Custom kernels (kernel.org): vmlinuz-*, bzImage, linux-*
	// - Generic/custom: bzImage, linux, initrd
	// 
	// Uses filename pattern matching for known distributions, complemented by
	// binary inspection in enhancedCollectBootFiles() for unknown binaries
	loaderEntries := make([]string, 0)
	grubConfigs := make([]string, 0)
	kernels := make([]string, 0)
	initrds := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(out *[]string, path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		*out = append(*out, path)
	}

	_ = filepath.WalkDir(mountPoint, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}

		base := filepath.Base(path)
		dir := filepath.Dir(path)
		switch {
		case strings.EqualFold(base, "grub.cfg"):
			add(&grubConfigs, path)
		case filepath.Base(dir) == "entries" && filepath.Base(filepath.Dir(dir)) == "loader" && strings.HasSuffix(base, ".conf"):
			add(&loaderEntries, path)
		case strings.HasPrefix(base, "vmlinuz-"), strings.HasPrefix(base, "linux-"),
			// Kernel patterns supporting major Linux distributions:
			// vmlinuz-* : Debian/Ubuntu, Fedora/RHEL/CentOS, most systemd-based distros
			// linux-*   : SUSE/openSUSE, some custom configurations, kernel.org custom builds
			// vmlinuz   : Arch Linux main kernel (no version suffix)
			// linux     : SUSE/openSUSE kernel naming
			// bzImage   : Generic uncompressed kernel (NixOS, Proxmox, custom setups, kernel.org direct)
			base == "vmlinuz",    // Arch Linux
			base == "linux",       // SUSE/openSUSE
			base == "bzImage":     // NixOS, Proxmox, custom, kernel.org
			add(&kernels, path)
		case strings.HasPrefix(base, "initrd.img-"), strings.HasPrefix(base, "initramfs-"),
			// Initrd patterns supporting major Linux distributions:
			// initrd.img-* : Debian/Ubuntu, most Debian-based distros
			// initramfs-*  : Fedora/RHEL/CentOS, SUSE/openSUSE, Arch Linux
			// initrd       : SUSE/openSUSE, generic systems
			// initramfs-linux.img : Arch Linux specific naming
			base == "initrd",           // SUSE/openSUSE, generic
			base == "initramfs-linux.img": // Arch Linux
			add(&initrds, path)
		}
		return nil
	})

	sort.Strings(loaderEntries)
	sort.Strings(grubConfigs)
	sort.Strings(kernels)
	sort.Strings(initrds)
	return loaderEntries, grubConfigs, kernels, initrds
}

// commandExists checks if a command is available in PATH
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// inspectBinaryForKernel checks if a file appears to be a Linux kernel binary
// Uses external tools (file, strings, readelf) if available in the environment
// Returns false if tools are unavailable (graceful degradation)
func inspectBinaryForKernel(filePath string) bool {
	hasFile := commandExists("file")
	hasStrings := commandExists("strings")
	hasReadelf := commandExists("readelf")

	// If no inspection tools available, return false and rely on pattern matching
	if !hasFile && !hasStrings && !hasReadelf {
		return false
	}

	// Check file type using 'file' command
	if hasFile {
		if out, err := exec.Command("file", filePath).Output(); err == nil {
			output := strings.ToLower(string(out))
			// Look for kernel indicators
			if strings.Contains(output, "linux kernel") ||
			   (strings.Contains(output, "elf") && strings.Contains(output, "x86")) {
				return true
			}
		}
	}

	// Check for kernel strings using 'strings'
	if hasStrings {
		if out, err := exec.Command("strings", "-n", "10", filePath).Output(); err == nil {
			output := strings.ToLower(string(out))
			// Look for kernel-specific strings
			kernelIndicators := []string{
				"linux version",
				"kernel_version",
				"init/main.c",
				"arch/x86",
				"setup_arch",
				"start_kernel",
				"linux_banner",
			}
			for _, indicator := range kernelIndicators {
				if strings.Contains(output, indicator) {
					return true
				}
			}
		}
	}

	// Check ELF header for kernel-like properties
	if hasReadelf {
		if out, err := exec.Command("readelf", "-h", filePath).Output(); err == nil {
			output := strings.ToLower(string(out))
			// Kernels are typically ELF executables for x86
			if strings.Contains(output, "executable") &&
			   (strings.Contains(output, "x86-64") || strings.Contains(output, "80386")) {
				return true
			}
		}
	}

	return false
}

// inspectBinaryForInitrd checks if a file appears to be an initrd/initramfs
// Uses external tools (file, strings, gzip) if available in the environment
// Returns false if tools are unavailable (graceful degradation)
func inspectBinaryForInitrd(filePath string) bool {
	hasFile := commandExists("file")
	hasStrings := commandExists("strings")
	hasGzip := commandExists("gzip")

	// If no inspection tools available, return false and rely on pattern matching
	if !hasFile && !hasStrings && !hasGzip {
		return false
	}

	// Check file type using 'file' command
	if hasFile {
		if out, err := exec.Command("file", filePath).Output(); err == nil {
			output := strings.ToLower(string(out))
			// Look for initrd indicators
			if strings.Contains(output, "cpio") ||
			   strings.Contains(output, "initrd") ||
			   strings.Contains(output, "initramfs") ||
			   strings.Contains(output, "gzip compressed") ||
			   strings.Contains(output, "ascii cpio") {
				return true
			}
		}
	}

	// Check if it's gzip compressed (common for initrds)
	if hasGzip {
		if err := exec.Command("gzip", "-t", filePath).Run(); err == nil {
			// If gzip test passes, check if decompressed content looks like cpio
			if out, err := exec.Command("sh", "-c", "gzip -dc "+filePath+" | head -c 100").Output(); err == nil {
				// Look for cpio magic numbers
				if strings.Contains(string(out), "070701") || strings.Contains(string(out), "070702") {
					return true
				}
			}
		}
	}

	// Check for initrd-like strings
	if hasStrings {
		if out, err := exec.Command("strings", "-n", "5", filePath).Output(); err == nil {
			output := strings.ToLower(string(out))
			initrdIndicators := []string{
				"/init",
				"initramfs",
				"rdinit=",
				"rootfs",
				"pivot_root",
				"switch_root",
				"busybox",
				"udev",
				"systemd",
			}
			for _, indicator := range initrdIndicators {
				if strings.Contains(output, indicator) {
					return true
				}
			}
		}
	}

	return false
}

// enhancedCollectBootFiles extends collectBootFiles with binary inspection
// Uses filename patterns AND binary content analysis to identify kernels/initrds
// This provides compatibility with non-standard naming conventions and custom builds
// Gracefully degrades: if inspection tools (file, strings, readelf) are unavailable,
// relies solely on pattern matching which handles ~95% of real-world cases
func enhancedCollectBootFiles(mountPoint string) ([]string, []string, []string, []string) {
	loaderEntries, grubConfigs, kernels, initrds := collectBootFiles(mountPoint)

	// Additional binary inspection for potential kernels/initrds with non-standard names
	// Only performed if system tools are available; gracefully skipped otherwise
	_ = filepath.WalkDir(mountPoint, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}

		base := filepath.Base(path)

		// Skip files we've already identified
		for _, k := range kernels {
			if filepath.Base(k) == base {
				return nil
			}
		}
		for _, i := range initrds {
			if filepath.Base(i) == base {
				return nil
			}
		}

		// Inspect unknown binaries (returns false if tools unavailable)
		if inspectBinaryForKernel(path) {
			kernels = append(kernels, path)
		} else if inspectBinaryForInitrd(path) {
			initrds = append(initrds, path)
		}

		return nil
	})

	sort.Strings(kernels)
	sort.Strings(initrds)
	return loaderEntries, grubConfigs, kernels, initrds
}

func trimMountPrefix(mountPoint, path string) string {
	if rel, err := filepath.Rel(mountPoint, path); err == nil && rel != "." {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}

func snapshotMountFiles(mountPoint string, limit int) []string {
	files := make([]string, 0, limit)
	_ = filepath.WalkDir(mountPoint, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if path == mountPoint {
			return nil
		}
		files = append(files, trimMountPrefix(mountPoint, path))
		if len(files) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func resolveBootPath(mountPoint, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	ref = strings.Trim(ref, `"'`)
	ref = strings.TrimPrefix(ref, "(")
	if idx := strings.Index(ref, ")"); idx >= 0 {
		ref = ref[idx+1:]
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.HasPrefix(ref, "/") {
		ref = "/" + ref
	}
	path := filepath.Clean(filepath.Join(mountPoint, filepath.FromSlash(ref)))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func splitKernelLine(line string) (string, string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return "", "", false
	}
	if len(fields) == 2 {
		return fields[1], "", true
	}
	return fields[1], strings.Join(fields[2:], " "), true
}

func splitInitrdLine(line string) []string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return nil
	}
	return fields[1:]
}

func kernelVersionSuffix(base string) string {
	switch {
	case strings.HasPrefix(base, "vmlinuz-"):
		return strings.TrimPrefix(base, "vmlinuz-")
	case strings.HasPrefix(base, "linux-"):
		return strings.TrimPrefix(base, "linux-")
	default:
		return ""
	}
}

func isMemtestKernelBase(base string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	return strings.Contains(base, "memtest")
}

func initrdVersionSuffix(base string) string {
	switch {
	case strings.HasPrefix(base, "initrd.img-"):
		return strings.TrimPrefix(base, "initrd.img-")
	case strings.HasPrefix(base, "initramfs-"):
		suffix := strings.TrimPrefix(base, "initramfs-")
		suffix = strings.TrimSuffix(suffix, ".img")
		suffix = strings.TrimSuffix(suffix, ".gz")
		suffix = strings.TrimSuffix(suffix, ".xz")
		return suffix
	default:
		return ""
	}
}

func makeBootEntry(kernelRef string, initrdRefs []string, cmdline, source string) BootEntry {
	kernelRef = strings.TrimSpace(kernelRef)
	kernelBase := filepath.Base(kernelRef)
	entry := BootEntry{
		KernelRef:    kernelRef,
		KernelBase:   kernelBase,
		KernelSuffix: kernelVersionSuffix(kernelBase),
		InitrdRefs:   append([]string(nil), initrdRefs...),
		Cmdline:      strings.TrimSpace(cmdline),
		Source:       source,
	}
	for _, ref := range initrdRefs {
		base := filepath.Base(strings.TrimSpace(ref))
		entry.InitrdBases = append(entry.InitrdBases, base)
		entry.InitrdSuffix = append(entry.InitrdSuffix, initrdVersionSuffix(base))
	}
	return entry
}

func parseLoaderEntryCatalog(files []string) []BootEntry {
	entries := make([]BootEntry, 0)
	sort.Strings(files)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var kernelRef, cmdline string
		var initrdRefs []string
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(t, "linux "):
				ref, options, ok := splitKernelLine(t)
				if !ok {
					continue
				}
				kernelRef = ref
				if options != "" {
					cmdline = options
				}
			case strings.HasPrefix(t, "initrd "):
				initrdRefs = append(initrdRefs, splitInitrdLine(t)...)
			case strings.HasPrefix(t, "options "):
				cmdline = strings.TrimSpace(strings.TrimPrefix(t, "options "))
			}
		}
		if kernelRef == "" {
			continue
		}
		if isMemtestKernelBase(filepath.Base(kernelRef)) {
			continue
		}
		entries = append(entries, makeBootEntry(kernelRef, initrdRefs, cmdline, file))
	}
	return entries
}

func trimGrubValue(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"'`)
	return raw
}

func expandGrubVars(s string, vars map[string]string) string {
	out := s
	for i := 0; i < 4; i++ {
		next := out
		for key, value := range vars {
			next = strings.ReplaceAll(next, "${"+key+"}", value)
			next = strings.ReplaceAll(next, "$"+key, value)
		}
		if next == out {
			break
		}
		out = next
	}
	return out
}

func resolveGrubConfigRef(mountPoint, ref string, vars map[string]string) string {
	ref = expandGrubVars(trimGrubValue(ref), vars)
	if ref == "" {
		return ""
	}
	ref = strings.TrimPrefix(ref, "(")
	if idx := strings.Index(ref, ")"); idx >= 0 {
		ref = ref[idx+1:]
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.HasPrefix(ref, "/") {
		ref = "/" + ref
	}
	path := filepath.Clean(filepath.Join(mountPoint, filepath.FromSlash(ref)))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func grubConfigChain(grubPath, mountPoint string) []string {
	visited := make(map[string]struct{})
	var out []string

	var walk func(path string)
	walk = func(path string) {
		if path == "" {
			return
		}
		if _, ok := visited[path]; ok {
			return
		}
		visited[path] = struct{}{}
		out = append(out, path)

		data, err := os.ReadFile(path)
		if err != nil {
			return
		}

		vars := map[string]string{
			"prefix": filepath.ToSlash(strings.TrimPrefix(filepath.Dir(path), mountPoint)),
			"root":   "",
		}
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(t, "set "):
				assignment := strings.TrimSpace(strings.TrimPrefix(t, "set "))
				key, value, ok := strings.Cut(assignment, "=")
				if !ok {
					continue
				}
				key = strings.TrimSpace(key)
				if key == "" {
					continue
				}
				vars[key] = trimGrubValue(expandGrubVars(value, vars))
			case strings.HasPrefix(t, "configfile "):
				ref := strings.TrimSpace(strings.TrimPrefix(t, "configfile "))
				if next := resolveGrubConfigRef(mountPoint, ref, vars); next != "" {
					walk(next)
				}
			}
		}
	}

	walk(grubPath)
	return out
}

func parseGrubConfigCatalog(grubPath, mountPoint string) []BootEntry {
	var entries []BootEntry
	for _, path := range grubConfigChain(grubPath, mountPoint) {
		entries = append(entries, parseSingleGrubConfigCatalog(path)...)
	}
	return entries
}

func parseSingleGrubConfigCatalog(grubPath string) []BootEntry {
	data, err := os.ReadFile(grubPath)
	if err != nil {
		return nil
	}

	// Parse GRUB variables for cmdline expansion
	vars := parseGrubVars(grubPath)

	entries := make([]BootEntry, 0)
	// Parse lines with continuation handling
	lines := parseGrubLinesWithContinuation(string(data))
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "linux ") && !strings.HasPrefix(t, "linuxefi ") {
			continue
		}
		kernelRef, cmdline, ok := splitKernelLine(t)
		if !ok {
			continue
		}

		// Expand GRUB variables in cmdline
		cmdline = expandGrubVars(cmdline, vars)

		var initrdRefs []string
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if strings.HasPrefix(next, "linux ") || strings.HasPrefix(next, "linuxefi ") || strings.HasPrefix(next, "menuentry ") {
				break
			}
			if strings.HasPrefix(next, "initrd ") || strings.HasPrefix(next, "initrdefi ") {
				initrdRefs = append(initrdRefs, splitInitrdLine(next)...)
			}
		}
		if isMemtestKernelBase(filepath.Base(kernelRef)) {
			continue
		}
		entries = append(entries, makeBootEntry(kernelRef, initrdRefs, cmdline, grubPath))
	}
	return entries
}

func collectBootCatalog(mountPoint string) []BootEntry {
	loaderEntries, grubConfigs, _, _ := enhancedCollectBootFiles(mountPoint)
	entries := parseLoaderEntryCatalog(loaderEntries)
	for _, grubPath := range grubConfigs {
		entries = append(entries, parseGrubConfigCatalog(grubPath, mountPoint)...)
	}
	return entries
}

func matchBootEntryCmdline(entries []BootEntry, kernel, initrd string) (string, string, bool) {
	kernelBase := filepath.Base(kernel)
	initrdBase := filepath.Base(initrd)
	kernelSuffix := kernelVersionSuffix(kernelBase)
	initrdSuffix := initrdVersionSuffix(initrdBase)

	matchPair := func(entry BootEntry, bySuffix bool) bool {
		for i := range entry.InitrdBases {
			if bySuffix {
				if entry.KernelSuffix != "" && entry.KernelSuffix == kernelSuffix && i < len(entry.InitrdSuffix) && entry.InitrdSuffix[i] != "" && entry.InitrdSuffix[i] == initrdSuffix {
					return true
				}
				continue
			}
			if entry.KernelBase == kernelBase && entry.InitrdBases[i] == initrdBase {
				return true
			}
		}
		return false
	}

	for _, entry := range entries {
		if strings.TrimSpace(entry.Cmdline) == "" {
			continue
		}
		if matchPair(entry, false) {
			return entry.Cmdline, entry.Source, true
		}
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Cmdline) == "" {
			continue
		}
		if entry.KernelBase == kernelBase {
			return entry.Cmdline, entry.Source, true
		}
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Cmdline) == "" {
			continue
		}
		if matchPair(entry, true) {
			return entry.Cmdline, entry.Source, true
		}
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Cmdline) == "" {
			continue
		}
		if entry.KernelSuffix != "" && entry.KernelSuffix == kernelSuffix {
			return entry.Cmdline, entry.Source, true
		}
	}
	return "", "", false
}

func looksWeakCmdline(cmdline string) bool {
	fields := strings.Fields(strings.TrimSpace(cmdline))
	if len(fields) == 0 {
		return true
	}
	meaningfulPrefixes := []string{
		"root=",
		"rootfstype=",
		"rootflags=",
		"boot=",
		"resume=",
		"cryptdevice=",
		"rd.luks",
		"rd.lvm",
		"rd.md",
		"rd.dm",
		"zfs=",
		"root=ZFS=",
		"btrfs=",
		"subvol=",
		"rw",
		"ro",
		"initrd=",
		"init=",
		"systemd.unit=",
		"security=",
		"apparmor=",
		"selinux=",
		"audit=",
		"quiet",
		"splash",
		"vga=",
		"video=",
		"console=",
		"panic=",
		"maxcpus=",
		"nr_cpus=",
		"mem=",
		"iomem=",
		"acpi=",
		"pci=",
		"usbcore.",
		"ehci_hcd.",
		"ohci_hcd.",
		"uhci_hcd.",
		"xhci_hcd.",
		"scsi_mod.",
		"sd_mod.",
		"ahci.",
		"ata_piix.",
		"virtio_",
		"9p",
		"virtiofs",
	}
	for _, field := range fields {
		for _, prefix := range meaningfulPrefixes {
			if strings.HasPrefix(field, prefix) {
				return false
			}
		}
	}
	return true
}

func summarizeBootEntry(entry BootEntry) string {
	initrds := strings.Join(entry.InitrdBases, "|")
	if initrds == "" {
		initrds = "-"
	}
	cmdline := strings.TrimSpace(entry.Cmdline)
	if cmdline == "" {
		cmdline = "<empty>"
	}
	return fmt.Sprintf("kernel=%s initrd=%s source=%s cmdline=%s", entry.KernelBase, initrds, trimMountPrefix("/mnt/proxmox", entry.Source), cmdline)
}

func looksLikeRootFilesystem(mountPoint string) bool {
	required := []string{"etc", "usr", "var"}
	for _, name := range required {
		info, err := os.Stat(filepath.Join(mountPoint, name))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

func synthesizeRootCmdline(device, existing string) (string, bool) {
	device = strings.TrimSpace(device)
	if device == "" {
		return "", false
	}

	fields := strings.Fields(strings.TrimSpace(existing))
	out := make([]string, 0, len(fields)+2)
	out = append(out, "root="+device, "ro")
	for _, field := range fields {
		if strings.HasPrefix(field, "root=") || field == "ro" || field == "rw" {
			continue
		}
		out = append(out, field)
	}
	return strings.Join(out, " "), true
}

func findBootFromLoaderEntryFiles(mountPoint string, files []string) (string, string, string, bool) {
	if len(files) == 0 {
		return "", "", "", false
	}

	sort.Strings(files)
	for i := len(files) - 1; i >= 0; i-- {
		data, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}
		var kernelPath, cmdline string
		var initrdRefs []string
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(t, "linux "):
				kernelRef, options, ok := splitKernelLine(t)
				if !ok {
					continue
				}
				kernelPath = resolveBootPath(mountPoint, kernelRef)
				cmdline = options
			case strings.HasPrefix(t, "initrd "):
				initrdRefs = append(initrdRefs, splitInitrdLine(t)...)
			case strings.HasPrefix(t, "options "):
				cmdline = strings.TrimSpace(strings.TrimPrefix(t, "options "))
			}
		}
		if kernelPath == "" {
			continue
		}
		for _, initrdRef := range initrdRefs {
			if initrdPath := resolveBootPath(mountPoint, initrdRef); initrdPath != "" {
				return kernelPath, initrdPath, cmdline, true
			}
		}
	}
	return "", "", "", false
}

func findBootFromLoaderEntries(mountPoint string) (string, string, string, bool) {
	files, _, _, _ := collectBootFiles(mountPoint)
	return findBootFromLoaderEntryFiles(mountPoint, files)
}

func findBootFromGrubConfig(grubPath, mountPoint string) (string, string, string, bool) {
	for _, path := range grubConfigChain(grubPath, mountPoint) {
		if kernel, initrd, cmdline, ok := findBootFromSingleGrubConfig(path, mountPoint); ok {
			return kernel, initrd, cmdline, true
		}
	}
	return "", "", "", false
}

func parseGrubLinesWithContinuation(data string) []string {
	rawLines := strings.Split(data, "\n")
	var lines []string
	var currentLine string

	for _, rawLine := range rawLines {
		trimmed := strings.TrimSpace(rawLine)
		// Handle line continuation (backslash at end)
		if strings.HasSuffix(trimmed, "\\") {
			currentLine += strings.TrimSuffix(trimmed, "\\")
			continue
		}
		currentLine += trimmed
		if currentLine != "" {
			lines = append(lines, currentLine)
		}
		currentLine = ""
	}

	// Handle case where last line has continuation
	if currentLine != "" {
		lines = append(lines, currentLine)
	}

	return lines
}

func parseGrubVars(grubPath string) map[string]string {
	data, err := os.ReadFile(grubPath)
	if err != nil {
		return nil
	}

	vars := map[string]string{
		"prefix": filepath.ToSlash(filepath.Dir(grubPath)),
		"root":   "",
	}

	lines := parseGrubLinesWithContinuation(string(data))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "set "):
			assignment := strings.TrimSpace(strings.TrimPrefix(t, "set "))
			key, value, ok := strings.Cut(assignment, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			vars[key] = trimGrubValue(expandGrubVars(value, vars))
		}
	}
	return vars
}

func findBootFromSingleGrubConfig(grubPath, mountPoint string) (string, string, string, bool) {
	data, err := os.ReadFile(grubPath)
	if err != nil {
		return "", "", "", false
	}

	// Parse GRUB variables for cmdline expansion
	vars := parseGrubVars(grubPath)

	// Parse lines with continuation handling
	lines := parseGrubLinesWithContinuation(string(data))
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "linux ") && !strings.HasPrefix(t, "linuxefi ") {
			continue
		}
		kernelRef, cmdline, ok := splitKernelLine(t)
		if !ok {
			continue
		}

		// Expand GRUB variables in cmdline
		cmdline = expandGrubVars(cmdline, vars)

		kernelPath := resolveBootPath(mountPoint, kernelRef)
		if kernelPath == "" {
			continue
		}

		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if strings.HasPrefix(next, "linux ") || strings.HasPrefix(next, "linuxefi ") || strings.HasPrefix(next, "menuentry ") {
				break
			}
			if strings.HasPrefix(next, "initrd ") || strings.HasPrefix(next, "initrdefi ") {
				for _, initrdRef := range splitInitrdLine(next) {
					if initrdPath := resolveBootPath(mountPoint, initrdRef); initrdPath != "" {
						return kernelPath, initrdPath, cmdline, true
					}
				}
			}
		}
	}

	return "", "", "", false
}

func matchKernelInitrdPair(kernels, initrds []string) (string, string, bool) {
	initrdBySuffix := make(map[string]string, len(initrds))
	for _, initrd := range initrds {
		base := filepath.Base(initrd)
		switch {
		case strings.HasPrefix(base, "initrd.img-"):
			initrdBySuffix[strings.TrimPrefix(base, "initrd.img-")] = initrd
		case strings.HasPrefix(base, "initramfs-"):
			suffix := strings.TrimPrefix(base, "initramfs-")
			suffix = strings.TrimSuffix(suffix, ".img")
			suffix = strings.TrimSuffix(suffix, ".gz")
			suffix = strings.TrimSuffix(suffix, ".xz")
			initrdBySuffix[suffix] = initrd
		}
	}

	sort.Strings(kernels)
	for i := len(kernels) - 1; i >= 0; i-- {
		kernel := kernels[i]
		base := filepath.Base(kernel)
		var suffix string
		switch {
		case strings.HasPrefix(base, "vmlinuz-"):
			suffix = strings.TrimPrefix(base, "vmlinuz-")
		case strings.HasPrefix(base, "linux-"):
			suffix = strings.TrimPrefix(base, "linux-")
		default:
			continue
		}
		if initrd, ok := initrdBySuffix[suffix]; ok {
			return kernel, initrd, true
		}
	}
	return "", "", false
}

func findBootArtifacts(mountPoint string) (string, string, string, bool) {
	loaderEntries, grubConfigs, kernels, initrds := enhancedCollectBootFiles(mountPoint)

	if kernel, initrd, cmdline, ok := findBootFromLoaderEntryFiles(mountPoint, loaderEntries); ok {
		return kernel, initrd, cmdline, true
	}
	for _, grubPath := range grubConfigs {
		if kernel, initrd, cmdline, ok := findBootFromGrubConfig(grubPath, mountPoint); ok {
			return kernel, initrd, cmdline, true
		}
	}

	if kernel, initrd, ok := matchKernelInitrdPair(kernels, initrds); ok {
		cmdline, err := findBootCmdline(mountPoint, kernel)
		if err == nil {
			return kernel, initrd, cmdline, true
		}
		return kernel, initrd, "", true
	}
	return "", "", "", false
}

func appendBootDebug(debug *[]string, format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	*debug = append(*debug, line)
	recordBootLaunchDebug(line)
}

// BootSystem mounts the first unlocked drive's bootable partition, loads the
// Proxmox kernel and initrd via kexec, then executes kexec to transfer control.
func BootSystem() (*BootResult, error) {
	drives := scanDrives()
	debug := make([]string, 0, 32)
	bootCandidates := bootCandidateDrives(drives)
	var locked []string
	for _, d := range drives {
		if !d.Opal {
			continue
		}
		if d.Locked {
			locked = append(locked, d.Device)
		}
	}
	if len(bootCandidates) == 0 {
		appendBootDebug(&debug, "No startup-locked OPAL drive has transitioned to unlocked.")
		return nil, BootAttemptError{
			Message: "boot is unavailable until a startup-locked OPAL drive is unlocked",
			Debug:   debug,
		}
	}
	appendBootDebug(&debug, "Boot candidate drives: %s", strings.Join(bootCandidates, ", "))

	fullyUnlocked := len(locked) == 0
	var warning string
	if !fullyUnlocked {
		warning = fmt.Sprintf("WARNING: locked drives: %s", strings.Join(locked, ", "))
		appendBootDebug(&debug, "%s", warning)
	}

	mountPoint := "/mnt/proxmox"
	os.MkdirAll(mountPoint, 0755)

	tools := availableRescanTools()
	if len(tools) == 0 {
		appendBootDebug(&debug, "Partition rescan tools available: none")
	} else {
		appendBootDebug(&debug, "Partition rescan tools available: %s", strings.Join(tools, ", "))
	}
	appendBootDebug(&debug, "Refreshing partition tables for boot candidates.")
	for _, bootDrive := range bootCandidates {
		rescanBlockDeviceLayout(bootDrive)
		if partitions, err := listDevicePartitions(bootDrive); err == nil {
			appendBootDebug(&debug, "Visible partitions after refresh for %s: %s", bootDrive, strings.Join(partitions, ", "))
			for _, part := range partitions {
				appendBootDebug(&debug, "Block type for %s: %s", part, probeBlockType(part))
			}
		}
	}

	lvmTools := availableLVMTools()
	if len(lvmTools) == 0 {
		appendBootDebug(&debug, "LVM tools available: none")
	} else {
		appendBootDebug(&debug, "LVM tools available: %s", strings.Join(lvmTools, ", "))
	}
	activateLVM()
	if lvs := listLogicalVolumes(); len(lvs) == 0 {
		appendBootDebug(&debug, "Logical volumes detected after activation: none")
	} else {
		appendBootDebug(&debug, "Logical volumes detected after activation: %s", strings.Join(lvs, ", "))
	}

	searchDevices, err := buildBootSearchDevices(bootCandidates)
	if err != nil {
		return nil, err
	}
	appendBootDebug(&debug, "Mount search targets: %s", strings.Join(searchDevices, ", "))
	bootCatalog := make([]BootEntry, 0, 8)
	for _, dev := range searchDevices {
		appendBootDebug(&debug, "Trying mount target: %s", dev)
		if err := exec.Command("mount", "-r", dev, mountPoint).Run(); err != nil {
			appendBootDebug(&debug, "Mount failed: %s", err)
			continue
		}

		if entries, err := os.ReadDir(mountPoint); err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			appendBootDebug(&debug, "Mount root contents: %s", strings.Join(names, " "))
		}

		unmount := func() { exec.Command("umount", mountPoint).Run() }
		appendBootDebug(&debug, "Mounted %s on %s", dev, mountPoint)
		entries := collectBootCatalog(mountPoint)
		if len(entries) > 0 {
			bootCatalog = append(bootCatalog, entries...)
			appendBootDebug(&debug, "Cataloged %d boot entries from %s", len(entries), dev)
			for i, entry := range entries {
				if i >= 4 {
					appendBootDebug(&debug, "Additional boot entries on %s omitted: %d", dev, len(entries)-i)
					break
				}
				appendBootDebug(&debug, "Boot entry %d on %s: %s", i+1, dev, summarizeBootEntry(entry))
			}
		}

		if kernel, initrd, cmdline, ok := findBootArtifacts(mountPoint); ok {
			if matchedCmdline, source, matched := matchBootEntryCmdline(bootCatalog, kernel, initrd); matched {
				if strings.TrimSpace(cmdline) == "" || strings.TrimSpace(cmdline) != strings.TrimSpace(matchedCmdline) {
					cmdline = matchedCmdline
					appendBootDebug(&debug, "Matched cmdline from boot catalog for %s using %s", dev, source)
				}
			}
			if looksWeakCmdline(cmdline) && looksLikeRootFilesystem(mountPoint) {
				if synthesized, ok := synthesizeRootCmdline(dev, cmdline); ok {
					cmdline = synthesized
					appendBootDebug(&debug, "Synthesized root cmdline for %s", dev)
				}
			}
			appendBootDebug(&debug, "Found kernel: %s", kernel)
			appendBootDebug(&debug, "Found initrd: %s", initrd)
			appendBootDebug(&debug, "Found cmdline: %s", cmdline)
			if strings.TrimSpace(cmdline) == "" {
				appendBootDebug(&debug, "Refusing to kexec with an empty kernel command line.")
				unmount()
				return nil, BootAttemptError{
					Message: "unable to determine kernel command line for boot target",
					Debug:   debug,
				}
			}
			if looksWeakCmdline(cmdline) {
				appendBootDebug(&debug, "Refusing to kexec with a weak kernel command line: %s", cmdline)
				unmount()
				return nil, BootAttemptError{
					Message: "kernel command line looks incomplete for boot target",
					Debug:   debug,
				}
			}

			if err := exec.Command("kexec", "-l", kernel, "--initrd="+initrd, "--append="+cmdline).Run(); err != nil {
				appendBootDebug(&debug, "kexec -l failed: %s", err)
				unmount()
				return nil, BootAttemptError{Message: err.Error(), Debug: debug}
			}
			unmount()
			
			// Signal success before kexec -e (network will disappear)
			result := &BootResult{
				Kernel:        kernel,
				Initrd:        initrd,
				Cmdline:       cmdline,
				Drives:        drives,
				Warning:       warning,
				FullyUnlocked: fullyUnlocked,
				Debug:         debug,
			}
			finishBootLaunch(result, nil)  // Signal success to UI
			// Signal main() to shut down the HTTP server and fire kexec -e.
			// We must not call kexec -e here — doing so from inside a live
			// HTTP server goroutine causes it to fail silently because the Go
			// runtime has active goroutines making syscalls. Instead we hand
			// control to main(), which shuts the server down cleanly first.
			close(kexecReady)
			// Block until main() attempts kexec -e and reports back.
			// On success kexec -e never returns so this channel receives only
			// on failure, letting us surface the error through bootLaunchState.
			if err := <-kexecFailed; err != nil {
				return nil, BootAttemptError{
					Message: fmt.Sprintf("kexec -e failed: %v", err),
					Debug:   debug,
				}
			}
			// Unreachable on success — kexec -e replaces the kernel.
			return result, nil
		}
		loaderEntries, grubConfigs, kernels, initrds := collectBootFiles(mountPoint)
		appendBootDebug(&debug, "Detected loader entries: %d, grub configs: %d, kernels: %d, initrds: %d", len(loaderEntries), len(grubConfigs), len(kernels), len(initrds))
		if files := snapshotMountFiles(mountPoint, 40); len(files) > 0 {
			appendBootDebug(&debug, "Mounted file snapshot: %s", strings.Join(files, ", "))
		}
		appendBootDebug(&debug, "No boot artifacts found on %s", dev)
		unmount()
	}
	appendBootDebug(&debug, "Boot search exhausted without finding a usable kernel/initrd pair.")
	return nil, BootAttemptError{Message: "no bootable partition", Debug: debug}
}

// extractLinuxCmdline parses a GRUB "linux" or "linuxefi" line.
func extractLinuxCmdline(line string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return "", false
	}
	if len(fields) == 2 {
		return "", true
	}
	return strings.Join(fields[2:], " "), true
}

func parseQuotedShellValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if unquoted, err := strconv.Unquote(raw); err == nil {
		return unquoted
	}
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

func parseDefaultGrubCmdline(mountPoint string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(mountPoint, "etc", "default", "grub"))
	if err != nil {
		return "", false, err
	}

	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, value, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "GRUB_CMDLINE_LINUX" && key != "GRUB_CMDLINE_LINUX_DEFAULT" {
			continue
		}
		values[key] = strings.TrimSpace(parseQuotedShellValue(value))
	}

	parts := make([]string, 0, 2)
	if v := values["GRUB_CMDLINE_LINUX"]; v != "" {
		parts = append(parts, v)
	}
	if v := values["GRUB_CMDLINE_LINUX_DEFAULT"]; v != "" {
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		return "", false, fmt.Errorf("kernel command line not found in /etc/default/grub")
	}
	return strings.Join(parts, " "), true, nil
}

func parseKernelCmdlineFile(mountPoint string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(mountPoint, "etc", "kernel", "cmdline"))
	if err != nil {
		return "", false, err
	}
	cmdline := strings.TrimSpace(string(data))
	if cmdline == "" {
		return "", false, fmt.Errorf("kernel command line not found in /etc/kernel/cmdline")
	}
	return cmdline, true, nil
}

// findBootCmdline tries loader entries then grub configs in priority order.
func findBootCmdline(mountPoint, kernel string) (string, error) {
	kernelBase := filepath.Base(kernel)
	loaderEntries, grubConfigs, _, _ := collectBootFiles(mountPoint)

	candidates := []func() (string, bool, error){
		func() (string, bool, error) {
			if len(loaderEntries) == 0 {
				return "", false, fmt.Errorf("no loader entries under %s", mountPoint)
			}
			for _, f := range loaderEntries {
				data, err := os.ReadFile(f)
				if err != nil {
					continue
				}
				var kernelMatch bool
				var options string
				for _, line := range strings.Split(string(data), "\n") {
					t := strings.TrimSpace(line)
					if strings.HasPrefix(t, "linux ") {
						kernelMatch = kernelBase == "" || strings.Contains(t, kernelBase)
					} else if strings.HasPrefix(t, "options ") {
						options = strings.TrimSpace(strings.TrimPrefix(t, "options "))
					}
				}
				if kernelMatch {
					return options, true, nil
				}
			}
			return "", false, fmt.Errorf("kernel command line not found in loader entries")
		},
		func() (string, bool, error) {
			return parseGrubCfg(filepath.Join(mountPoint, "boot", "grub", "grub.cfg"), mountPoint, kernelBase)
		},
		func() (string, bool, error) {
			return parseGrubCfg(filepath.Join(mountPoint, "boot", "grub2", "grub.cfg"), mountPoint, kernelBase)
		},
		func() (string, bool, error) {
			return parseGrubCfg(filepath.Join(mountPoint, "grub", "grub.cfg"), mountPoint, kernelBase)
		},
		func() (string, bool, error) {
			return parseKernelCmdlineFile(mountPoint)
		},
		func() (string, bool, error) {
			return parseDefaultGrubCmdline(mountPoint)
		},
	}
	for _, grubPath := range grubConfigs {
		path := grubPath
		candidates = append(candidates, func() (string, bool, error) {
			return parseGrubCfg(path, mountPoint, kernelBase)
		})
	}

	for _, try := range candidates {
		if cmdline, found, err := try(); err == nil && found {
			return cmdline, nil
		}
	}
	return "", fmt.Errorf("unable to determine target kernel command line from %s", mountPoint)
}

// parseGrubCfg extracts the kernel command line from a grub.cfg file and any
// configfile targets reachable within the mounted filesystem.
func parseGrubCfg(grubPath, mountPoint, kernelBase string) (string, bool, error) {
	for _, path := range grubConfigChain(grubPath, mountPoint) {
		if cmdline, found, err := parseSingleGrubCfg(path, kernelBase); err == nil && found {
			return cmdline, true, nil
		}
	}
	return "", false, fmt.Errorf("kernel command line not found in %s", grubPath)
}

func parseSingleGrubCfg(grubPath, kernelBase string) (string, bool, error) {
	data, err := os.ReadFile(grubPath)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "linux ") && !strings.HasPrefix(t, "linuxefi ") {
			continue
		}
		if kernelBase != "" && !strings.Contains(t, kernelBase) {
			continue
		}
		if cmdline, ok := extractLinuxCmdline(t); ok {
			return cmdline, true, nil
		}
	}
	return "", false, fmt.Errorf("kernel command line not found in %s", grubPath)
}

// ============================================================
// CONSOLE TUI
// ============================================================

const privacyTimeout = 30 * time.Second

const (
	colorReset  = "\033[0m"
	colorBlue   = "\033[38;5;32m"
	colorPurple = "\033[38;5;91m"
	colorOrange = "\033[38;5;208m"
	colorDim    = "\033[38;5;245m"
)

func consoleInterface() {
	fd := int(os.Stdin.Fd())
	for {
		fmt.Print("\033[H\033[2J")
		fmt.Println(colorBlue + "🔒 PBA STANDBY" + colorReset)
		printConsoleStatus(currentStatus())
		fmt.Println("\n" + colorDim + "Press any key..." + colorReset)

		old, err := term.MakeRaw(fd)
		if err != nil {
			log.Printf("term.MakeRaw failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		term.Restore(fd, old)
		activeMenu(fd)
	}
}

func activeMenu(fd int) {
	for {
		fmt.Print("\033[H\033[2J")
		fmt.Println(colorBlue + "🔑 ACTIVE MODE" + colorReset)
		printConsoleStatus(currentStatus())

		fmt.Printf(
			"\n[%sENTER%s] %sUnlock%s  [%sB%s] %sBoot%s  [%sP%s] %sChange PW%s  [%sR%s] %sReboot%s  [%sS%s] %sShutdown%s\n",
			colorDim, colorReset, colorPurple, colorReset,
			colorDim, colorReset, colorBlue, colorReset,
			colorDim, colorReset, colorPurple, colorReset,
			colorDim, colorReset, colorOrange, colorReset,
			colorDim, colorReset, colorBlue, colorReset,
		)

		old, err := term.MakeRaw(fd)
		if err != nil {
			log.Printf("term.MakeRaw failed: %v", err)
			return
		}

		type readResult struct {
			b   byte
			err error
		}
		ch := make(chan readResult, 1)
		go func() {
			buf := make([]byte, 1)
			_, err := os.Stdin.Read(buf)
			ch <- readResult{buf[0], err}
		}()

		var key string
		select {
		case res := <-ch:
			term.Restore(fd, old)
			if res.err != nil {
				return
			}
			key = strings.ToUpper(string(res.b))
		case <-time.After(privacyTimeout):
			term.Restore(fd, old)
			return
		}

		switch key {
		case "\r":
			fmt.Print("Password: ")
			pw, _ := term.ReadPassword(fd)
			if _, err := attemptUnlock(string(pw)); err != nil {
				fmt.Println("\n❌", err)
				time.Sleep(2 * time.Second)
			}

		case "B":
			res, err := BootSystem()
			if err != nil {
				fmt.Println(err)
			} else if res.Warning != "" {
				fmt.Println(res.Warning)
			}
			time.Sleep(2 * time.Second)

		case "P":
			eligible := eligiblePasswordChangeTargets(scanDrives())
			if len(eligible) == 0 {
				fmt.Println("\n❌ No unlocked OPAL drives are eligible for password change.")
				time.Sleep(3 * time.Second)
				break
			}
			fmt.Printf("\nRequirements: %s\n", passwordPolicySummary())
			fmt.Println("BIOS note: if SID password changes fail, check firmware/TPM settings and disable Block SID for the next boot.")
			devices := make([]string, 0, len(eligible))
			for _, drive := range eligible {
				devices = append(devices, drive.Device)
			}
			fmt.Printf("Target device (%s): ", strings.Join(devices, ", "))
			reader := bufio.NewReader(os.Stdin)
			deviceLine, _ := reader.ReadString('\n')
			deviceLine = strings.TrimSpace(deviceLine)
			if deviceLine == "" {
				fmt.Println("\n❌ target device is required")
				time.Sleep(2 * time.Second)
				break
			}
			fmt.Print("\nCurrent: ")
			curr, _ := term.ReadPassword(fd)
			fmt.Print("\nNew: ")
			newP, _ := term.ReadPassword(fd)
			fmt.Print("\nConfirm: ")
			conf, _ := term.ReadPassword(fd)

			if string(newP) != string(conf) {
				fmt.Println("\n❌ mismatch")
				time.Sleep(2 * time.Second)
				break
			}
			if err := validatePassword(string(newP)); err != nil {
				fmt.Println("\n❌", err)
				time.Sleep(2 * time.Second)
				break
			}
			results, err := changePassword(string(curr), string(newP), []string{deviceLine})
			if err != nil {
				fmt.Println("\n❌", err)
				time.Sleep(3 * time.Second)
				break
			}
			fmt.Println()
			anySuccess := false
			for _, result := range results {
				if result.Success {
					anySuccess = true
					fmt.Printf("✅ %s: %s\n", result.Device, result.Detail)
					continue
				}
				msg := result.Error
				if msg == "" {
					msg = "password change failed"
				}
				if result.Detail != "" {
					msg += " (" + result.Detail + ")"
				}
				fmt.Printf("❌ %s: %s\n", result.Device, msg)
			}
			if !anySuccess {
				fmt.Println("❌ No drive accepted the new unlock password.")
			}
			time.Sleep(4 * time.Second)

		case "R":
			exec.Command("reboot", "-nf").Run()

		case "S":
			exec.Command("poweroff", "-nf").Run()
		}
	}
}

// ============================================================
// HTTP HELPERS
// ============================================================

func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		jsonResponse(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return true
	}
	return false
}

func runSedutil(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sedutil-cli", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("sedutil-cli timed out")
	}
	if err != nil {
		if len(out) > 0 {
			return string(out), fmt.Errorf("sedutil-cli failed: %v", err)
		}
		return "", fmt.Errorf("sedutil-cli failed: %v", err)
	}
	return string(out), nil
}

func runExpertCommand(w http.ResponseWriter, args ...string) {
	out, err := runSedutil(45*time.Second, args...)
	resp := map[string]string{
		"command": strings.Join(append([]string{"sedutil-cli"}, args...), " "),
		"output":  strings.TrimSpace(out),
	}
	if err != nil {
		resp["error"] = err.Error()
		jsonResponse(w, http.StatusBadRequest, resp)
		return
	}
	resp["status"] = "ok"
	jsonResponse(w, http.StatusOK, resp)
}

func firstExistingPath(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func startSSHService() {
	dropbearBin := firstExistingPath("/usr/local/sbin/dropbear", "/usr/sbin/dropbear", "/usr/local/bin/dropbear")
	if dropbearBin == "" {
		if multi := firstExistingPath("/usr/local/bin/dropbearmulti", "/usr/bin/dropbearmulti"); multi != "" {
			symlinkPath := "/tmp/dropbear"
			_ = os.Remove(symlinkPath)
			if err := os.Symlink(multi, symlinkPath); err == nil {
				dropbearBin = symlinkPath
			} else {
				log.Printf("[ssh] failed to prepare dropbearmulti symlink: %v", err)
			}
		}
	}
	if dropbearBin == "" {
		log.Println("[ssh] dropbear not present; SSH UI disabled")
		return
	}

	ecdsaKey := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_ecdsa_host_key",
		"/etc/dropbear/dropbear_ecdsa_host_key",
	)
	ed25519Key := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_ed25519_host_key",
		"/etc/dropbear/dropbear_ed25519_host_key",
	)
	rsaKey := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_rsa_host_key",
		"/etc/dropbear/dropbear_rsa_host_key",
	)
	if ed25519Key == "" && ecdsaKey == "" && rsaKey == "" {
		log.Println("[ssh] dropbear keys not present; SSH UI disabled")
		return
	}

	args := []string{
		"-R",
		"-E",
		"-F",
		"-p", "2222",
	}
	if ed25519Key != "" {
		args = append(args, "-r", ed25519Key)
	}
	if ecdsaKey != "" {
		args = append(args, "-r", ecdsaKey)
	}
	if rsaKey != "" {
		args = append(args, "-r", rsaKey)
	}
	if banner := firstExistingPath("/usr/local/etc/dropbear/banner", "/etc/dropbear/banner"); banner != "" {
		args = append(args, "-b", banner)
	}

	cmd := exec.Command(dropbearBin, args...)
	if err := cmd.Start(); err != nil {
		log.Printf("[ssh] failed to start dropbear: %v", err)
		return
	}
	log.Printf("[ssh] dropbear started on port 2222 (pid %d)", cmd.Process.Pid)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("[ssh] dropbear exited: %v", err)
		}
	}()
}

func makeSystemActionHandler(label string, args ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": label})
		go func() {
			time.Sleep(500 * time.Millisecond)
			exec.Command(args[0], args[1:]...).Run()
		}()
	}
}

// ============================================================
// MAIN — HTTP SERVER
// ============================================================

func main() {
	initializeBootState()
	go consoleInterface()
	startSSHService()

	httpErrorLog := log.New(filteredHTTPLogWriter{}, "", 0)

	go func() {
		redirectSrv := &http.Server{
			Addr: ":80",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
			}),
			ErrorLog: httpErrorLog,
		}
		if err := redirectSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[http] redirect server failed: %v", err)
		}
	}()

	mux := http.NewServeMux()

	mux.Handle("/", http.FileServer(http.Dir("static")))

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, currentStatus())
	})

	mux.HandleFunc("/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, DiagnosticsResponse{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Drives:      collectDriveDiagnostics(),
		})
	})

	mux.HandleFunc("/password-policy", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, passwordPolicy)
	})

	mux.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		results, err := attemptUnlock(req.Password)
		if err != nil {
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		token := ""
		if anySuccessfulUnlock(results) {
			var mintErr error
			token, mintErr = mintSessionToken()
			if mintErr != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session token"})
				return
			}
		}
		jsonResponse(w, http.StatusOK, UnlockResponse{Results: results, Token: token})
	})

	mux.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireSessionToken(w, r) {
			return
		}
		var req struct {
			CurrentPassword string   `json:"currentPassword"`
			NewPassword     string   `json:"newPassword"`
			Devices         []string `json:"devices"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if err := validatePassword(req.NewPassword); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		results, err := changePassword(req.CurrentPassword, req.NewPassword, req.Devices)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, PasswordChangeResponse{
			Results: results,
		})
	})

	mux.HandleFunc("/expert/auth", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if expertPasswordHash == "" {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "expert auth is not configured"})
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(expertPasswordHash), []byte(req.Password)); err != nil {
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": "invalid expert password"})
			return
		}
		token, err := mintExpertToken()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to create expert session"})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"token": token})
	})

	mux.HandleFunc("/expert/status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, map[string]bool{
			"configured":    expertPasswordHash != "",
			"authenticated": validExpertToken(r.Header.Get("X-Expert-Token")),
		})
	})

	mux.HandleFunc("/expert/revert-tper", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}
		var req struct {
			Device   string `json:"device"`
			Password string `json:"password"`
			Confirm  string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if !strings.HasPrefix(req.Device, "/dev/") {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a /dev path"})
			return
		}
		if req.Confirm != "REVERT" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "confirmation text must be REVERT"})
			return
		}
		runExpertCommand(w, "--reverttper", req.Password, req.Device)
	})

	mux.HandleFunc("/expert/psid-revert", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}
		var req struct {
			Device  string `json:"device"`
			PSID    string `json:"psid"`
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if !strings.HasPrefix(req.Device, "/dev/") {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a /dev path"})
			return
		}
		if strings.TrimSpace(req.PSID) == "" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "PSID is required"})
			return
		}
		if req.Confirm != "ERASE" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "confirmation text must be ERASE"})
			return
		}
		runExpertCommand(w, "--yesIreallywanttoERASEALLmydatausingthePSID", req.PSID, req.Device)
	})

	mux.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireSessionTokenOrUnlockedDrive(w, r) {
			return
		}
		if err := startBootLaunch(); err != nil {
			jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "boot requested"})
	})

	mux.HandleFunc("/boot-status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, currentBootLaunchStatus())
	})

	mux.HandleFunc("/reboot", makeSystemActionHandler("rebooting", "reboot", "-nf"))
	mux.HandleFunc("/poweroff", makeSystemActionHandler("powering off", "poweroff", "-nf"))

	httpsSrv := &http.Server{
		Addr:     ":443",
		Handler:  mux,
		ErrorLog: httpErrorLog,
	}
	// Run the HTTPS server in a goroutine so main() can remain unblocked
	// and handle the kexec handoff when a web boot is requested.
	go func() {
		if err := httpsSrv.ListenAndServeTLS("server.crt", "server.key"); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTPS server error: %v", err)
		}
	}()
	// Wait for BootSystem to signal that kexec -l succeeded and it is ready
	// for us to fire kexec -e. This only triggers on web-initiated boots;
	// console and SSH boots call BootSystem() synchronously and never close
	// this channel, so they are unaffected.
	<-kexecReady
	// Shut the HTTP server down before calling kexec -e. A live server with
	// active goroutines making syscalls will cause kexec -e to fail silently.
	// We use a short timeout so the current response has time to flush.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 1*time.Second)
	httpsSrv.Shutdown(shutdownCtx)
	shutdownCancel()
	// Brief pause to let TLS write buffers drain to the client.
	time.Sleep(200 * time.Millisecond)
	if err := exec.Command("kexec", "-e").Run(); err != nil {
		// kexec -e returned — it failed. Send the error back to BootSystem
		// so it can populate bootLaunchState with a useful message.
		kexecFailed <- err
		// Restart the HTTPS server so the web UI can reconnect and display
		// the error via /boot-status rather than getting a dead connection.
		log.Printf("kexec -e failed: %v — restarting HTTPS server", err)
		if err := httpsSrv.ListenAndServeTLS("server.crt", "server.key"); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTPS server restart failed: %v", err)
		}
	}
	// kexec -e succeeded — the kernel has been replaced. Never reached.
	select {}
}

async function boot() {
    try {
        await postJSON("/boot", {}, authHeaders())
        // Don't poll - just assume success like SSH does
        setBootUiBusy(true)
        $("bootResult").innerText = "Booting..."
        // The page will become unresponsive when kexec succeeds
    } catch (err) {
        setBootUiBusy(false)
        $("bootResult").innerText = err.message || "Boot failed"
    }
}
