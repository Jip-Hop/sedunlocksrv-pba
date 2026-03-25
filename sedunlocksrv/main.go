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
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"
)

// ============================================================
// DATA TYPES
// These structs are used as JSON request/response bodies across
// the HTTP API, the web UI (index.html), and the SSH script
// (ssh_sed_unlock.sh) which parses the JSON with grep/awk.
// ============================================================

// DriveStatus represents a single drive detected by sedutil-cli.
// Returned by /status, which is polled every 5 seconds by index.html
// and every 10 seconds by the SSH script.
type DriveStatus struct {
	Device string `json:"device"` // e.g. "/dev/sda"
	Locked bool   `json:"locked"` // true if the drive's locking range is active
	Opal   bool   `json:"opal"`   // true if the drive supports TCG OPAL 2.x
}

// UnlockResult is the per-drive outcome of a single unlock attempt.
type UnlockResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
}

// UnlockResponse is the top-level response body for POST /unlock.
// index.html currently checks only for the presence of an "error" key;
// ssh_sed_unlock.sh uses grep to check for "error" in the raw response.
type UnlockResponse struct {
	Results []UnlockResult `json:"results"`
}

// PasswordChangeResult is the per-drive outcome of a password change.
// A drive can fail independently of others (e.g. wrong current password
// on one drive but not another).
type PasswordChangeResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"` // omitted from JSON when empty
}

// PasswordChangeResponse is the top-level response body for POST /change-password.
type PasswordChangeResponse struct {
	Results []PasswordChangeResult `json:"results"`
}

// BootResult is the response body for POST /boot. It describes the kernel,
// initrd, and cmdline that were loaded via kexec, plus drive state at the
// time of boot. The web UI displays the Warning field if any drives were
// still locked when boot was requested.
type BootResult struct {
	Kernel        string        `json:"kernel"`
	Initrd        string        `json:"initrd"`
	Cmdline       string        `json:"cmdline"`
	Drives        []DriveStatus `json:"drives"`
	Warning       string        `json:"warning,omitempty"` // set if any drives were still locked
	FullyUnlocked bool          `json:"fullyUnlocked"`
}

// PasswordPolicy describes the complexity requirements enforced when this tool
// *sets* a new password via /change-password or the console P menu.
// It is NOT applied to unlock attempts — the drive may have been initialized
// by another tool (e.g. sedutil-cli directly) with a password that doesn't
// meet these requirements, and we must still be able to unlock it.
// The policy is exposed via GET /password-policy so index.html can display
// the current requirements in the Change Password tab.
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
	// failedAttempts counts consecutive failed unlock attempts across all
	// interfaces (web, SSH, console). It is reset to zero on any success.
	// Protected by mu.
	failedAttempts int
	maxAttempts    = 5 // power off after this many consecutive failures

	// mu protects failedAttempts from concurrent modification. Two simultaneous
	// unlock requests (e.g. web UI and SSH) could otherwise both read a value
	// below maxAttempts and both proceed, potentially overshooting the limit.
	mu sync.Mutex

	// passwordPolicy is loaded once at startup from environment variables.
	// Defaults match the README's "enterprise-grade" security requirements.
	passwordPolicy = loadPolicy()
)

// ============================================================
// PASSWORD POLICY
// ============================================================

