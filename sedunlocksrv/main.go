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
	"math"
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
// GLOBALS
// ============================================================
// kexecReady is closed by bootWithSelectedKernel after kexec -l succeeds,
// signalling main() to shut down the HTTP server and execute kexec -e.
// kexecFailed receives the error if kexec -e returns (i.e. fails), so the
// boot function can propagate it and main() can restart the HTTP server.
var (
	kexecReady  = make(chan struct{})
	kexecFailed = make(chan error, 1)
)

var (
	failedAttempts int
	maxAttempts    = 5
	mu             sync.Mutex
	unlockMu       sync.Mutex

	expertFailedAttempts int
	expertMu             sync.Mutex

	sessionMu         sync.RWMutex
	apiSessionToken   string
	expertSessionTok  string
	bootStateMu       sync.RWMutex
	startupLockedOpal = map[string]struct{}{}
	bootLaunchState   BootLaunchStatus
	bootCacheEpoch    uint64 = 1
	bootStateEpoch    uint64 = 1
	bootCacheValid    bool
	bootCacheError    string
	bootCacheKernels  []BootKernelInfo
	bootCacheDebug    []string

	flashMu    sync.RWMutex
	flashState FlashStatus

	passwordPolicy     = loadPolicy()
	expertPasswordHash = loadExpertPasswordHash()
)

var buildVersion = "dev"
var repoURL = ""

// Settling delays — base values at settle-factor 1.0.
// These are scaled by settleFactor (set via --settle= build flag, injected at compile time).
// Ratio is 1 : 2.5 : 5 (inter-command : partition : discovery).
const (
	baseOpalInterCmdDelay = 50 * time.Millisecond  // between setlockingrange and setmbrdone
	basePartitionSettle   = 125 * time.Millisecond // after rescanBlockDeviceLayout
	baseDiscoverySettle   = 250 * time.Millisecond // before discovery starts
)

// settleFactorStr is set at compile time via -ldflags "-X main.settleFactorStr=...".
var settleFactorStr string

// debugLevelStr is set at compile time via -ldflags "-X main.debugLevelStr=...".
var debugLevelStr string

const (
	debugVerbose = 0 // full trace
	debugNormal  = 1 // user-facing progress
	debugQuiet   = 2 // suppress logs
)

var settleFactor = parseSettleFactor()
var debugLevel = parseDebugLevel()

// parseSettleFactor reads the compile-time settleFactorStr and returns the
// multiplier applied to all hardware settle delays. Falls back to 1.0.
func parseSettleFactor() float64 {
	if settleFactorStr != "" {
		if f, err := strconv.ParseFloat(settleFactorStr, 64); err == nil && f >= 0 {
			return f
		}
	}
	return 1.0
}

// settleDelay returns a base duration scaled by the global settleFactor.
func settleDelay(base time.Duration) time.Duration {
	return time.Duration(math.Round(float64(base) * settleFactor))
}

// parseDebugLevel reads the compile-time debugLevelStr and returns the
// verbosity threshold (0=verbose, 1=normal, 2=quiet). Falls back to normal.
func parseDebugLevel() int {
	if debugLevelStr != "" {
		if n, err := strconv.Atoi(debugLevelStr); err == nil && n >= debugVerbose && n <= debugQuiet {
			return n
		}
	}
	return debugNormal
}

// shouldEmitDebug returns true if the given verbosity level should produce output
// based on the global debugLevel setting.
func shouldEmitDebug(level int) bool {
	// Smaller number means more verbosity: 0 logs everything, 2 logs almost nothing.
	return debugLevel <= level
}

// dbgPrintf is a level-gated Printf; output is suppressed when level < debugLevel.
func dbgPrintf(level int, format string, args ...interface{}) {
	if !shouldEmitDebug(level) {
		return
	}
	log.Printf(format, args...)
}

// dbgPrintln is a level-gated Println; output is suppressed when level < debugLevel.
func dbgPrintln(level int, args ...interface{}) {
	if !shouldEmitDebug(level) {
		return
	}
	log.Println(args...)
}

// filteredHTTPLogWriter suppresses benign TLS/connection errors from the
// standard HTTP server error log to keep terminal output clean.
type filteredHTTPLogWriter struct{}

// Write filters out noisy HTTP server errors (TLS handshake failures,
// broken pipes, etc.) and forwards the rest to the debug logger.
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
	dbgPrintf(debugNormal, "[http] %s", msg)
	return len(p), nil
}

// ============================================================
// PASSWORD POLICY
// ============================================================

// loadPolicy reads password complexity rules from environment variables
// (PASSWORD_COMPLEXITY_ON, MIN_PASSWORD_LENGTH, REQUIRE_UPPER, etc.).
// Returns a PasswordPolicy with defaults when variables are unset.
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

// loadExpertPasswordHash reads the bcrypt hash for expert-mode authentication
// from the EXPERT_PASSWORD_HASH environment variable.
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

