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

type DriveStatus struct {
	Device string `json:"device"`
	Locked bool   `json:"locked"`
}

type UnlockResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
}

type UnlockResponse struct {
	Results []UnlockResult `json:"results"`
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

type PasswordPolicy struct {
	MinLength      int  `json:"minLength"`
	RequireUpper   bool `json:"requireUpper"`
	RequireLower   bool `json:"requireLower"`
	RequireNumber  bool `json:"requireNumber"`
	RequireSpecial bool `json:"requireSpecial"`
}

var (
	failedAttempts int
	maxAttempts    = 5
	mu             sync.Mutex

	passwordPolicy = loadPolicy()
)

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

// ---------------- DRIVE ----------------

func scanDrives() []DriveStatus {
	var statuses []DriveStatus

	out, err := exec.Command("sedutil-cli", "--scan").Output()
	if err != nil {
		return statuses
	}

	lines := strings.Split(string(out), "\n")

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}

		dev := fields[0]

		query, _ := exec.Command("sedutil-cli", "--query", dev).Output()
		locked := strings.Contains(string(query), "Locked = Y")

		statuses = append(statuses, DriveStatus{
			Device: dev,
			Locked: locked,
		})
	}

	return statuses
}

// ---------------- UNLOCK ----------------

func attemptUnlock(password string) ([]UnlockResult, error) {
	var results []UnlockResult
	successAny := false

	for _, d := range scanDrives() {
		if !d.Locked {
			continue
		}

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

	mu.Lock()
	defer mu.Unlock()

	if successAny {
		failedAttempts = 0
	} else {
		failedAttempts++
		log.Printf("Failed unlock attempt %d/%d\n", failedAttempts, maxAttempts)

		if failedAttempts >= maxAttempts {
			log.Println("Max failed attempts reached. Powering off.")
			go exec.Command("poweroff", "-nf").Run()
			return results, fmt.Errorf("maximum failed attempts reached")
		}
	}

	return results, nil
}

// ---------------- PASSWORD CHANGE ----------------

func changePassword(current, new string) []PasswordChangeResult {
	var results []PasswordChangeResult

	for _, d := range scanDrives() {
		err1 := exec.Command("sedutil-cli",
			"--setsidpassword", current, new, d.Device).Run()

		err2 := exec.Command("sedutil-cli",
			"--setadmin1password", current, new, d.Device).Run()

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

// ---------------- BOOT ----------------

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

	out, err := exec.Command("lsblk", "-ln", "-o", "NAME", bootDrive).Output()
	if err != nil {
		return nil, err
	}

	parts := strings.Fields(string(out))
	mountPoint := "/mnt/proxmox"
	os.MkdirAll(mountPoint, 0755)

	for _, p := range parts[1:] {
		part := "/dev/" + p

		if exec.Command("mount", "-r", part, mountPoint).Run() != nil {
			continue
		}

		kernels, _ := filepath.Glob(mountPoint + "/boot/vmlinuz-*-pve")
		initrds, _ := filepath.Glob(mountPoint + "/boot/initrd.img-*-pve")

		if len(kernels) > 0 && len(initrds) > 0 {
			kernel := kernels[len(kernels)-1]
			initrd := initrds[len(initrds)-1]

			cmdlineBytes, _ := os.ReadFile("/proc/cmdline")
			cmdline := strings.TrimSpace(string(cmdlineBytes))

			exec.Command("umount", mountPoint).Run()

			if err := exec.Command("kexec",
				"-l", kernel,
				"--initrd="+initrd,
				"--append="+cmdline,
			).Run(); err != nil {
				return nil, err
			}

			go exec.Command("kexec", "-e").Run()

			return &BootResult{
				Kernel:        kernel,
				Initrd:        initrd,
				Cmdline:       cmdline,
				Drives:        drives,
				Warning:       warning,
				FullyUnlocked: fullyUnlocked,
			}, nil
		}

		exec.Command("umount", mountPoint).Run()
	}

	return nil, fmt.Errorf("no bootable partition")
}

// ---------------- TUI ----------------

func consoleInterface() {
	fd := int(os.Stdin.Fd())

	for {
		fmt.Print("\033[H\033[2J")
		fmt.Println("🔒 PBA STANDBY")

		for _, d := range scanDrives() {
			if d.Locked {
				fmt.Println("❌", d.Device)
			} else {
				fmt.Println("✅", d.Device)
			}
		}

		fmt.Println("\nPress any key...")
		old, _ := term.MakeRaw(fd)
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		term.Restore(fd, old)

		activeMenu(fd)
	}
}

func activeMenu(fd int) {
	for {
		fmt.Print("\033[H\033[2J")
		fmt.Println("🔑 ACTIVE MODE")

		for _, d := range scanDrives() {
			if d.Locked {
				fmt.Println("❌", d.Device)
			} else {
				fmt.Println("✅", d.Device)
			}
		}

		fmt.Println("\n[ENTER] Unlock  [B] Boot  [P] Change PW  [R] Reboot  [S] Shutdown")

		old, _ := term.MakeRaw(fd)
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		term.Restore(fd, old)

		switch strings.ToUpper(string(buf)) {

		case "\r":
			fmt.Print("Password: ")
			pw, _ := term.ReadPassword(fd)
			_, err := attemptUnlock(string(pw))
			if err != nil {
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

// ---------------- HTTP ----------------

func main() {
	go consoleInterface()

	go http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
	}))

	http.Handle("/", http.FileServer(http.Dir(".")))

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"drives": scanDrives(),
		})
	})

	http.HandleFunc("/password-policy", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(passwordPolicy)
	})

	http.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		results, err := attemptUnlock(req.Password)

		if err != nil {
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
			})
			return
		}

		json.NewEncoder(w).Encode(UnlockResponse{Results: results})
	})

	http.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CurrentPassword string `json:"currentPassword"`
			NewPassword     string `json:"newPassword"`
		}

		json.NewDecoder(r.Body).Decode(&req)

		if err := validatePassword(req.NewPassword); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		json.NewEncoder(w).Encode(PasswordChangeResponse{
			Results: changePassword(req.CurrentPassword, req.NewPassword),
		})
	})

	http.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		res, err := BootSystem()
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) {
		go exec.Command("reboot", "-nf").Run()
	})

	http.HandleFunc("/poweroff", func(w http.ResponseWriter, r *http.Request) {
		go exec.Command("poweroff", "-nf").Run()
		json.NewEncoder(w).Encode(map[string]string{"status": "powering off"})
	})

	log.Fatal(http.ListenAndServeTLS(":443", "server.crt", "server.key", nil))
}
