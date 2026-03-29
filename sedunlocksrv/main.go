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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
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
	"golang.org/x/term"
)

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

	flashMu    sync.RWMutex
	flashState FlashStatus

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
		return strings.ToLower(v) == "true" || strings.ToLower(v) == "on"
	}
	getInt := func(k string, def int) int {
		if i, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return i
		}
		return def
	}

	// Check if password complexity is globally disabled
	complexityEnabled := getBool("PASSWORD_COMPLEXITY_ON", true)

	// If complexity is disabled, return a policy with no requirements
	if !complexityEnabled {
		return PasswordPolicy{
			MinLength:      0,
			RequireUpper:   false,
			RequireLower:   false,
			RequireNumber:  false,
			RequireSpecial: false,
		}
	}

	// Otherwise, apply the configured requirements
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
		// Auto-reset if the previous boot has been stuck for over 2 minutes
		if time.Since(bootLaunchState.StartedAt) > 2*time.Minute {
			log.Printf("Auto-resetting stale boot-in-progress state (started %s ago)", time.Since(bootLaunchState.StartedAt).Round(time.Second))
			resetBootLaunchStateLocked()
		} else {
			return fmt.Errorf("boot is already in progress")
		}
	}
	resetBootLaunchStateLocked()
	bootLaunchState.InProgress = true
	bootLaunchState.StartedAt = time.Now()
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
		finishBootLaunch(result, err)
	}()
	return nil
}

