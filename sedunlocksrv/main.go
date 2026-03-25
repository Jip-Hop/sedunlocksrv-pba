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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Drives     []DriveStatus            `json:"drives"`
	Interfaces []NetworkInterfaceStatus `json:"interfaces"`
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

var (
	// mu protects failedAttempts. The check, work, and increment are all
	// done under a single lock hold to prevent TOCTOU races between
	// concurrent unlock requests (e.g. web UI + SSH open simultaneously).
	failedAttempts int
	maxAttempts    = 5
	mu             sync.Mutex
	unlockMu       sync.Mutex

	sessionMu        sync.RWMutex
	apiSessionToken  string
	expertSessionTok string

	passwordPolicy    = loadPolicy()
	expertPasswordHash = loadExpertPasswordHash()
)

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

// scanDrives calls sedutil-cli to enumerate all drives and their lock state.
// Each call uses a 5-second context timeout so a stalled drive or missing
// binary never blocks the HTTP handler indefinitely. (Fix #1)
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
		query, _ := queryDrive(dev)
		locked := strings.Contains(query, "Locked = Y")
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

// currentStatus returns a snapshot of drives and network interfaces.
// Callers that need both should call this once and pass the result down
// rather than calling scanDrives/scanNetworkInterfaces separately. (Size #1)
func currentStatus() StatusResponse {
	return StatusResponse{
		Drives:     scanDrives(),
		Interfaces: scanNetworkInterfaces(),
	}
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
	for _, d := range status.Drives {
		if d.Locked {
			fmt.Println("❌", d.Device)
		} else {
			fmt.Println("✅", d.Device)
		}
	}
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

// attemptUnlock tries to unlock all locked drives with the given password.
//
// Rate limiting: the entire check-and-increment sequence is held under mu so
// two concurrent requests cannot both pass the limit check simultaneously.
// (Fix #2 — closes the TOCTOU race between check and increment.)
func attemptUnlock(password string) ([]UnlockResult, error) {
	if strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("password cannot be blank")
	}

	// Serialize unlock workflows without holding the failed-attempt mutex while
	// running external commands. This prevents long lock holds that block other
	// request paths, while preserving deterministic attempt accounting.
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

// changePassword updates SID and Admin1 passwords on all detected drives.
//
// Both commands are attempted and each failure is reported independently.
// If only one of the two sedutil-cli calls fails the drive is left in a
// split state; the error message now identifies which command failed so
// the operator can take corrective action. (Fix #5)
func changePassword(current, newPw string) []PasswordChangeResult {
	var results []PasswordChangeResult
	for _, d := range scanDrives() {
		err1 := exec.Command("sedutil-cli", "--setsidpassword", current, newPw, d.Device).Run()
		err2 := exec.Command("sedutil-cli", "--setadmin1password", current, newPw, d.Device).Run()

		var errMsg string
		switch {
		case err1 != nil && err2 != nil:
			errMsg = "failed to update SID and Admin1 passwords"
		case err1 != nil:
			errMsg = "SID password update failed (drive may be in split state — Admin1 was updated)"
		case err2 != nil:
			errMsg = "Admin1 password update failed (drive may be in split state — SID was updated)"
		}

		results = append(results, PasswordChangeResult{
			Device:  d.Device,
			Success: err1 == nil && err2 == nil,
			Error:   errMsg,
		})
	}
	return results
}

// ============================================================
// BOOT
// ============================================================

// BootSystem mounts the first unlocked drive's bootable partition, loads the
// Proxmox kernel and initrd via kexec, then executes kexec to transfer control.
func BootSystem() (*BootResult, error) {
	drives := scanDrives()

	var unlocked, locked []string
	for _, d := range drives {
		if d.Locked {
			locked = append(locked, d.Device)
		} else {
			unlocked = append(unlocked, d.Device)
		}
	}
	if len(unlocked) == 0 {
		return nil, fmt.Errorf("no unlocked drives")
	}

	fullyUnlocked := len(locked) == 0
	var warning string
	if !fullyUnlocked {
		warning = fmt.Sprintf("WARNING: locked drives: %s", strings.Join(locked, ", "))
	}

	bootDrive := unlocked[0]

	// Use lsblk with a TYPE filter to get only partition entries,
	// avoiding the fragile parts[1:] index assumption. (Fix #3)
	out, err := exec.Command("lsblk", "-ln", "-o", "NAME,TYPE", bootDrive).Output()
	if err != nil {
		return nil, err
	}

	mountPoint := "/mnt/proxmox"
	os.MkdirAll(mountPoint, 0755)

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "part" {
			continue
		}
		part := "/dev/" + fields[0]

		if err := exec.Command("mount", "-r", part, mountPoint).Run(); err != nil {
			continue
		}
		unmount := func() { exec.Command("umount", mountPoint).Run() }

		kernels, _ := filepath.Glob(mountPoint + "/boot/vmlinuz-*-pve")
		initrds, _ := filepath.Glob(mountPoint + "/boot/initrd.img-*-pve")

		if len(kernels) > 0 && len(initrds) > 0 {
			kernel := kernels[len(kernels)-1]
			initrd := initrds[len(initrds)-1]

			cmdline, err := findBootCmdline(mountPoint, kernel)
			if err != nil {
				unmount()
				return nil, err
			}
			unmount()

			if err := exec.Command("kexec", "-l", kernel, "--initrd="+initrd, "--append="+cmdline).Run(); err != nil {
				return nil, err
			}
			go func() {
				time.Sleep(500 * time.Millisecond)
				exec.Command("kexec", "-e").Run()
			}()
			return &BootResult{
				Kernel:        kernel,
				Initrd:        initrd,
				Cmdline:       cmdline,
				Drives:        drives,
				Warning:       warning,
				FullyUnlocked: fullyUnlocked,
			}, nil
		}
		unmount()
	}
	return nil, fmt.Errorf("no bootable partition")
}