// loadPolicy reads password policy settings from environment variables.
// This allows the policy to be tuned at build time by setting ENV values
// in tc-config without recompiling the binary.
//
// Environment variables and their defaults:
//   MIN_PASSWORD_LENGTH  (default: 12)
//   REQUIRE_UPPER        (default: true)
//   REQUIRE_LOWER        (default: true)
//   REQUIRE_NUMBER       (default: true)
//   REQUIRE_SPECIAL      (default: true)
func loadPolicy() PasswordPolicy {
	getBool := func(k string, def bool) bool {
		v := os.Getenv(k)
		if v == "" {
			return def
		}
		return v == "true"
	}

	getInt := func(k string, def int) int {
		v := os.Getenv(k)
		if i, err := strconv.Atoi(v); err == nil {
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

// validatePassword checks a proposed new password against the loaded policy.
// This is called only when *setting* a password (POST /change-password and
// the console P menu). It is intentionally NOT called during unlock — see
// the PasswordPolicy comment above for the rationale.
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
			// Anything that isn't a letter or digit counts as a special character.
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
// It is called on every status poll, unlock attempt, and boot request, so its
// results always reflect the current state of the hardware.
//
// sedutil-cli --scan output format (one line per drive):
//   /dev/sda  2  Samsung SSD 850 ...
//   ^device   ^OPAL version
//
// sedutil-cli --query /dev/sda output includes "Locked = Y" when locked.
func scanDrives() []DriveStatus {
	var statuses []DriveStatus

	out, err := exec.Command("sedutil-cli", "--scan").Output()
	if err != nil {
		// If sedutil-cli is not available or fails, return an empty list.
		// Callers handle an empty drive list gracefully.
		return statuses
	}

	lines := strings.Split(string(out), "\n")

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		dev := fields[0]

		// Skip lines that aren't device paths (e.g. header lines).
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}

		// The second field is the OPAL version string reported by sedutil.
		// We check HasPrefix("2") rather than Contains("2") to avoid false
		// positives from strings like "12" or "20".
		opal := strings.HasPrefix(fields[1], "2")

		// Query the individual drive for its current lock state.
		query, _ := exec.Command("sedutil-cli", "--query", dev).Output()
		locked := strings.Contains(string(query), "Locked = Y")

		statuses = append(statuses, DriveStatus{
			Device: dev,
			Locked: locked,
			Opal:   opal,
		})
	}

	return statuses
}

// ============================================================
// UNLOCK
// ============================================================

// attemptUnlock tries to unlock all currently-locked drives with the given
// password. It is the handler for all three interfaces:
//   - POST /unlock        (web UI and SSH script)
//   - console ENTER key   (physical terminal / TUI)
//
// Rate limiting: after maxAttempts consecutive failures across all interfaces,
// the system powers off to prevent brute-force attacks. The counter resets to
// zero on any successful unlock.
//
// The password is only checked for blankness here. Full complexity requirements
// (validatePassword) are NOT enforced — see PasswordPolicy comment for rationale.
func attemptUnlock(password string) ([]UnlockResult, error) {
	// Reject blank passwords immediately. No point sending them to sedutil-cli
	// and burning a rate-limit attempt on an obviously invalid input.
	if strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("password cannot be blank")
	}

	// Check the lockout state before doing any work. This is done under the
	// mutex so that two concurrent unlock requests (e.g. web UI tab and SSH
	// session open simultaneously) can't both slip past the check at the same
	// time.
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
			// Already unlocked — skip. The web UI's Boot button becomes visible
			// once at least one drive reports unlocked.
			continue
		}

		// Two sedutil-cli calls are required per drive to fully unlock it:
		//   1. --setlockingrange 0 rw  — enables read/write access to locking range 0
		//   2. --setmbrdone on          — clears the MBR shadow so the real MBR is visible
		err1 := exec.Command("sedutil-cli",
			"--setlockingrange", "0", "rw", password, d.Device).Run()

		err2 := exec.Command("sedutil-cli",
			"--setmbrdone", "on", password, d.Device).Run()

		success := err1 == nil && err2 == nil

		if success {
			successAny = true
		}

		results = append(results, UnlockResult{
			Device:  d.Device,
			Success: success,
		})
	}

	// Update the failure counter under the mutex.
	mu.Lock()
	defer mu.Unlock()

	if successAny {
		// At least one drive unlocked — reset the counter.
		failedAttempts = 0
	} else {
		failedAttempts++
		log.Printf("Failed unlock attempt %d/%d\n", failedAttempts, maxAttempts)

		if failedAttempts >= maxAttempts {
			log.Println("Max failed attempts reached. Powering off.")
			// Sleep briefly so the HTTP response (or console message) can be
			// delivered before the machine loses power.
			go func() {
				time.Sleep(500 * time.Millisecond)
				exec.Command("poweroff", "-nf").Run()
			}()
			return results, fmt.Errorf("maximum failed attempts reached")
		}
	}

	return results, nil
}

// ============================================================
// PASSWORD CHANGE
// ============================================================