func startBootLaunchWithKernel(kernelIndex int) error {
	if err := beginBootLaunch(); err != nil {
		return err
	}
	go func() {
		result, err := BootSystemWithKernel(kernelIndex)
		finishBootLaunch(result, err)
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

func isOpal2Drive(device string) bool {
	for _, d := range scanDrives() {
		if d.Device == device {
			return d.Opal
		}
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
	// If all complexity requirements are disabled, return a simple message
	if passwordPolicy.MinLength == 0 &&
		!passwordPolicy.RequireUpper &&
		!passwordPolicy.RequireLower &&
		!passwordPolicy.RequireNumber &&
		!passwordPolicy.RequireSpecial {
		return "no complexity requirements"
	}

	parts := make([]string, 0)
	if passwordPolicy.MinLength > 0 {
		parts = append(parts, fmt.Sprintf("min %d chars", passwordPolicy.MinLength))
	}
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

	if len(parts) == 0 {
		return "no complexity requirements"
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

// === REMOVED: isLinuxKernel()
// Detected kernels by ELF header + embedded string scanning. Removed in favor of
// fast filename-pattern matching. For custom-named kernels, edit collectBootFiles().

// === REMOVED: isInitrd()
// Detected initrds by magic number + cpio header + string scanning. Removed in favor of
// fast filename-pattern matching. For custom-named initrds, edit collectBootFiles().

// collectBootFiles scans for boot artifacts using filename patterns only (fast).
// Supports major Linux distributions:
// - Debian/Ubuntu: vmlinuz-*, initrd.img-*
// - Fedora/RHEL/CentOS: vmlinuz-*, initramfs-*
// - SUSE/openSUSE: linux, linux-*, initrd, initramfs-*
// - Arch Linux: vmlinuz, vmlinuz-*, initramfs-*, initramfs-linux.img
// - NixOS: bzImage, initrd
// - Proxmox: vmlinuz-*, initrd.img-*
// - Generic: bzImage, linux, initrd
//
// Custom kernel naming: If using custom kernel/initrd names, add them to the
// case statement below. For example, if your kernel is named "kernel.custom",
// add: case strings.HasPrefix(base, "kernel.custom"):
func collectBootFiles(mountPoint string) ([]string, []string, []string, []string) {
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
		case strings.HasPrefix(base, "vmlinuz-"),
			strings.HasPrefix(base, "linux-"),
			base == "vmlinuz",
			base == "linux",
			base == "bzImage":
			add(&kernels, path)
		case strings.HasPrefix(base, "initrd.img-"),
			strings.HasPrefix(base, "initramfs-"),
			base == "initrd",
			base == "initramfs-linux.img":
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
	loaderEntries, grubConfigs, _, _ := collectBootFiles(mountPoint)
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

// isValidCmdlineForDevice checks if a kernel cmdline's root= device
// matches the device we're currently booting from. This prevents using
// cmdlines meant for other drives (e.g., encrypted sda when we're on nvme0).
func isValidCmdlineForDevice(cmdline, device string) bool {
	// Extract root= value from cmdline
	var rootDevice string
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "root=") {
			rootDevice = strings.TrimPrefix(field, "root=")
			break
		}
	}

	if rootDevice == "" {
		// No root= found; can't validate, so allow it (might be weak but valid)
		return true
	}

	// Allow non-device-path root specifiers (UUID, PARTUUID, LABEL, ZFS, etc.)
	// These cannot be meaningfully compared to a block device path.
	for _, prefix := range []string{"UUID=", "PARTUUID=", "LABEL=", "PARTLABEL=", "ZFS=", "/dev/mapper/", "/dev/dm-"} {
		if strings.HasPrefix(rootDevice, prefix) {
			return true
		}
	}

	// Allow unexpanded GRUB variables (e.g., ${cmdline_root})
	if strings.Contains(rootDevice, "${") || strings.Contains(rootDevice, "$") {
		return true
	}

	// Normalize device names for comparison
	// e.g., /dev/nvme0n1p2 or nvme0n1p2
	deviceBase := filepath.Base(device)
	rootBase := filepath.Base(rootDevice)

	// If root device is a mapper or contains a hyphen (LVM style), be permissive
	if strings.Contains(rootBase, "mapper") || strings.Contains(rootBase, "-") {
		return true
	}

	// Compare disk families: strip partition numbers to get base disk name
	currentDisk := strings.TrimRight(strings.TrimRight(deviceBase, "0123456789"), "p")
	rootDisk := strings.TrimRight(strings.TrimRight(rootBase, "0123456789"), "p")

	// Reject only if both are plain /dev/ paths on clearly different physical drives
	if currentDisk != "" && rootDisk != "" && currentDisk != rootDisk {
		return false
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
		rawCmdline := cmdline
		cmdline = expandGrubVars(cmdline, vars)
		if rawCmdline != cmdline {
			recordBootLaunchDebug(fmt.Sprintf("grub-config %s: expanded cmdline %q -> %q", filepath.Base(grubPath), rawCmdline, cmdline))
		}

		kernelPath := resolveBootPath(mountPoint, kernelRef)
		if kernelPath == "" {
			recordBootLaunchDebug(fmt.Sprintf("grub-config %s: kernel ref %q did not resolve on filesystem", filepath.Base(grubPath), kernelRef))
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
						recordBootLaunchDebug(fmt.Sprintf("grub-config %s: returning kernel=%s initrd=%s cmdline=%q", filepath.Base(grubPath), filepath.Base(kernelPath), filepath.Base(initrdPath), cmdline))
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

func findBootArtifacts(mountPoint, device string) (string, string, string, bool) {
	loaderEntries, grubConfigs, kernels, initrds := collectBootFiles(mountPoint)

	if kernel, initrd, cmdline, ok := findBootFromLoaderEntryFiles(mountPoint, loaderEntries); ok {
		recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: found via loader entries: kernel=%s cmdline=%q", filepath.Base(kernel), cmdline))
		// If cmdline looks weak, try to augment it via the full fallback chain
		if looksWeakCmdline(cmdline) {
			recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: loader entry cmdline is weak, trying findBootCmdline fallback"))
			if betterCmdline, err := findBootCmdline(mountPoint, kernel, device); err == nil {
				recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: findBootCmdline returned: %q", betterCmdline))
				cmdline = betterCmdline
			} else {
				recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: findBootCmdline failed: %v", err))
			}
		}
		return kernel, initrd, cmdline, true
	}
	for _, grubPath := range grubConfigs {
		if kernel, initrd, cmdline, ok := findBootFromGrubConfig(grubPath, mountPoint); ok {
			recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: found via grub config %s: kernel=%s cmdline=%q", trimMountPrefix(mountPoint, grubPath), filepath.Base(kernel), cmdline))
			// If cmdline looks weak, try to augment it via the full fallback chain
			if looksWeakCmdline(cmdline) {
				recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: grub config cmdline is weak, trying findBootCmdline fallback"))
				if betterCmdline, err := findBootCmdline(mountPoint, kernel, device); err == nil {
					recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: findBootCmdline returned: %q", betterCmdline))
					cmdline = betterCmdline
				} else {
					recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: findBootCmdline failed: %v", err))
				}
			}
			return kernel, initrd, cmdline, true
		}
	}

	if kernel, initrd, ok := matchKernelInitrdPair(kernels, initrds); ok {
		recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: matched kernel/initrd pair: kernel=%s initrd=%s, searching for cmdline...", filepath.Base(kernel), filepath.Base(initrd)))
		cmdline, err := findBootCmdline(mountPoint, kernel, device)
		if err == nil {
			return kernel, initrd, cmdline, true
		}
		recordBootLaunchDebug(fmt.Sprintf("findBootArtifacts: findBootCmdline failed for matched pair: %v", err))
		return kernel, initrd, "", true
	}
	recordBootLaunchDebug("findBootArtifacts: no boot artifacts found")
	return "", "", "", false
}

func listAvailableBootKernelsWithDebug() ([]BootKernelInfo, []string, error) {
	debug := make([]string, 0, 64)
	appendDebug := func(format string, args ...interface{}) {
		line := fmt.Sprintf(format, args...)
		debug = append(debug, line)
		recordBootLaunchDebug(line)
	}

	drives := scanDrives()
	bootCandidates := bootCandidateDrives(drives)
	appendDebug("Boot candidates: %s", strings.Join(bootCandidates, ", "))

	if len(bootCandidates) == 0 {
		return nil, debug, fmt.Errorf("no startup-locked OPAL drive has transitioned to unlocked")
	}

	mountPoint := "/mnt/proxmox"
	_ = os.MkdirAll(mountPoint, 0755)

	// Activate LVM in case kernels are on LVM volumes
	activateLVM()

	searchDevices, err := buildBootSearchDevices(bootCandidates)
	if err != nil {
		appendDebug("buildBootSearchDevices failed: %v", err)
		return nil, debug, err
	}
	appendDebug("Search devices: %s", strings.Join(searchDevices, ", "))

	kernels := make([]BootKernelInfo, 0, 8)

	for _, dev := range searchDevices {
		appendDebug("Trying mount target: %s", dev)
		if err := runCommandTimeout(4*time.Second, "mount", "-r", dev, mountPoint); err != nil {
			appendDebug("Mount failed for %s: %v", dev, err)
			continue
		}

		unmount := func() {
			if err := runCommandTimeout(3*time.Second, "umount", mountPoint); err != nil {
				appendDebug("Unmount failed for %s: %v", dev, err)
			}
		}
		appendDebug("Mounted %s on %s", dev, mountPoint)

		// Log what collectBootFiles found on this mount
		loaderEntries, grubConfigs, rawKernels, rawInitrds := collectBootFiles(mountPoint)
		appendDebug("collectBootFiles on %s: loaders=%d grubs=%d kernels=%d initrds=%d", dev, len(loaderEntries), len(grubConfigs), len(rawKernels), len(rawInitrds))
		for _, g := range grubConfigs {
			appendDebug("  grub.cfg found: %s", trimMountPrefix(mountPoint, g))
		}
		for _, k := range rawKernels {
			appendDebug("  kernel found: %s", trimMountPrefix(mountPoint, k))
		}
		for _, i := range rawInitrds {
			appendDebug("  initrd found: %s", trimMountPrefix(mountPoint, i))
		}

		// Collect all boot entries from this mount
		entries := collectBootCatalog(mountPoint)
		appendDebug("Boot catalog entries on %s: %d", dev, len(entries))
		for i, entry := range entries {
			appendDebug("  catalog[%d]: kernel=%s cmdline=%q source=%s", i, entry.KernelBase, entry.Cmdline, trimMountPrefix(mountPoint, entry.Source))
		}

		// Also collect raw kernels/initrds for matching
		if rawKernel, rawInitrd, cmdline, ok := findBootArtifacts(mountPoint, dev); ok {
			name := filepath.Base(rawKernel)
			initrdName := filepath.Base(rawInitrd)
			kernels = append(kernels, BootKernelInfo{
				Index:      len(kernels),
				Device:     dev,
				Kernel:     rawKernel,
				KernelName: name,
				Initrd:     rawInitrd,
				InitrdName: initrdName,
				Cmdline:    cmdline,
				Source:     "discovered",
			})
			appendDebug("Discovered kernel/initrd on %s: %s | %s | cmdline=%q", dev, rawKernel, rawInitrd, cmdline)
			if looksWeakCmdline(cmdline) {
				appendDebug("WARNING: discovered cmdline looks weak: %q", cmdline)
			}
		} else {
			appendDebug("No raw kernel/initrd pair discovered on %s", dev)
		}

		// Add entries from boot catalog as alternatives
		for _, entry := range entries {
			if entry.KernelRef != "" && len(entry.InitrdRefs) > 0 {
				kernels = append(kernels, BootKernelInfo{
					Index:      len(kernels),
					Device:     dev,
					Kernel:     entry.KernelRef,
					KernelName: filepath.Base(entry.KernelRef),
					Initrd:     entry.InitrdRefs[0],
					InitrdName: filepath.Base(entry.InitrdRefs[0]),
					Cmdline:    entry.Cmdline,
					Source:     entry.Source,
				})
			}
		}

		appendDebug("Kernel candidates accumulated so far: %d", len(kernels))
		unmount()
	}

	if len(kernels) == 0 {
		appendDebug("Boot search exhausted with zero kernels")
		return nil, debug, fmt.Errorf("no kernels found on boot devices")
	}

	appendDebug("Boot search finished with %d kernel candidates", len(kernels))
	return kernels, debug, nil
}

// ListAvailableBootKernels discovers all available kernel/initrd pairs without booting.
// Returns a slice of BootKernelInfo for selection by the user.
func ListAvailableBootKernels() ([]BootKernelInfo, error) {
	kernels, _, err := listAvailableBootKernelsWithDebug()
	return kernels, err
}

// BootSystemWithKernel boots with a specific kernel selected by index.
// If kernelIndex < 0, uses the first available kernel.
func BootSystemWithKernel(kernelIndex int) (*BootResult, error) {
	kernels, discoverDebug, err := listAvailableBootKernelsWithDebug()
	if err != nil {
		return nil, BootAttemptError{
			Message: err.Error(),
			Debug:   append(discoverDebug, err.Error()),
		}
	}

	if len(kernels) == 0 {
		return nil, BootAttemptError{
			Message: "no kernels available",
			Debug:   []string{"no kernels found"},
		}
	}

	// Validate kernel index
	if kernelIndex < 0 || kernelIndex >= len(kernels) {
		kernelIndex = 0
	}

	selected := kernels[kernelIndex]
	debug := make([]string, 0, 32)

	// Get boot drives setup (reuse existing logic)
	drives := scanDrives()
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

	for _, line := range discoverDebug {
		appendBootDebug(&debug, "kernel-discovery: %s", line)
	}
	appendBootDebug(&debug, "Boot candidate drives: %s", strings.Join(bootCandidates, ", "))
	appendBootDebug(&debug, "Selected kernel index %d: %s (%s)", kernelIndex, selected.KernelName, selected.Cmdline)

	if selected.Device == "" {
		appendBootDebug(&debug, "Selected kernel has no source device metadata")
		return nil, BootAttemptError{
			Message: "selected kernel source is unknown",
			Debug:   debug,
		}
	}

	mountPoint := "/mnt/proxmox"
	_ = os.MkdirAll(mountPoint, 0755)
	if err := runCommandTimeout(4*time.Second, "mount", "-r", selected.Device, mountPoint); err != nil {
		appendBootDebug(&debug, "Failed to mount selected device %s: %v", selected.Device, err)
		return nil, BootAttemptError{
			Message: fmt.Sprintf("failed to mount selected boot device: %v", err),
			Debug:   debug,
		}
	}
	unmount := func() { _ = runCommandTimeout(3*time.Second, "umount", mountPoint) }
	defer unmount()

	fullyUnlocked := len(locked) == 0
	var warning string
	if !fullyUnlocked {
		warning = fmt.Sprintf("WARNING: locked drives: %s", strings.Join(locked, ", "))
		appendBootDebug(&debug, "%s", warning)
	}

	// Verify selected kernel and initrd exist
	if _, err := os.Stat(selected.Kernel); err != nil {
		appendBootDebug(&debug, "Selected kernel not found: %s", selected.Kernel)
		return nil, BootAttemptError{
			Message: "selected kernel not found",
			Debug:   debug,
		}
	}
	if _, err := os.Stat(selected.Initrd); err != nil {
		appendBootDebug(&debug, "Selected initrd not found: %s", selected.Initrd)
		return nil, BootAttemptError{
			Message: "selected initrd not found",
			Debug:   debug,
		}
	}

	appendBootDebug(&debug, "Found kernel: %s", selected.Kernel)
	appendBootDebug(&debug, "Found initrd: %s", selected.Initrd)
	appendBootDebug(&debug, "Found cmdline: %s", selected.Cmdline)

	// If cmdline is empty or weak, try to re-discover a better one
	if strings.TrimSpace(selected.Cmdline) == "" || looksWeakCmdline(selected.Cmdline) {
		appendBootDebug(&debug, "Cmdline is weak or empty (%q), attempting re-discovery...", selected.Cmdline)

		// Try findBootCmdline with full fallback chain
		if betterCmdline, err := findBootCmdline(mountPoint, selected.Kernel, selected.Device); err == nil && !looksWeakCmdline(betterCmdline) {
			appendBootDebug(&debug, "findBootCmdline found better cmdline: %s", betterCmdline)
			selected.Cmdline = betterCmdline
		} else {
			if err != nil {
				appendBootDebug(&debug, "findBootCmdline failed: %v", err)
			} else {
				appendBootDebug(&debug, "findBootCmdline returned weak cmdline: %q", betterCmdline)
			}
		}

		// Try boot catalog matching
		if looksWeakCmdline(selected.Cmdline) || strings.TrimSpace(selected.Cmdline) == "" {
			catalog := collectBootCatalog(mountPoint)
			appendBootDebug(&debug, "Re-checking boot catalog (%d entries) for a better cmdline", len(catalog))
			if matchedCmdline, source, matched := matchBootEntryCmdline(catalog, selected.Kernel, selected.Initrd); matched && !looksWeakCmdline(matchedCmdline) {
				appendBootDebug(&debug, "Boot catalog provided better cmdline from %s: %s", source, matchedCmdline)
				selected.Cmdline = matchedCmdline
			} else if matched {
				appendBootDebug(&debug, "Boot catalog matched but cmdline still weak: %q (source: %s)", matchedCmdline, source)
			}
		}

		// Try /etc/kernel/cmdline and /etc/default/grub
		if looksWeakCmdline(selected.Cmdline) || strings.TrimSpace(selected.Cmdline) == "" {
			if cmdline, found, err := parseKernelCmdlineFile(mountPoint); err == nil && found && !looksWeakCmdline(cmdline) {
				appendBootDebug(&debug, "/etc/kernel/cmdline provided: %s", cmdline)
				selected.Cmdline = cmdline
			} else if cmdline, found, err := parseDefaultGrubCmdline(mountPoint); err == nil && found && !looksWeakCmdline(cmdline) {
				appendBootDebug(&debug, "/etc/default/grub provided: %s", cmdline)
				selected.Cmdline = cmdline
			}
		}

		// Try synthesizing root= from the mount device if it looks like a root filesystem
		if looksWeakCmdline(selected.Cmdline) && looksLikeRootFilesystem(mountPoint) {
			if synthesized, ok := synthesizeRootCmdline(selected.Device, selected.Cmdline); ok {
				appendBootDebug(&debug, "Synthesized root cmdline from device %s: %s", selected.Device, synthesized)
				selected.Cmdline = synthesized
			}
		}

		appendBootDebug(&debug, "Final cmdline after re-discovery: %s", selected.Cmdline)
	}

	if strings.TrimSpace(selected.Cmdline) == "" {
		appendBootDebug(&debug, "Refusing to kexec with an empty kernel command line.")
		return nil, BootAttemptError{
			Message: "unable to determine kernel command line for boot target",
			Debug:   debug,
		}
	}
	if looksWeakCmdline(selected.Cmdline) {
		appendBootDebug(&debug, "Refusing to kexec with a weak kernel command line: %s", selected.Cmdline)
		return nil, BootAttemptError{
			Message: "kernel command line looks incomplete for boot target",
			Debug:   debug,
		}
	}

	// Load kernel with kexec
	if err := exec.Command("kexec", "-l", selected.Kernel, "--initrd="+selected.Initrd, "--append="+selected.Cmdline).Run(); err != nil {
		appendBootDebug(&debug, "kexec -l failed: %s", err)
		return nil, BootAttemptError{Message: err.Error(), Debug: debug}
	}
	unmount()

	appendBootDebug(&debug, "kexec -l succeeded.")
	result := &BootResult{
		Kernel:        selected.Kernel,
		Initrd:        selected.Initrd,
		Cmdline:       selected.Cmdline,
		Drives:        drives,
		Warning:       warning,
		FullyUnlocked: fullyUnlocked,
		Debug:         debug,
	}

	// Signal main() to shut down HTTPS cleanly and execute kexec -e.
	close(kexecReady)
	if err := <-kexecFailed; err != nil {
		return nil, BootAttemptError{
			Message: fmt.Sprintf("kexec -e failed: %v", err),
			Debug:   debug,
		}
	}
	return result, nil
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
		if err := runCommandTimeout(4*time.Second, "mount", "-r", dev, mountPoint); err != nil {
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

		unmount := func() {
			if err := runCommandTimeout(3*time.Second, "umount", mountPoint); err != nil {
				appendBootDebug(&debug, "Unmount failed for %s: %v", dev, err)
			}
		}
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

		if kernel, initrd, cmdline, ok := findBootArtifacts(mountPoint, dev); ok {
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
			finishBootLaunch(result, nil) // Signal success to UI
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

// findBootCmdline tries loader entries then grub configs in priority order,
// but also tries fallback sources if the primary result looks weak.
func findBootCmdline(mountPoint, kernel, device string) (string, error) {
	kernelBase := filepath.Base(kernel)
	loaderEntries, grubConfigs, _, _ := collectBootFiles(mountPoint)

	type namedCandidate struct {
		name string
		fn   func() (string, bool, error)
	}

	candidates := []namedCandidate{
		{"loader-entries", func() (string, bool, error) {
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
		}},
		// EFI-specific GRUB paths (checked early for EFI systems)
		{"efi-grub-configs", func() (string, bool, error) {
			// Search for GRUB config in /boot/efi/EFI/*/grub.cfg (Proxmox, Ubuntu EFI, etc.)
			efiDir := filepath.Join(mountPoint, "boot", "efi", "EFI")
			entries, err := os.ReadDir(efiDir)
			if err == nil {
				for _, entry := range entries {
					if !entry.IsDir() {
						continue
					}
					grubPath := filepath.Join(efiDir, entry.Name(), "grub.cfg")
					if cmdline, found, err := parseGrubCfg(grubPath, mountPoint, kernelBase); err == nil && found {
						return cmdline, true, nil
					}
				}
			}
			return "", false, fmt.Errorf("kernel command line not found in EFI GRUB configs")
		}},
		{"boot/grub/grub.cfg", func() (string, bool, error) {
			return parseGrubCfg(filepath.Join(mountPoint, "boot", "grub", "grub.cfg"), mountPoint, kernelBase)
		}},
		{"boot/grub2/grub.cfg", func() (string, bool, error) {
			return parseGrubCfg(filepath.Join(mountPoint, "boot", "grub2", "grub.cfg"), mountPoint, kernelBase)
		}},
		{"grub/grub.cfg", func() (string, bool, error) {
			return parseGrubCfg(filepath.Join(mountPoint, "grub", "grub.cfg"), mountPoint, kernelBase)
		}},
		{"boot-dir-walk", func() (string, bool, error) {
			var bestCmdline string
			var bestFound bool

			// Check targeted paths where grub.cfg is typically found
			grubSearchDirs := []string{
				filepath.Join(mountPoint, "boot"),
				filepath.Join(mountPoint, "efi"),
				filepath.Join(mountPoint, "grub"),
				filepath.Join(mountPoint, "grub2"),
			}

			for _, searchDir := range grubSearchDirs {
				if _, err := os.Stat(searchDir); err != nil {
					continue
				}
				_ = filepath.WalkDir(searchDir, func(path string, d fs.DirEntry, err error) error {
					if err != nil || d == nil || d.IsDir() {
						return nil
					}
					if filepath.Base(path) == "grub.cfg" {
						if cmdline, found, err := parseGrubCfg(path, mountPoint, kernelBase); err == nil && found && cmdline != "" {
							if !looksWeakCmdline(cmdline) {
								bestCmdline = cmdline
								bestFound = true
								return fs.SkipAll
							}
							if bestCmdline == "" {
								bestCmdline = cmdline
								bestFound = true
							}
						}
					}
					return nil
				})
				if bestFound && !looksWeakCmdline(bestCmdline) {
					break
				}
			}

			if bestFound {
				return bestCmdline, true, nil
			}
			return "", false, fmt.Errorf("no grub.cfg found in boot directories")
		}},
		{"/etc/kernel/cmdline", func() (string, bool, error) {
			return parseKernelCmdlineFile(mountPoint)
		}},
		{"/etc/default/grub", func() (string, bool, error) {
			return parseDefaultGrubCmdline(mountPoint)
		}},
	}
	for _, grubPath := range grubConfigs {
		path := grubPath
		candidates = append(candidates, namedCandidate{"extra-grub:" + trimMountPrefix(mountPoint, path), func() (string, bool, error) {
			return parseGrubCfg(path, mountPoint, kernelBase)
		}})
	}

	// Try candidates in order, but if we find a weak cmdline, keep trying
	// to see if a later candidate has a better one.
	var bestCmdline string
	for _, c := range candidates {
		cmdline, found, err := c.fn()
		if err != nil || !found || cmdline == "" {
			recordBootLaunchDebug(fmt.Sprintf("findBootCmdline[%s]: no result (err=%v)", c.name, err))
			continue
		}
		recordBootLaunchDebug(fmt.Sprintf("findBootCmdline[%s]: found %q", c.name, cmdline))
		// Skip cmdlines that point to a different device (e.g., sda when we're on nvme0)
		if !isValidCmdlineForDevice(cmdline, device) {
			recordBootLaunchDebug(fmt.Sprintf("findBootCmdline[%s]: rejected by isValidCmdlineForDevice (device=%s)", c.name, device))
			continue
		}
		// If this is strong (has meaningful content), return immediately
		if !looksWeakCmdline(cmdline) {
			recordBootLaunchDebug(fmt.Sprintf("findBootCmdline[%s]: accepted (strong)", c.name))
			return cmdline, nil
		}
		recordBootLaunchDebug(fmt.Sprintf("findBootCmdline[%s]: weak, saving as fallback", c.name))
		// Otherwise, save it and keep trying for a better one
		if bestCmdline == "" {
			bestCmdline = cmdline
		}
	}

	// If we found at least a weak cmdline, return it rather than error
	if bestCmdline != "" {
		return bestCmdline, nil
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
	vars := parseGrubVars(grubPath)
	lines := parseGrubLinesWithContinuation(string(data))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "linux ") && !strings.HasPrefix(t, "linuxefi ") {
			continue
		}
		if kernelBase != "" && !strings.Contains(t, kernelBase) {
			continue
		}
		if cmdline, ok := extractLinuxCmdline(t); ok {
			rawCmdline := cmdline
			cmdline = expandGrubVars(cmdline, vars)
			recordBootLaunchDebug(fmt.Sprintf("parseSingleGrubCfg(%s): linux line matched, raw=%q expanded=%q", filepath.Base(grubPath), rawCmdline, cmdline))
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

func validateUploadedPBAImageBytes(imageData []byte, filename string) ([]string, error) {
	validation := make([]string, 0, 12)
	if len(imageData) <= 0 {
		return validation, fmt.Errorf("uploaded image is empty")
	}
	validation = append(validation, fmt.Sprintf("file size: %d bytes", len(imageData)))
	if len(imageData) > 128<<20 {
		return validation, fmt.Errorf("uploaded image exceeds 128 MiB")
	}
	validation = append(validation, "size is within the 128 MiB OPAL2 guideline")
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".img" && ext != ".bin" {
		return validation, fmt.Errorf("uploaded image must end in .img or .bin")
	}
	validation = append(validation, "filename extension is acceptable")

	if len(imageData) < 512 {
		return validation, fmt.Errorf("uploaded image is too small to contain a valid MBR")
	}

	mbr := imageData[0:512]
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return validation, fmt.Errorf("uploaded image is missing the MBR signature")
	}
	validation = append(validation, "MBR signature 0x55AA is present")

	part1 := mbr[446:462]
	bootFlag := part1[0]
	partType := part1[4]
	startLBA := binary.LittleEndian.Uint32(part1[8:12])
	sectorCount := binary.LittleEndian.Uint32(part1[12:16])

	if bootFlag != 0x80 {
		return validation, fmt.Errorf("uploaded image does not have a bootable first partition")
	}
	validation = append(validation, "first partition is bootable")
	if partType != 0xEF {
		return validation, fmt.Errorf("uploaded image first partition is 0x%02x, expected 0xEF", partType)
	}
	validation = append(validation, "first partition type matches sfdisk recipe (0xEF)")
	if startLBA == 0 || sectorCount == 0 {
		return validation, fmt.Errorf("uploaded image first partition is invalid")
	}
	validation = append(validation, fmt.Sprintf("first partition geometry looks valid (start LBA %d, sectors %d)", startLBA, sectorCount))

	for i := 1; i < 4; i++ {
		entry := mbr[446+i*16 : 446+(i+1)*16]
		if entry[4] != 0 || binary.LittleEndian.Uint32(entry[8:12]) != 0 || binary.LittleEndian.Uint32(entry[12:16]) != 0 {
			return validation, fmt.Errorf("uploaded image has unexpected extra partitions")
		}
	}
	validation = append(validation, "no unexpected extra partitions were found")

	bootSectorOffset := int64(startLBA) * 512
	bootSectorEnd := bootSectorOffset + 512
	if bootSectorEnd > int64(len(imageData)) {
		return validation, fmt.Errorf("uploaded image boot partition is unreadable")
	}

	bootSector := imageData[bootSectorOffset:bootSectorEnd]
	if bootSector[510] != 0x55 || bootSector[511] != 0xaa {
		return validation, fmt.Errorf("uploaded image first partition is missing a valid boot sector signature")
	}
	validation = append(validation, "boot partition has a valid boot sector signature")
	if !bytes.HasPrefix(bootSector, []byte{0xeb}) && !bytes.HasPrefix(bootSector, []byte{0xe9}) {
		return validation, fmt.Errorf("uploaded image first partition does not look bootable")
	}
	validation = append(validation, "boot sector has a valid jump instruction")

	bytesPerSector := binary.LittleEndian.Uint16(bootSector[11:13])
	switch bytesPerSector {
	case 512, 1024, 2048, 4096:
		validation = append(validation, fmt.Sprintf("FAT sector size is valid (%d bytes)", bytesPerSector))
	default:
		return validation, fmt.Errorf("boot partition reports invalid sector size %d", bytesPerSector)
	}

	sectorsPerCluster := bootSector[13]
	if sectorsPerCluster == 0 || sectorsPerCluster&(sectorsPerCluster-1) != 0 {
		return validation, fmt.Errorf("boot partition reports invalid sectors-per-cluster value %d", sectorsPerCluster)
	}
	validation = append(validation, fmt.Sprintf("FAT cluster size is valid (%d sectors per cluster)", sectorsPerCluster))

	reservedSectors := binary.LittleEndian.Uint16(bootSector[14:16])
	if reservedSectors == 0 {
		return validation, fmt.Errorf("boot partition reports zero reserved sectors")
	}
	validation = append(validation, fmt.Sprintf("reserved sectors present (%d)", reservedSectors))

	numberOfFATs := bootSector[16]
	if numberOfFATs == 0 {
		return validation, fmt.Errorf("boot partition reports zero FAT tables")
	}
	validation = append(validation, fmt.Sprintf("FAT table count is valid (%d)", numberOfFATs))

	fsType := strings.TrimSpace(string(bootSector[82:90]))
	if !strings.Contains(fsType, "FAT32") {
		return validation, fmt.Errorf("boot partition is not marked as FAT32")
	}
	validation = append(validation, fmt.Sprintf("filesystem type marker is %q", fsType))

	return validation, nil
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

	mux.HandleFunc("/expert/check-ram", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}
		availableRAM, err := availableRAMBytes()
		if err != nil {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to read available memory"})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"availableBytes": availableRAM,
			"source":         "/proc/meminfo:MemAvailable",
		})
	})

	mux.HandleFunc("/expert/reflash-pba", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}

		const maxUploadBytes = 128 << 20
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1024)
		// Parse with full maxUploadBytes limit for in-memory handling
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid upload; ensure image size is <= 128 MiB"})
			return
		}

		device := strings.TrimSpace(r.FormValue("device"))
		password := r.FormValue("password")
		confirm := strings.TrimSpace(r.FormValue("confirm"))

		if !strings.HasPrefix(device, "/dev/") {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a /dev path"})
			return
		}
		if !isOpal2Drive(device) {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a detected OPAL2 drive"})
			return
		}
		if strings.TrimSpace(password) == "" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "current drive password is required"})
			return
		}
		if confirm != "FLASH" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "confirmation text must be FLASH"})
			return
		}

		file, fileHeader, err := r.FormFile("image")
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "pba image file is required"})
			return
		}
		defer file.Close()

		// Read image entirely into memory
		imageBuffer := bytes.NewBuffer(make([]byte, 0, maxUploadBytes+1))
		written, err := io.Copy(imageBuffer, file)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "failed to read uploaded image"})
			return
		}
		if written == 0 {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "uploaded image is empty"})
			return
		}

		imageData := imageBuffer.Bytes()
		validation, err := validateUploadedPBAImageBytes(imageData, fileHeader.Filename)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error(), "validation": validation})
			return
		}

		if status, err := queryDrive(device); err != nil {
			validation = append(validation, fmt.Sprintf("preflight --query failed for %s: %v", device, err))
			if extra := trimSedutilOutput(status); extra != "" {
				validation = append(validation, "preflight --query output: "+extra)
			}
		} else {
			validation = append(validation, "preflight --query succeeded for selected device")
			validation = append(validation, fmt.Sprintf("preflight LockingSupported=%s", queryField(status, "LockingSupported")))
			validation = append(validation, fmt.Sprintf("preflight LockingEnabled=%s", queryField(status, "LockingEnabled")))
			validation = append(validation, fmt.Sprintf("preflight MBRDone=%s", queryField(status, "MBRDone")))
		}

		runExpertPBAFlashBytes(w, password, imageData, device, validation)
	})

	mux.HandleFunc("/expert/flash-status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		flashMu.RLock()
		state := FlashStatus{
			InProgress: flashState.InProgress,
			Done:       flashState.Done,
			Success:    flashState.Success,
			Error:      flashState.Error,
			Lines:      append([]string(nil), flashState.Lines...),
		}
		flashMu.RUnlock()
		jsonResponse(w, http.StatusOK, state)
	})

	mux.HandleFunc("/boot-list", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		if requireSessionTokenOrUnlockedDrive(w, r) {
			return
		}
		kernels, debug, err := listAvailableBootKernelsWithDebug()
		if err != nil {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]interface{}{"error": err.Error(), "debug": debug})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"kernels": kernels, "debug": debug})
	})

	mux.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireSessionTokenOrUnlockedDrive(w, r) {
			return
		}

		// Check if kernel index is provided in JSON body or query
		kernelIndex := -1
		var req struct {
			KernelIndex int `json:"kernelIndex"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.KernelIndex >= 0 {
			kernelIndex = req.KernelIndex
		}

		var err error
		if kernelIndex >= 0 {
			err = startBootLaunchWithKernel(kernelIndex)
		} else {
			err = startBootLaunch()
		}

		if err != nil {
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