// extractLinuxCmdline parses a GRUB "linux" or "linuxefi" line.
// It returns the command line plus a validity flag.
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

// findBootCmdline tries three sources in priority order:
//  1. systemd-boot loader entries  (modern Proxmox)
//  2. grub.cfg under /boot/grub/   (legacy BIOS GRUB)
//  3. grub.cfg under /grub/        (some EFI layouts)
//
// The three source-specific functions are expressed as a slice of closures
// rather than three separate named functions, halving the boilerplate. (Size #2)
func findBootCmdline(mountPoint, kernel string) (string, error) {
	kernelBase := filepath.Base(kernel)

	candidates := []func() (string, bool, error){
		func() (string, bool, error) {
			// systemd-boot: scan *.conf files in loader/entries/
			entriesDir := filepath.Join(mountPoint, "loader", "entries")
			files, err := filepath.Glob(filepath.Join(entriesDir, "*.conf"))
			if err != nil || len(files) == 0 {
				return "", false, fmt.Errorf("no loader entries in %s", entriesDir)
			}
			sort.Strings(files)
			for _, f := range files {
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
			// GRUB config at /boot/grub/grub.cfg
			return parseGrubCfg(filepath.Join(mountPoint, "boot", "grub", "grub.cfg"), kernelBase)
		},
		func() (string, bool, error) {
			// GRUB config at /grub/grub.cfg (some EFI layouts)
			return parseGrubCfg(filepath.Join(mountPoint, "grub", "grub.cfg"), kernelBase)
		},
	}

	for _, try := range candidates {
		if cmdline, found, err := try(); err == nil && found {
			return cmdline, nil
		}
	}
	return "", fmt.Errorf("unable to determine target kernel command line from %s", mountPoint)
}

// parseGrubCfg extracts the kernel command line from a grub.cfg file.
// It looks for a "linux" or "linuxefi" directive that references kernelBase.
func parseGrubCfg(grubPath, kernelBase string) (string, bool, error) {
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
		printConsoleStatus(currentStatus()) // single scan shared by drives + interfaces
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
		printConsoleStatus(currentStatus()) // single scan shared by drives + interfaces

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
			changePassword(string(curr), string(newP))

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

// jsonResponse writes v as JSON with the given status code.
// The encode error is explicitly discarded — a client disconnect mid-stream
// is not actionable and would only pollute the log. (Fix #6)
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
		// Tiny Core's dropbear.tcz ships the multi-call binary only. Create a
		// temporary symlink so argv[0] is "dropbear" when we exec it.
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
	rsaKey := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_rsa_host_key",
		"/etc/dropbear/dropbear_rsa_host_key",
	)
	if ecdsaKey == "" && rsaKey == "" {
		log.Println("[ssh] dropbear keys not present; SSH UI disabled")
		return
	}

	args := []string{
		"-R", // create host keys on first connection if needed
		"-E", // log to stderr
		"-F", // stay in foreground under this process supervisor
		"-p", "2222",
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

// makeSystemActionHandler returns an http.HandlerFunc that runs cmd after a
// 500ms delay and responds with {"status": label}.
// Reboot and poweroff intentionally remain available even when no drive is
// unlocked so operators can recover from bad states without local console.
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
	go consoleInterface()
	startSSHService()

	httpErrorLog := log.New(filteredHTTPLogWriter{}, "", 0)

	// Port 80: redirect all HTTP to HTTPS.
	go func() {
		redirectSrv := &http.Server{
			Addr:     ":80",
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

	// Static files served from ./static/ to prevent the binary, certs, and
	// keys from being downloadable via the web interface.
	mux.Handle("/", http.FileServer(http.Dir("static")))

	// GET /status — current drive and interface state; polled every 5s by index.html.
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, currentStatus())
	})

	// GET /diagnostics — per-drive sedutil --query details for troubleshooting.
	mux.HandleFunc("/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, DiagnosticsResponse{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Drives:      collectDriveDiagnostics(),
		})
	})

	// GET /password-policy — active complexity policy; consumed by index.html.
	mux.HandleFunc("/password-policy", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, passwordPolicy)
	})

	// POST /unlock — attempt to unlock all locked drives with the given password.
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

	// POST /change-password — change drive password on all detected drives.
	mux.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireSessionToken(w, r) {
			return
		}
		var req struct {
			CurrentPassword string `json:"currentPassword"`
			NewPassword     string `json:"newPassword"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if err := validatePassword(req.NewPassword); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, PasswordChangeResponse{
			Results: changePassword(req.CurrentPassword, req.NewPassword),
		})
	})

	// POST /expert/auth — unlock expert actions with a build-time password hash.
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

	// POST /expert/revert-tper — destructive: resets TPer with current password.
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

	// POST /expert/psid-revert — destructive: full erase using PSID.
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

	// POST /boot — load and execute the Proxmox kernel via kexec.
	mux.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		if requireSessionToken(w, r) {
			return
		}
		res, err := BootSystem()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, res)
	})

	// POST /reboot  — cold reboot (re-locks drives; PBA starts again).
	// POST /poweroff — full shutdown.
	// Both use the makeSystemActionHandler factory. (Size #3)
	mux.HandleFunc("/reboot", makeSystemActionHandler("rebooting", "reboot", "-nf"))
	mux.HandleFunc("/poweroff", makeSystemActionHandler("powering off", "poweroff", "-nf"))

	httpsSrv := &http.Server{
		Addr:     ":443",
		Handler:  mux,
		ErrorLog: httpErrorLog,
	}
	log.Fatal(httpsSrv.ListenAndServeTLS("server.crt", "server.key"))
}


