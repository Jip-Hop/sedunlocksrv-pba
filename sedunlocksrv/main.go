package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"
)

type DriveStatus struct {
	Device string `json:"device"`
	Locked bool   `json:"locked"`
}

type PasswordRequest struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type UnlockResponse struct {
	Message string        `json:"message"`
	Drives  []DriveStatus `json:"drives"`
	Error   string        `json:"error,omitempty"`
}

// --- PASSWORD VALIDATION ---
func validatePassword(password string) error {
	var hasUpper, hasLower, hasNumber, hasSpecial bool
	if len(password) < 12 { return fmt.Errorf("at least 12 characters required") }
	for _, char := range password {
		switch {
		case unicode.IsUpper(char): hasUpper = true
		case unicode.IsLower(char): hasLower = true
		case unicode.IsNumber(char): hasNumber = true
		case strings.ContainsRune("!@#$%^&*(),.?\":{}|<>", char): hasSpecial = true
		}
	}
	if !hasUpper || !hasLower || !hasNumber || !hasSpecial {
		return fmt.Errorf("missing: Upper, Lower, Number, or Special char")
	}
	return nil
}

// --- DRIVE LOGIC ---
func scanDrives() []DriveStatus {
	var statuses []DriveStatus
	out, err := exec.Command("sh", "-c", "sedutil-cli --scan | awk '$1 ~ /\\/dev\\// && $2 ~ /2/ {print $1}'").Output()
	if err != nil { return statuses }
	devices := strings.Fields(string(out))
	for _, dev := range devices {
		queryOut, _ := exec.Command("sedutil-cli", "--query", dev).Output()
		isLocked := strings.Contains(string(queryOut), "Locked = Y")
		statuses = append(statuses, DriveStatus{Device: dev, Locked: isLocked})
	}
	return statuses
}

func attemptUnlock(password string) bool {
	drives := scanDrives()
	success := false
	for _, d := range drives {
		if d.Locked {
			exec.Command("sedutil-cli", "--setlockingrange", "0", "rw", password, d.Device).Run()
			exec.Command("sedutil-cli", "--setmbrdone", "on", password, d.Device).Run()
			success = true
		}
	}
	return success
}

// --- CONSOLE INTERFACE ---
func consoleInterface() {
	fd := int(os.Stdin.Fd())
	for {
		// Standby Mode
		fmt.Print("\033[H\033[2J")
		fmt.Println("🔒 PBA CONSOLE STANDBY")
		drives := scanDrives()
		for _, d := range drives {
			s := "✅"; if d.Locked { s = "❌" }
			fmt.Printf(" %s %s ", s, d.Device)
		}
		fmt.Println("\n\nPress any key to manage...")

		oldState, _ := term.MakeRaw(fd)
		b := make([]byte, 1); os.Stdin.Read(b)
		term.Restore(fd, oldState)

		// Active Menu with 30s Timeout
		active := true
		for active {
			fmt.Print("\033[H\033[2J")
			fmt.Println("🔑 SED UNLOCK SERVICE - ACTIVE")
			for _, d := range scanDrives() {
				s := "✅ UNLOCKED"; if d.Locked { s = "❌ LOCKED" }
				fmt.Printf("  %s: %s\n", d.Device, s)
			}
			fmt.Println("\n[ENTER] Unlock [F] Refresh [B] Boot [P] Change PW [R] Reboot [S] Shutdown")
			fmt.Println("(Idle for 30s returns to standby)")

			oldState, _ = term.MakeRaw(fd)
			charChan := make(chan string)
			go func() {
				buf := make([]byte, 1); os.Stdin.Read(buf)
				charChan <- strings.ToUpper(string(buf))
			}()

			select {
			case input := <-charChan:
				term.Restore(fd, oldState)
				switch input {
				case "\r", "\n":
					fmt.Print("\nEnter Password: ")
					pw, _ := term.ReadPassword(fd); attemptUnlock(string(pw))
				case "F": // Refresh handled by loop
					continue
				case "B":
					exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run()
				case "P":
					consoleChangePassword(fd)
				case "R":
					fmt.Print("\nConfirm REBOOT? (y/n): ")
					r := make([]byte, 1); os.Stdin.Read(r)
					if strings.ToUpper(string(r)) == "Y" { exec.Command("reboot", "-nf").Run() }
				case "S":
					fmt.Print("\nConfirm SHUTDOWN? (y/n): ")
					s := make([]byte, 1); os.Stdin.Read(s)
					if strings.ToUpper(string(s)) == "Y" { exec.Command("poweroff", "-nf").Run() }
				}
			case <-time.After(30 * time.Second):
				term.Restore(fd, oldState)
				active = false 
			}
		}
	}
}

func consoleChangePassword(fd int) {
	fmt.Println("\n--- Change Password ---")
	fmt.Print("Current: "); curr, _ := term.ReadPassword(fd)
	fmt.Print("\nNew:     "); newP, _ := term.ReadPassword(fd)
	fmt.Print("\nConfirm: "); conf, _ := term.ReadPassword(fd)
	if string(newP) != string(conf) || validatePassword(string(newP)) != nil {
		fmt.Println("\n❌ Invalid/Mismatch!"); time.Sleep(2 * time.Second); return
	}
	for _, d := range scanDrives() {
		exec.Command("sedutil-cli", "--setsidpassword", string(curr), string(newP), d.Device).Run()
		exec.Command("sedutil-cli", "--setadmin1password", string(curr), string(newP), d.Device).Run()
	}
	fmt.Println("\n✅ Success!"); time.Sleep(1 * time.Second)
}

func main() {
	go consoleInterface()
	go http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
	}))

	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"drives": scanDrives()})
	})
	http.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		var req PasswordRequest; json.NewDecoder(r.Body).Decode(&req)
		attemptUnlock(req.Password)
		json.NewEncoder(w).Encode(UnlockResponse{Message: "Done", Drives: scanDrives()})
	})
	http.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		var req PasswordRequest; json.NewDecoder(r.Body).Decode(&req)
		if err := validatePassword(req.NewPassword); err != nil {
			w.WriteHeader(400); json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); return
		}
		for _, d := range scanDrives() {
			exec.Command("sedutil-cli", "--setsidpassword", req.CurrentPassword, req.NewPassword, d.Device).Run()
			exec.Command("sedutil-cli", "--setadmin1password", req.CurrentPassword, req.NewPassword, d.Device).Run()
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "Done"})
	})
	http.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) { exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run() })
	http.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) { exec.Command("reboot", "-nf").Run() })
	log.Fatal(http.ListenAndServeTLS(":443", "server.crt", "server.key", nil))
}