// recordStartupLockedDrives snapshots which OPAL drives are locked at PBA
// startup. This snapshot is used later to identify boot candidates — only
// drives that were locked at startup and have since been unlocked are eligible.
func recordStartupLockedDrives() {
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

func cloneBootKernelInfoSlice(src []BootKernelInfo) []BootKernelInfo {
	if len(src) == 0 {
		return nil
	}
	out := make([]BootKernelInfo, len(src))
	copy(out, src)
	return out
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// getBootLaunchStatus returns a deep-copy snapshot of the current boot launch
// state, safe for concurrent reads by HTTP handlers polling /boot-status.
func getBootLaunchStatus() BootLaunchStatus {
	bootStateMu.RLock()
	defer bootStateMu.RUnlock()
	status := bootLaunchState
	if status.Debug != nil {
		status.Debug = cloneStringSlice(status.Debug)
	}
	if status.Kernels != nil {
		status.Kernels = cloneBootKernelInfoSlice(status.Kernels)
	}
	if status.Result != nil {
		resultCopy := *status.Result
		if resultCopy.Debug != nil {
			resultCopy.Debug = cloneStringSlice(resultCopy.Debug)
		}
		if resultCopy.Drives != nil {
			resultCopy.Drives = append([]DriveStatus(nil), resultCopy.Drives...)
		}
		status.Result = &resultCopy
	}
	return status
}

// resetBootStateLocked zeroes the global boot launch state.
// Must be called with bootStateMu held.
func resetBootStateLocked() {
	bootLaunchState = BootLaunchStatus{}
}

// invalidateKernelDiscoveryCache clears cached discovery results after a drive
// unlock/rescan changes the set of bootable devices. The next /boot-list must
// rescan from scratch so the cache always reflects the current unlock state.
func invalidateKernelDiscoveryCache() {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootStateEpoch++
	bootCacheValid = false
	bootCacheError = ""
	bootCacheKernels = nil
	bootCacheDebug = nil
	bootCacheEpoch = 0
	if !bootLaunchState.InProgress {
		bootLaunchState.DiscoveryDone = false
		bootLaunchState.Kernels = nil
		if !bootLaunchState.Accepted {
			bootLaunchState.Debug = nil
			bootLaunchState.Error = ""
		}
	}
}

func cacheKernelDiscoveryLocked(epoch uint64, kernels []BootKernelInfo, debug []string, err error) {
	bootCacheValid = true
	bootCacheEpoch = epoch
	if err != nil {
		bootCacheError = err.Error()
	} else {
		bootCacheError = ""
	}
	bootCacheKernels = cloneBootKernelInfoSlice(kernels)
	bootCacheDebug = cloneStringSlice(debug)
}

func getCachedKernelDiscovery() ([]BootKernelInfo, []string, string, bool) {
	bootStateMu.RLock()
	defer bootStateMu.RUnlock()
	if !bootCacheValid || bootCacheEpoch != bootStateEpoch {
		return nil, nil, "", false
	}
	return cloneBootKernelInfoSlice(bootCacheKernels), cloneStringSlice(bootCacheDebug), bootCacheError, true
}

// acquireBootLock marks a boot or discovery operation as in-progress.
// Returns an error if one is already running (auto-resets after 2 minutes).
// Must be called before launching any async boot/discovery goroutine.
func acquireBootLock() error {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	if bootLaunchState.InProgress {
		// Auto-reset if the previous boot has been stuck for over 2 minutes
		if time.Since(bootLaunchState.StartedAt) > 2*time.Minute {
			dbgPrintf(debugNormal, "Auto-resetting stale boot-in-progress state (started %s ago)", time.Since(bootLaunchState.StartedAt).Round(time.Second))
			resetBootStateLocked()
		} else {
			return fmt.Errorf("boot is already in progress")
		}
	}
	resetBootStateLocked()
	bootLaunchState.InProgress = true
	bootLaunchState.StartedAt = time.Now()
	return nil
}

// completeBootLaunch records the outcome of a boot attempt into the global
// boot state. On success, stores a deep copy of the result; on failure,
// captures the error message and any debug lines for the UI to display.
func completeBootLaunch(result *BootResult, err error) {
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

// startKernelDiscovery kicks off an async scan of unlocked drives to enumerate
// all available kernels. Results are stored in bootLaunchState and retrieved
// via getCachedKernels. The UI polls /boot-status for progress.
func startKernelDiscovery() error {
	if cachedKernels, cachedDebug, cachedError, ok := getCachedKernelDiscovery(); ok {
		bootStateMu.Lock()
		resetBootStateLocked()
		bootLaunchState.Debug = cachedDebug
		bootLaunchState.DiscoveryDone = true
		bootLaunchState.Kernels = cachedKernels
		bootLaunchState.Error = cachedError
		bootStateMu.Unlock()
		return nil
	}
	if err := acquireBootLock(); err != nil {
		return err
	}
	bootStateMu.RLock()
	epoch := bootStateEpoch
	bootStateMu.RUnlock()
	go func() {
		kernels, debug, err := discoverBootKernels()
		completeKernelDiscovery(epoch, kernels, debug, err)
	}()
	return nil
}

// completeKernelDiscovery finalizes an async kernel discovery operation,
// storing the discovered kernels or the error into the global boot state.
func completeKernelDiscovery(epoch uint64, kernels []BootKernelInfo, debug []string, err error) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootLaunchState.InProgress = false
	bootLaunchState.DiscoveryDone = true
	bootLaunchState.Debug = cloneStringSlice(debug)
	if err != nil {
		bootLaunchState.Error = err.Error()
		if bootErr, ok := err.(BootAttemptError); ok {
			bootLaunchState.Debug = cloneStringSlice(bootErr.Debug)
		}
		if epoch == bootStateEpoch {
			cacheKernelDiscoveryLocked(epoch, nil, bootLaunchState.Debug, err)
		}
		return
	}
	bootLaunchState.Error = ""
	bootLaunchState.Kernels = cloneBootKernelInfoSlice(kernels)
	if epoch == bootStateEpoch {
		cacheKernelDiscoveryLocked(epoch, kernels, debug, nil)
	}
}

// getCachedKernels returns the kernel list from a previously completed
// discovery. Returns nil if discovery hasn't run or found nothing.
func getCachedKernels() []BootKernelInfo {
	kernels, _, cachedError, ok := getCachedKernelDiscovery()
	if !ok {
		return nil
	}
	if cachedError != "" || len(kernels) == 0 {
		return nil
	}
	return kernels
}

// startBootWithKernel begins an async boot using the kernel at kernelIndex.
// If a prior discovery cached kernels, it reuses them; otherwise it runs
// a full discovery-then-boot sequence.
func startBootWithKernel(kernelIndex int) error {
	// Use previously discovered kernels if available
	cached := getCachedKernels()
	if cached != nil {
		if err := acquireBootLock(); err != nil {
			return err
		}
		go func() {
			result, err := bootWithSelectedKernel(cached, kernelIndex)
			completeBootLaunch(result, err)
		}()
		return nil
	}
	// Fallback: no cached discovery, run full discovery + boot
	if err := acquireBootLock(); err != nil {
		return err
	}
	go func() {
		result, err := discoverAndBootKernel(kernelIndex)
		completeBootLaunch(result, err)
	}()
	return nil
}

// recordBootDebug appends a normal-level debug line to the live boot-status
// stream if debug output is enabled at the normal level.
func recordBootDebug(line string) {
	// Level 1: user-facing progress and actionable diagnostics.
	if !shouldEmitDebug(debugNormal) {
		return
	}
	recordBootDebugStamped(line)
}

// recordBootDebugVerbose appends a verbose-level debug line to the live
// boot-status stream. Only emitted when debugLevel is 0 (full trace).
func recordBootDebugVerbose(line string) {
	// Level 0: deep internals useful when tracing parser/device behavior.
	if !shouldEmitDebug(debugVerbose) {
		return
	}
	recordBootDebugStamped(line)
}

// recordBootDebugStamped writes a timestamped line into the bootLaunchState
// debug log. No-op if no boot/discovery operation is in progress.
func recordBootDebugStamped(line string) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	if !bootLaunchState.InProgress {
		return
	}
	stamped := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000"), line)
	bootLaunchState.Debug = append(bootLaunchState.Debug, stamped)
}

// getStartupLockedDevices returns a copy of the set of OPAL devices that
// were locked when the PBA started. Used to determine boot eligibility.
func getStartupLockedDevices() map[string]struct{} {
	bootStateMu.RLock()
	defer bootStateMu.RUnlock()
	out := make(map[string]struct{}, len(startupLockedOpal))
	for dev := range startupLockedOpal {
		out[dev] = struct{}{}
	}
	return out
}

// getBootCandidates returns the sorted list of OPAL drives that were locked
// at startup and are now unlocked — i.e., drives eligible for kernel boot.
func getBootCandidates(drives []DriveStatus) []string {
	startupLocked := getStartupLockedDevices()
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

// buildStatusResponse assembles the full system status (drives, network,
// unlock attempts, boot readiness) for the /status API endpoint.
func buildStatusResponse() StatusResponse {
	drives := scanDrives()
	failed, max, remaining := getUnlockAttemptCounts()
	return StatusResponse{
		Drives:            drives,
		Interfaces:        scanNetworkInterfaces(),
		BootReady:         len(getBootCandidates(drives)) > 0,
		BootDrives:        getBootCandidates(drives),
		FailedAttempts:    failed,
		MaxAttempts:       max,
		AttemptsRemaining: remaining,
		Build:             buildVersion,
		RepoURL:           repoURL,
	}
}

// getUnlockAttemptCounts returns the current failed attempt count, the
// configured maximum, and how many attempts remain before lockout.
func getUnlockAttemptCounts() (failed, max, remaining int) {
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

// mintSessionToken generates a new cryptographically random session token
// and stores it as the active API session token. Returns the hex-encoded token.
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

// mintExpertToken generates a new cryptographically random token for
// expert-mode operations and stores it as the active expert session token.
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

// validSessionToken returns true if the given token matches the current
// active API session token (issued after a successful unlock).
func validSessionToken(token string) bool {
	if token == "" {
		return false
	}
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return apiSessionToken != "" && token == apiSessionToken
}

// requireSessionToken is an HTTP middleware guard. Returns true (and writes
// a 403 response) if the request lacks a valid X-Auth-Token header.
func requireSessionToken(w http.ResponseWriter, r *http.Request) bool {
	if !validSessionToken(r.Header.Get("X-Auth-Token")) {
		jsonResponse(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return true
	}
	return false
}

// hasUnlockedBootDrive returns true if at least one OPAL drive that was
// locked at startup is now unlocked and eligible for booting.
func hasUnlockedBootDrive() bool {
	return len(getBootCandidates(scanDrives())) > 0
}

// isLoopbackRequest reports whether the HTTP client is connecting from the
// local machine. We allow the SSH forced-command helper to use the boot API
// without a web session token, but only over loopback so that unlocked-drive
// state cannot be used as a remote network-side auth bypass.
func isLoopbackRequest(r *http.Request) bool {
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireSessionTokenOrUnlockedDrive is an HTTP middleware guard. Returns
// true (and writes a 403) if the request has neither a valid session token
// nor a loopback-local unlocked-drive request. This preserves the SSH helper's
// local boot flow without exposing tokenless boot control over the network.
func requireSessionTokenOrUnlockedDrive(w http.ResponseWriter, r *http.Request) bool {
	if validSessionToken(r.Header.Get("X-Auth-Token")) {
		return false
	}
	if isLoopbackRequest(r) && hasUnlockedBootDrive() {
		return false
	}
	jsonResponse(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
	return true
}

// validExpertToken returns true if the given token matches the current
// active expert session token (issued after expert password authentication).
func validExpertToken(token string) bool {
	if token == "" {
		return false
	}
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return expertSessionTok != "" && token == expertSessionTok
}

// requireExpertToken is an HTTP middleware guard. Returns true (and writes
// a 403 response) if the request lacks a valid X-Expert-Token header.
func requireExpertToken(w http.ResponseWriter, r *http.Request) bool {
	if !validExpertToken(r.Header.Get("X-Expert-Token")) {
		jsonResponse(w, http.StatusForbidden, map[string]string{"error": "expert authentication required"})
		return true
	}
	return false
}

// isOpal2Drive checks whether a device path corresponds to a detected
// OPAL-capable drive by scanning the current drive list.
func isOpal2Drive(device string) bool {
	for _, d := range scanDrives() {
		if d.Device == device {
			return d.Opal
		}
	}
	return false
}

// hasSuccessfulUnlock returns true if at least one drive in the results
// was successfully unlocked.
func hasSuccessfulUnlock(results []UnlockResult) bool {
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

// printConsoleStatus renders a compact status dashboard for the physical
// console TUI, including build info, drive state, boot readiness, and
// network interface details.
func printConsoleStatus(status StatusResponse) {
	fmt.Printf("%sBuild:%s %s\n", colorDim, colorReset, buildVersionConsoleText(status))
	opalCount := 0
	lockedCount := 0
	if len(status.Drives) == 0 {
		fmt.Println("No OPAL drives detected.")
	} else {
		for _, d := range status.Drives {
			if d.Opal {
				opalCount++
			}
			lockState := "UNLOCKED"
			if d.Locked {
				lockState = "LOCKED"
				lockedCount++
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
	fmt.Printf("\nDrive summary: %d total, %d OPAL, %d locked\n", len(status.Drives), opalCount, lockedCount)
	if status.BootReady {
		fmt.Printf("Boot-ready drives: %s\n", strings.Join(status.BootDrives, ", "))
	} else {
		fmt.Println("Boot-ready drives: none yet")
	}
	fmt.Printf("\nUnlock attempts: %d/%d failed (%d remaining)\n", status.FailedAttempts, status.MaxAttempts, status.AttemptsRemaining)
	fmt.Println("\nNetwork Interfaces (use these names for NET_IFACES / EXCLUDE_NETDEV):")
	if len(status.Interfaces) == 0 {
		fmt.Println("  No network interfaces reported.")
		return
	}
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

func showConsoleDiagnostics() {
	clearConsoleScreen()
	status := buildStatusResponse()
	fmt.Println(colorBlue + "🩺 DIAGNOSTICS" + colorReset)
	printConsoleStatus(status)
	printConsoleDriveDiagnostics(collectDriveDiagnostics())
	waitForConsoleEnter()
}

func printConsoleDriveDiagnostics(diag []DriveDiagnostics) {
	fmt.Println("\nDrive Diagnostics:")
	if len(diag) == 0 {
		fmt.Println("  No OPAL diagnostics available.")
		return
	}
	for _, d := range diag {
		fmt.Printf("  %s  opal=%t locked=%t locking=%s enabled=%s mbrEnabled=%s mbrDone=%s mediaEncrypt=%s queryLocked=%s\n",
			d.Device, d.Opal, d.Locked, d.LockingSupported, d.LockingEnabled, d.MBREnabled, d.MBRDone, d.MediaEncrypt, d.LockingRange0Locked)
	}
}

func runConsoleBootMenu() {
	kernels, debug, err := discoverBootKernels()
	if err != nil {
		fmt.Printf("\n❌ %v\n", err)
		printBootDebugBlock(debug)
		waitForConsoleEnter()
		return
	}
	if len(kernels) == 0 {
		fmt.Println("\n❌ No bootable kernels were discovered.")
		printBootDebugBlock(debug)
		waitForConsoleEnter()
		return
	}

	showRecovery := false
	for {
		clearConsoleScreen()
		fmt.Println(colorBlue + "🧭 BOOT SELECTION" + colorReset)
		printConsoleStatus(buildStatusResponse())

		displayed := make([]int, 0, len(kernels))
		fmt.Println("\nDiscovered kernels:")
		for idx, kernel := range kernels {
			if !showRecovery && kernel.Recovery {
				continue
			}
			displayed = append(displayed, idx)
			label := kernel.KernelName
			if label == "" {
				label = filepath.Base(kernel.Kernel)
			}
			if kernel.Recovery {
				label = "[Recovery] " + label
			}
			fmt.Printf("  %d. %s  (%s on %s)\n", len(displayed), label, kernel.Source, kernel.Device)
		}
		if len(displayed) == 0 {
			fmt.Println("  No kernels visible with recovery hidden.")
		}
		toggleText := "Show"
		if showRecovery {
			toggleText = "Hide"
		}
		fmt.Printf("\n[%sH%s] %s%s recovery%s  [%sQ%s] %sStandby%s\n",
			colorDim, colorReset, colorBlue, toggleText, colorReset,
			colorDim, colorReset, colorDim, colorReset,
		)
		choice := strings.TrimSpace(strings.ToUpper(readConsoleLine("Kernel number [Enter=1]: ")))
		switch choice {
		case "Q":
			return
		case "H":
			showRecovery = !showRecovery
			continue
		}
		if len(displayed) == 0 {
			continue
		}
		selectedDisplay := 1
		if choice != "" {
			n, convErr := strconv.Atoi(choice)
			if convErr != nil || n < 1 || n > len(displayed) {
				fmt.Printf("\n❌ Choose a number between 1 and %d.\n", len(displayed))
				waitForConsoleEnter()
				continue
			}
			selectedDisplay = n
		}
		res, bootErr := bootWithSelectedKernel(kernels, displayed[selectedDisplay-1])
		if bootErr != nil {
			fmt.Printf("\n❌ %v\n", bootErr)
			if bootAttemptErr, ok := bootErr.(BootAttemptError); ok {
				printBootDebugBlock(bootAttemptErr.Debug)
			}
			waitForConsoleEnter()
			return
		}
		if res != nil && res.Warning != "" {
			fmt.Println("\n" + res.Warning)
		}
		return
	}
}

// ============================================================
// UNLOCK
// ============================================================

// unlockDrivesWithPassword iterates all locked OPAL drives and attempts to
// unlock each using the provided password via sedutil-cli. On success, it
// re-reads partition tables and resets the attempt counter. On all-failed,
// it increments the counter and powers off the machine at max attempts.
func unlockDrivesWithPassword(password string) ([]UnlockResult, error) {
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
		// Brief delay to let drive firmware settle between OPAL commands
		time.Sleep(settleDelay(baseOpalInterCmdDelay))
		err2 := exec.Command("sedutil-cli", "--setmbrdone", "on", password, d.Device).Run()
		success := err1 == nil && err2 == nil
		if success {
			successAny = true
			rescanBlockDeviceLayout(d.Device)
			// Let the kernel and udev fully process the partition table
			// change before anything tries to scan the new partitions.
			time.Sleep(settleDelay(basePartitionSettle))
		}
		results = append(results, UnlockResult{Device: d.Device, Success: success})
	}

	if successAny {
		invalidateKernelDiscoveryCache()
		mu.Lock()
		failedAttempts = 0
		mu.Unlock()
	} else {
		mu.Lock()
		failedAttempts++
		dbgPrintf(debugNormal, "Failed unlock attempt %d/%d", failedAttempts, maxAttempts)
		if failedAttempts >= maxAttempts {
			mu.Unlock()
			dbgPrintln(debugNormal, "Max failed attempts reached. Powering off.")
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

// eligiblePasswordChangeTargets returns the OPAL drives whose password can
// be changed. Prefers drives that were locked at startup (boot drives);
// falls back to any unlocked OPAL drive if none match.
func eligiblePasswordChangeTargets(drives []DriveStatus) []DriveStatus {
	startupLocked := getStartupLockedDevices()
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

// selectPasswordChangeTargets validates the user-selected device list against
// eligible targets and returns the matching DriveStatus entries.
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

// trimSedutilOutput cleans sedutil-cli output for inclusion in error messages:
// trims whitespace, collapses internal runs, and truncates at 220 chars.
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

// changePassword updates the Admin1 and SID passwords on the selected OPAL
// drives using sedutil-cli. Returns per-device results indicating which
// password components succeeded or failed.
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

// trimForDebug trims whitespace and truncates a string to max characters
// for inclusion in compact debug log lines.
func trimForDebug(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > max {
		return s[:max] + " ...[truncated]"
	}
	return s
}

// runCommandWithOutputTimeout executes a command with a timeout and returns
// its combined stdout/stderr output and any error.
func runCommandWithOutputTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	extra := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		if extra != "" {
			return extra, fmt.Errorf("%s timed out after %s (%s)", name, timeout, trimForDebug(extra, 1500))
		}
		return "", fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		if extra != "" {
			return extra, fmt.Errorf("%s failed: %v (%s)", name, err, trimForDebug(extra, 1500))
		}
		return "", fmt.Errorf("%s failed: %v", name, err)
	}
	return extra, nil
}

// runLVMStep executes a single LVM command (e.g. pvscan, vgscan) and logs
// verbose debug output. Failures are logged but do not halt the boot flow.
func runLVMStep(timeout time.Duration, name string, args ...string) {
	recordBootDebugVerbose(fmt.Sprintf("Running %s %s...", name, strings.Join(args, " ")))
	out, err := runCommandWithOutputTimeout(timeout, name, args...)
	if err != nil {
		recordBootDebugVerbose(fmt.Sprintf("%s %s failed: %v", name, strings.Join(args, " "), err))
		return
	}
	if trimmed := trimForDebug(out, 600); trimmed != "" {
		recordBootDebugVerbose(fmt.Sprintf("%s %s output: %s", name, strings.Join(args, " "), trimmed))
	}
}

// activateLVM runs the LVM scan/activate sequence (pvscan, vgscan, vgchange)
// to make logical volumes visible under /dev/mapper/ after unlocking drives.
func activateLVM() {
	if haveRuntimeCommand("pvscan") {
		runLVMStep(10*time.Second, "pvscan", "--cache")
	}
	if haveRuntimeCommand("vgscan") {
		runLVMStep(10*time.Second, "vgscan", "--mknodes")
	}
	if haveRuntimeCommand("vgchange") {
		// --noudevsync: do not wait for udev to create device nodes.
		// The PBA has no udev running, so without this flag vgchange
		// hangs indefinitely after activating LVs in metadata.
		runLVMStep(10*time.Second, "vgchange", "-ay", "--noudevsync")
	}
	// vgchange --noudevsync skips device node creation. Run vgscan
	// --mknodes again to create /dev/mapper/* nodes manually.
	if haveRuntimeCommand("vgscan") {
		runLVMStep(10*time.Second, "vgscan", "--mknodes")
	}
}

// listLogicalVolumes returns all device-mapper paths under /dev/mapper/
// except the "control" node. Used to find LVM volumes after activation.
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

// listAllBlockPartitions enumerates every partition visible in
// /sys/class/block/ (those with a "partition" sysfs file) and returns
// their /dev/ paths.
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

// listMDDevices returns /dev/mdN paths for any Linux software RAID arrays
// visible in /sys/class/block/.
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

// likelyLVMPhysicalVolume reads the first 4 KB of a block device and
// returns true if it contains LVM2 signature bytes ("LABELONE" + "LVM2").
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

// collectBootSearchDevices builds the ordered list of block devices to
// try mounting when searching for kernels. Includes partitions on boot
// drives, LVM logical volumes, MD RAID devices, and all remaining partitions.
func collectBootSearchDevices(bootDrives []string) ([]string, error) {
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

	// LVM activation is performed by callers before this function.
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

	// Minimum file size for kernels/initrds to filter out false positives
	// (e.g. terminfo entries, dpkg metadata). Real kernels are several MB;
	// even the smallest compressed kernel exceeds 500KB.
	const minBootFileSize int64 = 512 * 1024

	isKernelName := func(base string) bool {
		switch {
		case strings.HasPrefix(base, "vmlinuz-"),
			strings.HasPrefix(base, "linux-"),
			base == "vmlinuz",
			base == "linux",
			base == "bzImage":
			return true
		default:
			return false
		}
	}
	isInitrdName := func(base string) bool {
		switch {
		case strings.HasPrefix(base, "initrd.img-"),
			strings.HasPrefix(base, "initramfs-"),
			base == "initrd",
			base == "initramfs-linux.img":
			return true
		default:
			return false
		}
	}

	// Search only boot-relevant directories to avoid scanning the entire root
	// filesystem (which would match thousands of irrelevant files like
	// usr/share/terminfo/l/linux or var/lib/dpkg/info/linux-base.list).
	bootDirs := []string{
		filepath.Join(mountPoint, "boot"),
		filepath.Join(mountPoint, "efi"),
		filepath.Join(mountPoint, "EFI"),
		filepath.Join(mountPoint, "loader"),
		filepath.Join(mountPoint, "grub"),
		filepath.Join(mountPoint, "grub2"),
	}

	for _, dir := range bootDirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			parentDir := filepath.Dir(path)
			switch {
			case strings.EqualFold(base, "grub.cfg"):
				add(&grubConfigs, path)
			case filepath.Base(parentDir) == "entries" && filepath.Base(filepath.Dir(parentDir)) == "loader" && strings.HasSuffix(base, ".conf"):
				add(&loaderEntries, path)
			case isKernelName(base):
				if info, err := d.Info(); err == nil && info.Size() >= minBootFileSize {
					add(&kernels, path)
				}
			case isInitrdName(base):
				if info, err := d.Info(); err == nil && info.Size() >= minBootFileSize {
					add(&initrds, path)
				}
			}
			return nil
		})
	}

	sort.Strings(loaderEntries)
	sort.Strings(grubConfigs)
	sort.Strings(kernels)
	sort.Strings(initrds)
	return loaderEntries, grubConfigs, kernels, initrds
}

// trimMountPrefix strips the mount point from an absolute path to produce
// a relative path for display in debug logs.
func trimMountPrefix(mountPoint, path string) string {
	if rel, err := filepath.Rel(mountPoint, path); err == nil && rel != "." {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}

// resolveBootPath converts a boot config reference (e.g. "/boot/vmlinuz-6.8")
// into an absolute filesystem path under mountPoint. Returns "" if the file
// doesn't exist. Strips GRUB device prefixes like "(hd0,1)".
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

// splitKernelLine parses a GRUB "linux" directive into (path, cmdline, ok).
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

// splitInitrdLine parses a GRUB "initrd" directive into a list of initrd paths.
func splitInitrdLine(line string) []string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return nil
	}
	return fields[1:]
}

// kernelVersionSuffix extracts the version suffix from a kernel filename
// (e.g. "vmlinuz-6.8.12-9-pve" → "6.8.12-9-pve").
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

// isMemtestKernelBase returns true if the filename looks like a memtest binary,
// which should be excluded from boot kernel candidates.
func isMemtestKernelBase(base string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	return strings.Contains(base, "memtest")
}

// initrdVersionSuffix extracts the version suffix from an initrd filename
// (e.g. "initrd.img-6.8.12-9-pve" → "6.8.12-9-pve").
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

// makeBootEntry constructs a BootEntry with pre-parsed base names and
// version suffixes for efficient matching against discovered files.
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

// parseLoaderEntryCatalog parses systemd-boot style loader entry .conf files
// into BootEntry structs, extracting kernel, initrd, and options directives.
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

// trimGrubValue strips surrounding whitespace and quotes from a GRUB config value.
func trimGrubValue(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"'`)
	return raw
}

// hasGrubPrefix checks if a trimmed line starts with a GRUB directive keyword
// followed by a space or tab. GRUB2's grub-mkconfig uses tabs as separators.
func hasGrubPrefix(line, keyword string) bool {
	if !strings.HasPrefix(line, keyword) {
		return false
	}
	rest := line[len(keyword):]
	return len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t')
}

// expandGrubVars performs up to 4 rounds of GRUB variable substitution
// (both ${var} and $var forms) on a string.
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

// resolveGrubConfigRef resolves a GRUB "configfile" reference (with variable
// expansion) to an absolute path under mountPoint. Returns "" if not found.
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

// grubConfigChain follows the GRUB "configfile" chain starting from grubPath,
// resolving each target within mountPoint. Returns all reachable config files
// in traversal order (cycle-safe via visited set).
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
			case hasGrubPrefix(t, "set"):
				assignment := strings.TrimSpace(t[4:])
				key, value, ok := strings.Cut(assignment, "=")
				if !ok {
					continue
				}
				key = strings.TrimSpace(key)
				if key == "" {
					continue
				}
				vars[key] = trimGrubValue(expandGrubVars(value, vars))
			case hasGrubPrefix(t, "configfile"):
				ref := strings.TrimSpace(t[11:])
				if next := resolveGrubConfigRef(mountPoint, ref, vars); next != "" {
					walk(next)
				}
			}
		}
	}

	walk(grubPath)
	return out
}

// parseGrubConfigCatalog parses a grub.cfg (and any chained configs) into
// BootEntry structs representing all linux/initrd menuentry pairs.
func parseGrubConfigCatalog(grubPath, mountPoint string) []BootEntry {
	var entries []BootEntry
	for _, path := range grubConfigChain(grubPath, mountPoint) {
		entries = append(entries, parseSingleGrubConfigCatalog(path)...)
	}
	return entries
}

// parseSingleGrubConfigCatalog parses one grub.cfg file into BootEntry structs.
// Handles line continuations, GRUB variable expansion, and memtest filtering.
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
		if !hasGrubPrefix(t, "linux") && !hasGrubPrefix(t, "linuxefi") {
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
			if hasGrubPrefix(next, "linux") || hasGrubPrefix(next, "linuxefi") || hasGrubPrefix(next, "menuentry") {
				break
			}
			if hasGrubPrefix(next, "initrd") || hasGrubPrefix(next, "initrdefi") {
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

// collectBootCatalog gathers all BootEntry structs from loader entry files
// and GRUB configs found under the given mount point.
func collectBootCatalog(mountPoint string) []BootEntry {
	loaderEntries, grubConfigs, _, _ := collectBootFiles(mountPoint)
	entries := parseLoaderEntryCatalog(loaderEntries)
	for _, grubPath := range grubConfigs {
		entries = append(entries, parseGrubConfigCatalog(grubPath, mountPoint)...)
	}
	return entries
}

// matchBootEntryCmdline searches boot catalog entries for one whose kernel
// and initrd match the given files, and returns its cmdline. Tries exact
// basename match first, then kernel-only, then version-suffix matches.
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

// looksWeakCmdline returns true if the kernel command line lacks meaningful
// boot parameters (such as root=, resume=, or cryptdevice=). Weak cmdlines
// are kept as fallbacks while the search continues for a stronger candidate.
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

// looksLikeRecoveryCmdline returns true if the cmdline contains flags
// commonly used for GRUB recovery/safe-mode entries.
func looksLikeRecoveryCmdline(cmdline string) bool {
	for _, f := range strings.Fields(cmdline) {
		switch f {
		case "single", "recovery", "emergency", "rescue",
			"systemd.unit=rescue.target",
			"systemd.unit=emergency.target":
			return true
		}
	}
	return false
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

// summarizeBootEntry formats a BootEntry as a compact one-line string
// for debug log output.
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

// looksLikeRootFilesystem returns true if the mount point contains /etc,
// /usr, and /var directories, suggesting it is a root filesystem.
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

// readOSRelease reads /etc/os-release from mount and returns (ID, PRETTY_NAME).
func readOSRelease(mountPoint string) (string, string) {
	data, err := os.ReadFile(filepath.Join(mountPoint, "etc", "os-release"))
	if err != nil {
		return "", ""
	}
	var id, prettyName string
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch key {
		case "ID":
			id = value
		case "PRETTY_NAME":
			prettyName = value
		}
	}
	return id, prettyName
}

// synthesizeRootCmdline constructs a kernel cmdline by prepending root=device
// and ro to any existing flags. Used as a last resort when no boot config
// provides a real cmdline but the mount device is a root filesystem.
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

// findBootFromLoaderEntryFiles searches systemd-boot loader entry files
// (newest first) for a kernel+initrd pair that exists on the filesystem.
// Returns the resolved kernel path, initrd path, cmdline, and whether found.
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

// findBootFromGrubConfig walks the GRUB configfile chain starting at grubPath
// and returns the first kernel+initrd+cmdline found on the filesystem.
func findBootFromGrubConfig(grubPath, mountPoint string) (string, string, string, bool) {
	for _, path := range grubConfigChain(grubPath, mountPoint) {
		if kernel, initrd, cmdline, ok := findBootFromSingleGrubConfig(path, mountPoint); ok {
			return kernel, initrd, cmdline, true
		}
	}
	return "", "", "", false
}

// parseGrubLinesWithContinuation splits GRUB config content into logical
// lines, joining lines that end with a backslash continuation character.
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

// parseGrubVars reads "set" directives from a grub.cfg file and returns
// a map of variable names to expanded values (e.g. prefix, root).
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
		if hasGrubPrefix(t, "set") {
			assignment := strings.TrimSpace(t[4:])
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

// findBootFromSingleGrubConfig parses one grub.cfg for the first linux+initrd
// pair whose files exist on the filesystem, with GRUB variable expansion.
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
		if !hasGrubPrefix(t, "linux") && !hasGrubPrefix(t, "linuxefi") {
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
			recordBootDebugVerbose(fmt.Sprintf("grub-config %s: expanded cmdline %q -> %q", filepath.Base(grubPath), rawCmdline, cmdline))
		}

		kernelPath := resolveBootPath(mountPoint, kernelRef)
		if kernelPath == "" {
			recordBootDebugVerbose(fmt.Sprintf("grub-config %s: kernel ref %q did not resolve on filesystem", filepath.Base(grubPath), kernelRef))
			continue
		}

		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if hasGrubPrefix(next, "linux") || hasGrubPrefix(next, "linuxefi") || hasGrubPrefix(next, "menuentry") {
				break
			}
			if hasGrubPrefix(next, "initrd") || hasGrubPrefix(next, "initrdefi") {
				for _, initrdRef := range splitInitrdLine(next) {
					if initrdPath := resolveBootPath(mountPoint, initrdRef); initrdPath != "" {
						recordBootDebugVerbose(fmt.Sprintf("grub-config %s: returning kernel=%s initrd=%s cmdline=%q", filepath.Base(grubPath), filepath.Base(kernelPath), filepath.Base(initrdPath), cmdline))
						return kernelPath, initrdPath, cmdline, true
					}
				}
			}
		}
	}

	return "", "", "", false
}

// matchKernelInitrdPair finds the best (newest) kernel/initrd pair by matching
// version suffixes. Returns the first match scanning newest-to-oldest.
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

// matchAllKernelInitrdPairs matches all kernels with their initrds by version suffix.
// Returns pairs sorted newest-first (reverse lexicographic order).
func matchAllKernelInitrdPairs(kernels, initrds []string) [][2]string {
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

	sorted := append([]string(nil), kernels...)
	sort.Strings(sorted)
	pairs := make([][2]string, 0, len(sorted))
	for i := len(sorted) - 1; i >= 0; i-- {
		kernel := sorted[i]
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
			pairs = append(pairs, [2]string{kernel, initrd})
		}
	}
	return pairs
}

// findBootArtifacts is the primary boot artifact search for a single mount.
// Tries loader entries, then GRUB configs, then raw kernel/initrd filename
// matching. Augments weak cmdlines via the findBootCmdline fallback chain.
func findBootArtifacts(mountPoint, device string) (string, string, string, bool) {
	loaderEntries, grubConfigs, kernels, initrds := collectBootFiles(mountPoint)

	if kernel, initrd, cmdline, ok := findBootFromLoaderEntryFiles(mountPoint, loaderEntries); ok {
		recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: found via loader entries: kernel=%s cmdline=%q", filepath.Base(kernel), cmdline))
		// If cmdline looks weak, try to augment it via the full fallback chain
		if looksWeakCmdline(cmdline) {
			recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: loader entry cmdline is weak, trying findBootCmdline fallback"))
			if betterCmdline, err := findBootCmdline(mountPoint, kernel, device); err == nil {
				recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: findBootCmdline returned: %q", betterCmdline))
				cmdline = betterCmdline
			} else {
				recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: findBootCmdline failed: %v", err))
			}
		}
		return kernel, initrd, cmdline, true
	}
	for _, grubPath := range grubConfigs {
		if kernel, initrd, cmdline, ok := findBootFromGrubConfig(grubPath, mountPoint); ok {
			recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: found via grub config %s: kernel=%s cmdline=%q", trimMountPrefix(mountPoint, grubPath), filepath.Base(kernel), cmdline))
			// If cmdline looks weak, try to augment it via the full fallback chain
			if looksWeakCmdline(cmdline) {
				recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: grub config cmdline is weak, trying findBootCmdline fallback"))
				if betterCmdline, err := findBootCmdline(mountPoint, kernel, device); err == nil {
					recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: findBootCmdline returned: %q", betterCmdline))
					cmdline = betterCmdline
				} else {
					recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: findBootCmdline failed: %v", err))
				}
			}
			return kernel, initrd, cmdline, true
		}
	}

	if kernel, initrd, ok := matchKernelInitrdPair(kernels, initrds); ok {
		recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: matched kernel/initrd pair: kernel=%s initrd=%s, searching for cmdline...", filepath.Base(kernel), filepath.Base(initrd)))
		cmdline, err := findBootCmdline(mountPoint, kernel, device)
		if err == nil {
			return kernel, initrd, cmdline, true
		}
		recordBootDebugVerbose(fmt.Sprintf("findBootArtifacts: findBootCmdline failed for matched pair: %v", err))
		return kernel, initrd, "", true
	}
	recordBootDebugVerbose("findBootArtifacts: no boot artifacts found")
	return "", "", "", false
}

// discoverBootKernels scans all unlocked boot candidate drives, mounts each
// partition/LV/MD device, and collects every available kernel+initrd+cmdline
// combination. Returns the deduplicated list, debug log lines, and any error.
func discoverBootKernels() ([]BootKernelInfo, []string, error) {
	debug := make([]string, 0, 64)
	appendDebug := func(format string, args ...interface{}) {
		// Discovery progress is considered level-1 logging and is hidden in quiet mode.
		if !shouldEmitDebug(debugNormal) {
			return
		}
		line := fmt.Sprintf(format, args...)
		debug = append(debug, line)
		recordBootDebug(line)
	}
	appendDebugVerbose := func(format string, args ...interface{}) {
		if !shouldEmitDebug(debugVerbose) {
			return
		}
		line := fmt.Sprintf(format, args...)
		debug = append(debug, line)
		recordBootDebugVerbose(line)
	}

	// Brief pause to let drive firmware and udev settle after a recent
	// unlock. Without this, sedutil-cli --scan / --query and LVM commands
	// can hit the drive before it has fully transitioned out of the locked
	// state, causing hangs identical to the flash-after-query race.
	delay := settleDelay(baseDiscoverySettle)
	appendDebug("Waiting for drive firmware to settle (%s at factor %.2f)...", delay, settleFactor)
	time.Sleep(delay)

	drives := scanDrives()
	bootCandidates := getBootCandidates(drives)
	appendDebug("Boot candidates: %s", strings.Join(bootCandidates, ", "))

	if len(bootCandidates) == 0 {
		return nil, debug, fmt.Errorf("no startup-locked OPAL drive has transitioned to unlocked")
	}

	mountPoint := "/mnt/proxmox"
	_ = os.MkdirAll(mountPoint, 0755)

	// Activate LVM in case kernels are on LVM volumes
	appendDebug("Activating LVM...")
	activateLVM()
	appendDebug("LVM activation complete.")

	searchDevices, err := collectBootSearchDevices(bootCandidates)
	if err != nil {
		appendDebug("collectBootSearchDevices failed: %v", err)
		return nil, debug, err
	}
	appendDebugVerbose("Search devices: %s", strings.Join(searchDevices, ", "))

	kernels := make([]BootKernelInfo, 0, 8)

	for _, dev := range searchDevices {
		appendDebugVerbose("Trying mount target: %s", dev)
		if err := runCommandTimeout(4*time.Second, "mount", "-r", dev, mountPoint); err != nil {
			appendDebugVerbose("Mount failed for %s: %v", dev, err)
			continue
		}

		unmount := func() {
			if err := runCommandTimeout(3*time.Second, "umount", mountPoint); err != nil {
				appendDebugVerbose("Unmount failed for %s: %v", dev, err)
			}
		}
		appendDebugVerbose("Mounted %s on %s", dev, mountPoint)

		// Log detected OS if this is a root filesystem
		if osID, osName := readOSRelease(mountPoint); osID != "" {
			appendDebugVerbose("Detected OS on %s: %s (%s)", dev, osName, osID)
		}

		// Log what collectBootFiles found on this mount
		loaderEntries, grubConfigs, rawKernels, rawInitrds := collectBootFiles(mountPoint)
		appendDebugVerbose("collectBootFiles on %s: loaders=%d grubs=%d kernels=%d initrds=%d", dev, len(loaderEntries), len(grubConfigs), len(rawKernels), len(rawInitrds))
		for _, g := range grubConfigs {
			appendDebugVerbose("  grub.cfg found: %s", trimMountPrefix(mountPoint, g))
		}
		for _, k := range rawKernels {
			appendDebugVerbose("  kernel found: %s", trimMountPrefix(mountPoint, k))
		}
		for _, i := range rawInitrds {
			appendDebugVerbose("  initrd found: %s", trimMountPrefix(mountPoint, i))
		}

		// Collect all boot entries from this mount
		entries := collectBootCatalog(mountPoint)
		appendDebugVerbose("Boot catalog entries on %s: %d", dev, len(entries))
		for i, entry := range entries {
			appendDebugVerbose("  catalog[%d]: kernel=%s cmdline=%q source=%s", i, entry.KernelBase, entry.Cmdline, trimMountPrefix(mountPoint, entry.Source))
		}

		// Also collect raw kernels/initrds for matching
		// Match ALL kernel/initrd pairs, not just the first one
		allPairs := matchAllKernelInitrdPairs(rawKernels, rawInitrds)
		appendDebugVerbose("Matched %d kernel/initrd pairs on %s", len(allPairs), dev)

		for _, pair := range allPairs {
			rawKernel, rawInitrd := pair[0], pair[1]
			// Try to find the best cmdline for this kernel
			cmdline, err := findBootCmdline(mountPoint, rawKernel, dev)
			if err != nil {
				cmdline = ""
			}
			// Enhance weak cmdlines with synthesized root=
			if (cmdline == "" || looksWeakCmdline(cmdline)) && looksLikeRootFilesystem(mountPoint) {
				if synthesized, ok := synthesizeRootCmdline(dev, cmdline); ok {
					appendDebugVerbose("  Synthesized cmdline for %s: %q", filepath.Base(rawKernel), synthesized)
					cmdline = synthesized
				}
			}
			kernels = append(kernels, BootKernelInfo{
				Index:      len(kernels),
				Device:     dev,
				Kernel:     rawKernel,
				KernelName: filepath.Base(rawKernel),
				Initrd:     rawInitrd,
				InitrdName: filepath.Base(rawInitrd),
				Cmdline:    cmdline,
				Source:     "discovered",
			})
			appendDebugVerbose("  Kernel: %s | %s | cmdline=%q", filepath.Base(rawKernel), filepath.Base(rawInitrd), cmdline)
		}

		// If no pairs matched but findBootArtifacts can find something (e.g. from loader entries/grub)
		if len(allPairs) == 0 {
			if rawKernel, rawInitrd, cmdline, ok := findBootArtifacts(mountPoint, dev); ok {
				if (cmdline == "" || looksWeakCmdline(cmdline)) && looksLikeRootFilesystem(mountPoint) {
					if synthesized, ok := synthesizeRootCmdline(dev, cmdline); ok {
						cmdline = synthesized
					}
				}
				kernels = append(kernels, BootKernelInfo{
					Index:      len(kernels),
					Device:     dev,
					Kernel:     rawKernel,
					KernelName: filepath.Base(rawKernel),
					Initrd:     rawInitrd,
					InitrdName: filepath.Base(rawInitrd),
					Cmdline:    cmdline,
					Source:     "discovered",
				})
				appendDebugVerbose("  Artifact: %s | %s | cmdline=%q", filepath.Base(rawKernel), filepath.Base(rawInitrd), cmdline)
			} else {
				appendDebugVerbose("No kernel/initrd pairs found on %s", dev)
			}
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

		appendDebugVerbose("Kernel candidates accumulated so far: %d", len(kernels))
		unmount()
	}

	if len(kernels) == 0 {
		appendDebug("Boot search exhausted with zero kernels")
		return nil, debug, fmt.Errorf("no kernels found on boot devices")
	}

	// Deduplicate kernels by (kernelName, initrdName, cmdline).
	// First occurrence wins — discovered entries appear before catalog.
	seen := make(map[string]bool)
	deduped := kernels[:0]
	for _, k := range kernels {
		key := k.KernelName + "\x00" + k.InitrdName + "\x00" + k.Cmdline
		if seen[key] {
			continue
		}
		seen[key] = true
		k.Index = len(deduped)
		k.Recovery = looksLikeRecoveryCmdline(k.Cmdline)
		deduped = append(deduped, k)
	}
	kernels = deduped

	appendDebug("Boot search finished with %d kernel candidates", len(kernels))
	return kernels, debug, nil
}

// discoverAndBootKernel boots with a specific kernel selected by index.
// If kernelIndex < 0, uses the first available kernel.
func discoverAndBootKernel(kernelIndex int) (*BootResult, error) {
	kernels, _, err := discoverBootKernels()
	if err != nil {
		return nil, BootAttemptError{
			Message: err.Error(),
			Debug:   []string{err.Error()},
		}
	}
	return bootWithSelectedKernel(kernels, kernelIndex)
}

// bootWithSelectedKernel boots a kernel from an already-discovered list.
func bootWithSelectedKernel(kernels []BootKernelInfo, kernelIndex int) (*BootResult, error) {
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

	// Get boot drives setup
	drives := scanDrives()
	bootCandidates := getBootCandidates(drives)
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
		appendBootDebugVerbose(&debug, "Failed to mount selected device %s: %v", selected.Device, err)
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

	appendBootDebug(&debug, "kexec -l succeeded. Preparing for OS handoff...")
	result := &BootResult{
		Kernel:        selected.Kernel,
		Initrd:        selected.Initrd,
		Cmdline:       selected.Cmdline,
		Drives:        drives,
		Warning:       warning,
		FullyUnlocked: fullyUnlocked,
		Debug:         debug,
	}

	// Give the web UI time to poll and display the final boot log
	// before the HTTP server shuts down for kexec.
	time.Sleep(3 * time.Second)

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

// parseQuotedShellValue strips shell quoting (double or single quotes)
// from a value read from a GRUB defaults file.
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

// parseDefaultGrubCmdline reads /etc/default/grub from the mounted filesystem
// and extracts the GRUB_CMDLINE_LINUX and GRUB_CMDLINE_LINUX_DEFAULT values.
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

// parseKernelCmdlineFile reads /etc/kernel/cmdline from the mounted filesystem
// as a fallback source for the target kernel command line.
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
			recordBootDebugVerbose(fmt.Sprintf("findBootCmdline[%s]: no result (err=%v)", c.name, err))
			continue
		}
		recordBootDebugVerbose(fmt.Sprintf("findBootCmdline[%s]: found %q", c.name, cmdline))
		// Skip cmdlines that point to a different device (e.g., sda when we're on nvme0)
		if !isValidCmdlineForDevice(cmdline, device) {
			recordBootDebugVerbose(fmt.Sprintf("findBootCmdline[%s]: rejected by isValidCmdlineForDevice (device=%s)", c.name, device))
			continue
		}
		// If this is strong (has meaningful content), return immediately
		if !looksWeakCmdline(cmdline) {
			recordBootDebugVerbose(fmt.Sprintf("findBootCmdline[%s]: accepted (strong)", c.name))
			return cmdline, nil
		}
		recordBootDebugVerbose(fmt.Sprintf("findBootCmdline[%s]: weak, saving as fallback", c.name))
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

// parseSingleGrubCfg extracts the kernel command line from a single grub.cfg
// file by scanning for linux/linuxefi directives that match the target kernel.
func parseSingleGrubCfg(grubPath, kernelBase string) (string, bool, error) {
	data, err := os.ReadFile(grubPath)
	if err != nil {
		return "", false, err
	}
	vars := parseGrubVars(grubPath)
	lines := parseGrubLinesWithContinuation(string(data))

	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !hasGrubPrefix(t, "linux") && !hasGrubPrefix(t, "linuxefi") {
			continue
		}
		if kernelBase != "" && !strings.Contains(t, kernelBase) {
			continue
		}
		if cmdline, ok := extractLinuxCmdline(t); ok {
			rawCmdline := cmdline
			cmdline = expandGrubVars(cmdline, vars)
			recordBootDebugVerbose(fmt.Sprintf("parseSingleGrubCfg(%s): linux line matched, raw=%q expanded=%q", filepath.Base(grubPath), rawCmdline, cmdline))
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

func clearConsoleScreen() {
	fmt.Print("\033[H\033[2J")
}

func readConsoleLine(prompt string) string {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func readConsoleMenuKey(fd int, timeout time.Duration) (string, bool, error) {
	timeoutMs := int(timeout / time.Millisecond)
	if timeout > 0 && timeoutMs == 0 {
		timeoutMs = 1
	}
	pollFDs := []unix.PollFd{{
		Fd:     int32(fd),
		Events: unix.POLLIN,
	}}
	for {
		n, err := unix.Poll(pollFDs, timeoutMs)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if n == 0 || pollFDs[0].Revents&unix.POLLIN == 0 {
			return "", false, nil
		}
		buf := make([]byte, 1)
		_, err = unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", false, err
		}
		return strings.ToUpper(string(buf[0])), true, nil
	}
}

func waitForConsoleEnter() {
	_ = readConsoleLine("\nPress Enter to continue...")
}

func buildVersionConsoleText(status StatusResponse) string {
	if status.Build == "" {
		return "unknown"
	}
	if status.RepoURL == "" {
		return status.Build
	}
	return fmt.Sprintf("%s (%s)", status.Build, status.RepoURL)
}

func printBootDebugBlock(debug []string) {
	if len(debug) == 0 {
		return
	}
	fmt.Println("\nBoot debug:")
	for _, line := range debug {
		fmt.Printf("  %s\n", line)
	}
}

// runConsoleUI is the main loop for the physical console TUI. It displays
// the standby screen, waits for a keypress, then enters the active menu.
// Runs in its own goroutine for the duration of the PBA lifecycle.
func runConsoleUI() {
	fd := int(os.Stdin.Fd())
	for {
		clearConsoleScreen()
		fmt.Println(colorBlue + "🔒 PBA STANDBY" + colorReset)
		printConsoleStatus(buildStatusResponse())
		fmt.Println("\n" + colorDim + "Press any key..." + colorReset)

		old, err := term.MakeRaw(fd)
		if err != nil {
			dbgPrintf(debugNormal, "term.MakeRaw failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		term.Restore(fd, old)
		runActiveConsoleMenu(fd)
	}
}

// runActiveConsoleMenu presents an interactive menu on the physical console
// for unlock, boot, diagnostics, reboot, and shutdown operations. Destructive
// expert actions and password changes are intentionally web-only. Times out
// back to the standby screen after privacyTimeout.
func runActiveConsoleMenu(fd int) {
	for {
		clearConsoleScreen()
		fmt.Println(colorBlue + "🔑 ACTIVE MODE" + colorReset)
		printConsoleStatus(buildStatusResponse())

		fmt.Printf(
			"\n[%sENTER%s] %sUnlock%s  [%sB%s] %sBoot%s  [%sN%s] %sNetwork%s  [%sD%s] %sDiagnostics%s  [%sR%s] %sReboot%s  [%sS%s] %sShutdown%s  [%sQ%s] %sStandby%s\n",
			colorDim, colorReset, colorPurple, colorReset,
			colorDim, colorReset, colorBlue, colorReset,
			colorDim, colorReset, colorBlue, colorReset,
			colorDim, colorReset, colorBlue, colorReset,
			colorDim, colorReset, colorOrange, colorReset,
			colorDim, colorReset, colorBlue, colorReset,
			colorDim, colorReset, colorDim, colorReset,
		)

		old, err := term.MakeRaw(fd)
		if err != nil {
			dbgPrintf(debugNormal, "term.MakeRaw failed: %v", err)
			return
		}

		key, ok, err := readConsoleMenuKey(fd, privacyTimeout)
		term.Restore(fd, old)
		if err != nil {
			dbgPrintf(debugNormal, "console input failed: %v", err)
			return
		}
		if !ok {
			return
		}

		switch key {
		case "\r":
			fmt.Print("Password: ")
			pw, _ := term.ReadPassword(fd)
			results, err := unlockDrivesWithPassword(string(pw))
			fmt.Println()
			if len(results) == 0 && err == nil {
				fmt.Println("No locked OPAL drives were reported.")
			}
			for _, result := range results {
				if result.Success {
					fmt.Printf("✅ %s unlocked\n", result.Device)
				} else {
					fmt.Printf("❌ %s unlock failed\n", result.Device)
				}
			}
			if err != nil {
				fmt.Println("❌", err)
			}
			waitForConsoleEnter()

		case "B":
			runConsoleBootMenu()

		case "N":
			runConsoleNetworkMenu()

		case "D":
			showConsoleDiagnostics()

		case "R":
			exec.Command("reboot", "-nf").Run()

		case "S":
			exec.Command("poweroff", "-nf").Run()

		case "Q":
			return
		}
	}
}

func readImageRange(f *os.File, offset int64, size int) ([]byte, error) {
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n != size {
		return nil, io.ErrUnexpectedEOF
	}
	return buf, nil
}

// validateUploadedPBAImageFile checks that a user-uploaded PBA image stored
// in a temporary file has a valid MBR signature, a bootable EFI partition
// (type 0xEF), and a FAT32 filesystem. Only the required sectors are read so
// the upload path does not keep a duplicate full-image copy in memory.
func validateUploadedPBAImageFile(imagePath, filename string) ([]string, error) {
	validation := make([]string, 0, 12)
	info, err := os.Stat(imagePath)
	if err != nil {
		return validation, fmt.Errorf("failed to stat uploaded image: %w", err)
	}
	if info.Size() <= 0 {
		return validation, fmt.Errorf("uploaded image is empty")
	}
	validation = append(validation, fmt.Sprintf("file size: %d bytes", info.Size()))
	if info.Size() > 128<<20 {
		return validation, fmt.Errorf("uploaded image exceeds 128 MiB")
	}
	validation = append(validation, "size is within the 128 MiB OPAL2 guideline")
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".img" && ext != ".bin" {
		return validation, fmt.Errorf("uploaded image must end in .img or .bin")
	}
	validation = append(validation, "filename extension is acceptable")

	if info.Size() < 512 {
		return validation, fmt.Errorf("uploaded image is too small to contain a valid MBR")
	}

	f, err := os.Open(imagePath)
	if err != nil {
		return validation, fmt.Errorf("failed to open uploaded image: %w", err)
	}
	defer f.Close()

	mbr, err := readImageRange(f, 0, 512)
	if err != nil {
		return validation, fmt.Errorf("failed to read uploaded image MBR: %w", err)
	}
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
	if bootSectorEnd > info.Size() {
		return validation, fmt.Errorf("uploaded image boot partition is unreadable")
	}

	bootSector, err := readImageRange(f, bootSectorOffset, 512)
	if err != nil {
		return validation, fmt.Errorf("failed to read uploaded image boot sector: %w", err)
	}
	if bootSector[510] != 0x55 || bootSector[511] != 0xaa {
		return validation, fmt.Errorf("uploaded image first partition is missing a valid boot sector signature")
	}
	validation = append(validation, "boot partition has a valid boot sector signature")
	if bootSector[0] != 0xeb && bootSector[0] != 0xe9 {
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

// main initialises the PBA server: records startup-locked drives, launches the
// console TUI and SSH service, then starts the HTTPS API server on port 443.
func main() {
	dbgPrintf(debugNormal, "Settle factor: %.2f (inter-cmd=%s, partition=%s, discovery=%s)",
		settleFactor,
		settleDelay(baseOpalInterCmdDelay),
		settleDelay(basePartitionSettle),
		settleDelay(baseDiscoverySettle))
	dbgPrintf(debugNormal, "Debug level: %d (0=verbose, 1=normal, 2=quiet)", debugLevel)

	recordStartupLockedDrives()
	go runConsoleUI()
	startSSHService()

	httpErrorLog := log.New(filteredHTTPLogWriter{}, "", 0)

	go func() {
		redirectSrv := &http.Server{
			Addr: ":80",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Preserve the user-visible host for HTTP->HTTPS redirects so
				// custom certificates tied to a real DNS name/IP are not replaced
				// with "localhost" on the client side.
				host := r.Host
				if h, _, err := net.SplitHostPort(r.Host); err == nil {
					host = h
				}
				http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
			}),
			ErrorLog: httpErrorLog,
		}
		if err := redirectSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			dbgPrintf(debugNormal, "[http] redirect server failed: %v", err)
		}
	}()

	mux := http.NewServeMux()

	mux.Handle("/", http.FileServer(http.Dir("static")))

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, buildStatusResponse())
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
		if checkOrigin(w, r) {
			return
		}
		limitBody(r)
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		results, err := unlockDrivesWithPassword(req.Password)
		if err != nil {
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		token := ""
		if hasSuccessfulUnlock(results) {
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
		if checkOrigin(w, r) {
			return
		}
		if requireSessionToken(w, r) {
			return
		}
		limitBody(r)
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
		if checkOrigin(w, r) {
			return
		}
		if expertPasswordHash == "" {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "expert auth is not configured"})
			return
		}
		expertMu.Lock()
		if expertFailedAttempts >= maxAttempts {
			expertMu.Unlock()
			dbgPrintln(debugNormal, "Max expert auth attempts reached. Powering off.")
			go func() {
				time.Sleep(500 * time.Millisecond)
				exec.Command("poweroff", "-nf").Run()
			}()
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": "maximum failed attempts reached"})
			return
		}
		expertMu.Unlock()
		limitBody(r)
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(expertPasswordHash), []byte(req.Password)); err != nil {
			expertMu.Lock()
			expertFailedAttempts++
			dbgPrintf(debugNormal, "Failed expert auth attempt %d/%d", expertFailedAttempts, maxAttempts)
			if expertFailedAttempts >= maxAttempts {
				expertMu.Unlock()
				dbgPrintln(debugNormal, "Max expert auth attempts reached. Powering off.")
				go func() {
					time.Sleep(500 * time.Millisecond)
					exec.Command("poweroff", "-nf").Run()
				}()
				jsonResponse(w, http.StatusForbidden, map[string]string{"error": "maximum failed attempts reached"})
				return
			}
			expertMu.Unlock()
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": "invalid expert password"})
			return
		}
		expertMu.Lock()
		expertFailedAttempts = 0
		expertMu.Unlock()
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
		if checkOrigin(w, r) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}
		limitBody(r)
		var req struct {
			Device   string `json:"device"`
			Password string `json:"password"`
			Confirm  string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if !validateDevicePath(req.Device) {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a valid /dev path"})
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
		if checkOrigin(w, r) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}
		limitBody(r)
		var req struct {
			Device  string `json:"device"`
			PSID    string `json:"psid"`
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if !validateDevicePath(req.Device) {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a valid /dev path"})
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
		if checkOrigin(w, r) {
			return
		}
		if requireExpertToken(w, r) {
			return
		}

		const maxUploadBytes = 128 << 20
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1024)
		mr, err := r.MultipartReader()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid upload; multipart form data is required"})
			return
		}

		device := ""
		password := ""
		confirm := ""
		imagePath := ""
		imageFilename := ""

		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				if imagePath != "" {
					_ = os.Remove(imagePath)
				}
				jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "failed to read upload data"})
				return
			}

			switch part.FormName() {
			case "device":
				if data, err := io.ReadAll(io.LimitReader(part, 4096)); err == nil {
					device = strings.TrimSpace(string(data))
				}
			case "password":
				if data, err := io.ReadAll(io.LimitReader(part, 4096)); err == nil {
					password = string(data)
				}
			case "confirm":
				if data, err := io.ReadAll(io.LimitReader(part, 4096)); err == nil {
					confirm = strings.TrimSpace(string(data))
				}
			case "image":
				if imagePath != "" {
					part.Close()
					_ = os.Remove(imagePath)
					jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "only one image upload is allowed"})
					return
				}
				imageFilename = part.FileName()
				tmp, err := os.CreateTemp("", "sedunlocksrv-pba-upload-*.img")
				if err != nil {
					part.Close()
					jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to prepare temporary upload file"})
					return
				}
				imagePath = tmp.Name()
				limited := &io.LimitedReader{R: part, N: maxUploadBytes + 1}
				written, copyErr := io.Copy(tmp, limited)
				closeErr := tmp.Close()
				part.Close()
				if copyErr != nil {
					_ = os.Remove(imagePath)
					jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "failed to read uploaded image"})
					return
				}
				if closeErr != nil {
					_ = os.Remove(imagePath)
					jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to finalize uploaded image"})
					return
				}
				if limited.N == 0 {
					_ = os.Remove(imagePath)
					jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "uploaded image exceeds 128 MiB"})
					return
				}
				if written == 0 {
					_ = os.Remove(imagePath)
					jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "uploaded image is empty"})
					return
				}
			default:
				_, _ = io.Copy(io.Discard, part)
			}
			part.Close()
		}

		if !validateDevicePath(device) {
			_ = os.Remove(imagePath)
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a valid /dev path"})
			return
		}
		if !isOpal2Drive(device) {
			_ = os.Remove(imagePath)
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "device must be a detected OPAL2 drive"})
			return
		}
		if strings.TrimSpace(password) == "" {
			_ = os.Remove(imagePath)
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "current drive password is required"})
			return
		}
		if confirm != "FLASH" {
			_ = os.Remove(imagePath)
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "confirmation text must be FLASH"})
			return
		}
		if imagePath == "" || imageFilename == "" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "pba image file is required"})
			return
		}

		validation, err := validateUploadedPBAImageFile(imagePath, imageFilename)
		if err != nil {
			_ = os.Remove(imagePath)
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

		runExpertPBAFlashImagePath(w, password, imagePath, device, validation)
	})

	mux.HandleFunc("/expert/flash-status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		if requireExpertToken(w, r) {
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
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if checkOrigin(w, r) {
			return
		}
		if requireSessionTokenOrUnlockedDrive(w, r) {
			return
		}
		if err := startKernelDiscovery(); err != nil {
			jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "discovery started"})
	})

	mux.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if checkOrigin(w, r) {
			return
		}
		if requireSessionTokenOrUnlockedDrive(w, r) {
			return
		}

		limitBody(r)
		var req struct {
			KernelIndex int `json:"kernelIndex"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.KernelIndex < 0 {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "kernelIndex is required"})
			return
		}

		if err := startBootWithKernel(req.KernelIndex); err != nil {
			jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "boot requested"})
	})

	mux.HandleFunc("/boot-status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, getBootLaunchStatus())
	})

	mux.HandleFunc("/reboot", newSystemActionHandler("rebooting", "reboot", "-nf"))
	mux.HandleFunc("/poweroff", newSystemActionHandler("powering off", "poweroff", "-nf"))

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
	// Wait for a boot function to signal that kexec -l succeeded and it is
	// ready for us to fire kexec -e. Web and console boot flows both close this
	// channel after loading the selected kernel; idle sessions simply block here
	// until an operator requests a boot.
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
		// kexec -e returned — it failed. Send the error back to the boot
		// function so it can populate bootLaunchState with a useful message.
		kexecFailed <- err
		// Restart the HTTPS server so the web UI can reconnect and display
		// the error via /boot-status rather than getting a dead connection.
		dbgPrintf(debugNormal, "kexec -e failed: %v — restarting HTTPS server", err)
		if err := httpsSrv.ListenAndServeTLS("server.crt", "server.key"); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTPS server restart failed: %v", err)
		}
	}
	// kexec -e succeeded — the kernel has been replaced. Never reached.
	select {}
}