// changePassword updates the SID and Admin1 passwords on all detected drives.
// It is called by POST /change-password (web UI Change Password tab) and by
// the console P menu.
//
// Both passwords must be updated together — SID is the master credential used
// by sedutil for administrative operations, Admin1 is the locking credential
// used during unlock. If they diverge, the drive may become difficult to manage.
//
// NOTE: This function does not independently verify that `current` is correct.
// A wrong current password will cause sedutil-cli to fail, which is reported
// as success:false with a generic error message. The caller is responsible for
// communicating the result to the user.
func changePassword(current, newPw string) []PasswordChangeResult {
	var results []PasswordChangeResult

	for _, d := range scanDrives() {
		// Update the SID (Security ID) password — used for sedutil admin operations.
		err1 := exec.Command("sedutil-cli",
			"--setsidpassword", current, newPw, d.Device).Run()

		// Update the Admin1 password — used for locking range operations (unlock).
		err2 := exec.Command("sedutil-cli",
			"--setadmin1password", current, newPw, d.Device).Run()

		success := err1 == nil && err2 == nil

		var errMsg string
		if !success {
			errMsg = "failed to update password"
		}

		results = append(results, PasswordChangeResult{
			Device:  d.Device,
			Success: success,
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
// This is called by POST /boot (web UI Boot button, SSH B option) and the
// console B key.
//
// Using kexec rather than a cold reboot keeps the drives authorized across the
// transition — a cold reboot would cause the drives to re-lock (requiring the
// PBA again), whereas kexec performs a "warm" kernel switch without power cycling.
//
// The function returns a BootResult immediately so the caller can send an HTTP
// response before kexec fires. Kexec itself runs in a short-delay goroutine to
// ensure the response is flushed first.
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

	// If any drives are still locked, include a warning in the response.
	// The web UI and SSH script display this to the user but still proceed
	// with booting.
	var warning string
	if !fullyUnlocked {
		warning = fmt.Sprintf("WARNING: locked drives: %s", strings.Join(locked, ", "))
	}

	// Use the first unlocked drive as the boot source.
	bootDrive := unlocked[0]

	// List partitions on the boot drive using lsblk.
	out, err := exec.Command("lsblk", "-ln", "-o", "NAME", bootDrive).Output()
	if err != nil {
		return nil, err
	}

	parts := strings.Fields(string(out))
	mountPoint := "/mnt/proxmox"
	os.MkdirAll(mountPoint, 0755)

	// Try each partition in turn until we find one with a Proxmox kernel.
	for _, p := range parts[1:] { // parts[0] is the drive itself; skip it
		part := "/dev/" + p

		// Mount read-only — we only need to read the kernel/initrd files.
		if err := exec.Command("mount", "-r", part, mountPoint).Run(); err != nil {
			continue // not a mountable filesystem — try the next partition
		}

		// Ensure the partition is unmounted on every path out of this block,
		// including error returns, to avoid leaving stale mounts.
		unmount := func() { exec.Command("umount", mountPoint).Run() }

		// Look for Proxmox-specific kernel and initrd naming conventions.
		kernels, _ := filepath.Glob(mountPoint + "/boot/vmlinuz-*-pve")
		initrds, _ := filepath.Glob(mountPoint + "/boot/initrd.img-*-pve")

		if len(kernels) > 0 && len(initrds) > 0 {
			// Use the last (lexicographically highest = most recent) kernel version.
			kernel := kernels[len(kernels)-1]
			initrd := initrds[len(initrds)-1]

			// Reuse the current kernel's cmdline so Proxmox boots with the same
			// parameters it was configured with (e.g. root device, console settings).
			cmdlineBytes, _ := os.ReadFile("/proc/cmdline")
			cmdline := strings.TrimSpace(string(cmdlineBytes))

			unmount()

			// Stage the kernel into kexec. This does not yet transfer control.
			if err := exec.Command("kexec",
				"-l", kernel,
				"--initrd="+initrd,
				"--append="+cmdline,
			).Run(); err != nil {
				return nil, err
			}

			// Execute the staged kernel after a brief delay so the HTTP response
			// (or console output) can be delivered before control transfers.
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

// ============================================================
// CONSOLE TUI
// ============================================================

// privacyTimeout is the duration of inactivity in the active menu before the
// console returns to the standby screen. This prevents the active menu (which
// shows drive status and accepts commands) from remaining visible indefinitely
// on an unattended physical terminal.
const privacyTimeout = 30 * time.Second

// consoleInterface is the outermost loop of the physical terminal UI.
// It runs in a goroutine alongside the HTTP server so both interfaces are
// available simultaneously.
//
// The console has two states:
//   STANDBY — shows drive status, waits for any keypress to become active
//   ACTIVE  — shows the command menu, returns to standby after privacyTimeout
func consoleInterface() {
	fd := int(os.Stdin.Fd())

	for {
		// Standby screen: show current drive status and wait for a keypress.
		fmt.Print("\033[H\033[2J") // clear screen (ANSI escape)
		fmt.Println("🔒 PBA STANDBY")

		for _, d := range scanDrives() {
			if d.Locked {
				fmt.Println("❌", d.Device)
			} else {
				fmt.Println("✅", d.Device)
			}
		}

		fmt.Println("\nPress any key...")

		// Switch stdin to raw mode so we receive individual keypresses
		// without waiting for Enter.
		old, err := term.MakeRaw(fd)
		if err != nil {
			log.Printf("term.MakeRaw failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		term.Restore(fd, old)

		// Any key transitions to the active command menu.
		activeMenu(fd)
	}
}

// activeMenu shows the command menu on the physical terminal and processes
// keypresses until the privacy timeout expires or the user navigates away.
// It returns to consoleInterface (standby) when done.
func activeMenu(fd int) {
	for {
		// Redraw the active menu on every iteration to reflect current drive state.
		fmt.Print("\033[H\033[2J") // clear screen
		fmt.Println("🔑 ACTIVE MODE")

		for _, d := range scanDrives() {
			if d.Locked {
				fmt.Println("❌", d.Device)
			} else {
				fmt.Println("✅", d.Device)
			}
		}

		fmt.Println("\n[ENTER] Unlock  [B] Boot  [P] Change PW  [R] Reboot  [S] Shutdown")

		old, err := term.MakeRaw(fd)
		if err != nil {
			log.Printf("term.MakeRaw failed: %v", err)
			return // back to standby
		}

		// Read a single keypress in a goroutine so we can also select on
		// the privacy timeout channel.
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
				return // stdin error — back to standby
			}
			key = strings.ToUpper(string(res.b))
		case <-time.After(privacyTimeout):
			// No keypress within the timeout window — return to standby screen.
			term.Restore(fd, old)
			return
		}

		switch key {

		case "\r": // Enter — prompt for password and attempt unlock
			fmt.Print("Password: ")
			pw, _ := term.ReadPassword(fd)
			_, err := attemptUnlock(string(pw))
			if err != nil {
				fmt.Println("\n❌", err)
				time.Sleep(2 * time.Second)
			}

		case "B": // Boot — load Proxmox kernel via kexec
			res, err := BootSystem()
			if err != nil {
				fmt.Println(err)
			} else if res.Warning != "" {
				fmt.Println(res.Warning)
			}
			time.Sleep(2 * time.Second)

		case "P": // Change Password — prompts for current and new passwords
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

			// validatePassword is called here (setting a new password) but NOT
			// during unlock — drives may have been initialized with passwords
			// that don't meet policy, and we must still be able to unlock them.
			if err := validatePassword(string(newP)); err != nil {
				fmt.Println("\n❌", err)
				time.Sleep(2 * time.Second)
				break
			}

			changePassword(string(curr), string(newP))

		case "R": // Reboot
			exec.Command("reboot", "-nf").Run()

		case "S": // Shutdown
			exec.Command("poweroff", "-nf").Run()
		}
	}
}

// ============================================================
// HTTP HELPERS
// ============================================================

// jsonResponse writes v as JSON to w with the given HTTP status code.
// It sets Content-Type so browsers and clients correctly interpret the body.
// All API endpoints use this instead of calling json.NewEncoder directly,
// which would leave the Content-Type as the default "text/plain".
func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// requireMethod writes a 405 and returns true if the request method doesn't
// match. Handlers call this at the top and return immediately if it returns
// true. This prevents mutating endpoints (unlock, boot, reboot, etc.) from
// being triggered by browser prefetches, bookmarks, or GET-based attacks.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		jsonResponse(w, http.StatusMethodNotAllowed,
			map[string]string{"error": "method not allowed"})
		return true
	}
	return false
}

// ============================================================
// MAIN — HTTP SERVER
// ============================================================

func main() {
	// Start the console TUI in a background goroutine. It runs independently
	// of the HTTP server and shares the same underlying functions (attemptUnlock,
	// BootSystem, etc.) that the HTTP handlers use.
	go consoleInterface()

	// Port 80: redirect all HTTP requests to HTTPS. This ensures the web UI
	// is always accessed over an encrypted connection, even if the user types
	// the IP address without "https://".
	go http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
	}))

	// Use an explicit mux (rather than http.DefaultServeMux) so that the HTTPS
	// server and the HTTP redirect server don't share the same handler table.
	mux := http.NewServeMux()

	// Static files (index.html, any CSS/JS) are served from ./static/ rather
	// than the working directory. This prevents server.crt, server.key, and
	// the sedunlocksrv binary from being downloadable via the web interface.
	mux.Handle("/", http.FileServer(http.Dir("static")))

	// GET /status — returns the current locked/unlocked state of all drives.
	// Polled every 5 seconds by index.html's setInterval(refresh, 5000) and
	// every 10 seconds by the SSH script's read -t 10 loop.
	// The Boot button in index.html is shown/hidden based on this response.
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"drives": scanDrives(),
		})
	})

	// GET /password-policy — returns the active password complexity policy.
	// Consumed by index.html's loadPolicy() to display requirements in the
	// Change Password tab. Not used by the SSH script (which has no UI for
	// setting passwords).
	mux.HandleFunc("/password-policy", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodGet) {
			return
		}
		jsonResponse(w, http.StatusOK, passwordPolicy)
	})

	// POST /unlock — attempts to unlock all locked drives with the given password.
	// Called by index.html's unlock() function and ssh_sed_unlock.sh's U option.
	// Request body:  { "password": "..." }
	// Success:       { "results": [{ "device": "/dev/sda", "success": true }] }
	// Failure:       { "error": "..." }  with HTTP 403
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

		jsonResponse(w, http.StatusOK, UnlockResponse{Results: results})
	})

	// POST /change-password — changes the drive password on all detected drives.
	// Called only by index.html's changePw() function (the SSH script has no
	// password change UI). The new password is validated against the policy
	// before any sedutil-cli calls are made.
	// Request body:  { "currentPassword": "...", "newPassword": "..." }
	// Success:       { "results": [...] }
	// Policy error:  { "error": "..." }  with HTTP 400
	mux.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
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

		// Enforce password policy on the new password. This is the only place
		// in the codebase where validatePassword is called — unlock does not
		// use it (see PasswordPolicy comment for rationale).
		if err := validatePassword(req.NewPassword); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		jsonResponse(w, http.StatusOK, PasswordChangeResponse{
			Results: changePassword(req.CurrentPassword, req.NewPassword),
		})
	})

	// POST /boot — loads and executes the Proxmox kernel via kexec.
	// Called by index.html's boot() function (Boot button, visible only when
	// at least one drive is unlocked) and ssh_sed_unlock.sh's B option.
	// The response is sent before kexec fires (500ms delay in BootSystem).
	mux.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}

		res, err := BootSystem()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, res)
	})

	// POST /reboot — performs a cold reboot of the PBA system.
	// Note: unlike /boot (kexec), this does a full hardware reboot, which will
	// re-lock the drives and bring the PBA up again from scratch.
	// Called by index.html's reboot() and ssh_sed_unlock.sh's R option.
	mux.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		// Send the response before sleeping, so the client receives confirmation.
		jsonResponse(w, http.StatusOK, map[string]string{"status": "rebooting"})
		go func() {
			time.Sleep(500 * time.Millisecond)
			exec.Command("reboot", "-nf").Run()
		}()
	})

	// POST /poweroff — shuts the system down completely.
	// Called by index.html's shutdown() and ssh_sed_unlock.sh's S option.
	mux.HandleFunc("/poweroff", func(w http.ResponseWriter, r *http.Request) {
		if requireMethod(w, r, http.MethodPost) {
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "powering off"})
		go func() {
			time.Sleep(500 * time.Millisecond)
			exec.Command("poweroff", "-nf").Run()
		}()
	})

	// Start the HTTPS server. This call blocks until the server exits (which
	// in practice only happens on a fatal error, since the system powers off
	// or reboots through other means).
	// server.crt and server.key are generated by make-cert.sh during the build.
	log.Fatal(http.ListenAndServeTLS(":443", "server.crt", "server.key", mux))
}
