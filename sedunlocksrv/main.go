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

// --- PASSWORD VALIDATION LOGIC ---
func validatePassword(password string) error {
	var (
		hasUpper   = false
		hasLower   = false
		hasNumber  = false
		hasSpecial = false
	)
	if len(password) < 12 {
		return fmt.Errorf("password must be at least 12 characters long")
	}
	for _, char := range password {
		switch {
		case unicode.IsUpper(char):
			hasUpper = true
		case unicode.IsLower(char):
			hasLower = true
		case unicode.IsNumber(char):
			hasNumber = true
		case strings.ContainsRune("!@#$%^&*(),.?\":{}|<>", char):
			hasSpecial = true
		}
	}
	if !hasUpper || !hasLower || !hasNumber || !hasSpecial {
		return fmt.Errorf("password requires: Upper, Lower, Number, and Special character")
	}
	return nil
}

func scanDrives() []DriveStatus {
	var statuses []DriveStatus
	out, err := exec.Command("sh", "-c", "sedutil-cli --scan | awk '$1 ~ /\\/dev\\// && $2 ~ /2/ {print $1}'").Output()
	if err != nil {
		return statuses
	}
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
	for _, drive := range drives {
		if drive.Locked {
			exec.Command("sedutil-cli", "--setlockingrange", "0", "rw", password, drive.Device).Run()
			exec.Command("sedutil-cli", "--setmbrdone", "on", password, drive.Device).Run()
			success = true
		}
	}
	return success
}

func clearConsole() {
	fmt.Print("\033[H\033[2J")
}

func consoleInterface() {
	fd := int(os.Stdin.Fd())
	for {
		clearConsole()
		fmt.Println("🔒 PBA CONSOLE STANDBY")
		drives := scanDrives()
		anyLocked := false
		for _, d := range drives {
			status := "✅"
			if d.Locked { status = "❌"; anyLocked = true }
			fmt.Printf("  %s %s  ", status, d.Device)
		}
		fmt.Printf("\n\nSystem: %s\nPress any key to manage...", map[bool]string{true: "LOCKED", false: "READY"}[anyLocked])

		oldState, _ := term.MakeRaw(fd)
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		term.Restore(fd, oldState)

		active := true
		for active {
			clearConsole()
			fmt.Println("🔑 SED UNLOCK SERVICE - ACTIVE")
			drives = scanDrives()
			for _, d := range drives {
				status := "✅ UNLOCKED"
				if d.Locked { status = "❌ LOCKED" }
				fmt.Printf("  %s: %s\n", d.Device, status)
			}
			fmt.Println("\n[ENTER] Unlock  [B] Boot  [P] Change PW  [R] Reboot  [S] Shutdown")

			oldState, _ = term.MakeRaw(fd)
			charChan := make(chan byte)
			go func() {
				b := make([]byte, 1)
				os.Stdin.Read(b)
				charChan <- b
			}()

			select {
			case inputByte := <-charChan:
				term.Restore(fd, oldState)
				input := strings.ToUpper(string(inputByte))
				switch input {
				case "\r", "\n":
					fmt.Print("\nEnter Password: ")
					pw, _ := term.ReadPassword(fd)
					if attemptUnlock(string(pw)) { fmt.Println("\n✅ Success!") } else { fmt.Println("\n❌ Failed.") }
					time.Sleep(1500 * time.Millisecond)
				case "B":
					exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run()
				case "P":
					consoleChangePassword(fd)
				case "R":
					fmt.Print("\n⚠️  Confirm REBOOT? (y/n): ")
					conf := make([]byte, 1); os.Stdin.Read(conf)
					if strings.ToUpper(string(conf)) == "Y" { exec.Command("reboot", "-nf").Run() }
				case "S":
					fmt.Print("\n⚠️  Confirm SHUTDOWN? (y/n): ")
					conf := make([]byte, 1); os.Stdin.Read(conf)
					if strings.ToUpper(string(conf)) == "Y" { exec.Command("poweroff", "-nf").Run() }
				}
			case <-time.After(30 * time.Second):
				term.Restore(fd, oldState)
				active = false 
			}
		}
	}
}

func consoleChangePassword(fd int) {
	fmt.Println("\n--- Change SED Password ---")
	fmt.Println("Complexity: 12+ chars, Upper, Lower, Number, Special (!@#...)")
	fmt.Print("Current Password: ")
	curr, _ := term.ReadPassword(fd)
	fmt.Print("\nNew Password:     ")
	newPw, _ := term.ReadPassword(fd)
	fmt.Print("\nConfirm New PW:   ")
	conf, _ := term.ReadPassword(fd)

	if string(newPw) != string(conf) {
		fmt.Println("\n❌ Passwords do not match!")
		time.Sleep(2 * time.Second); return
	}

	if err := validatePassword(string(newPw)); err != nil {
		fmt.Printf("\n❌ %v\n", err)
		time.Sleep(3 * time.Second); return
	}
	
	devices := scanDrives()
	for _, dev := range devices {
		exec.Command("sedutil-cli", "--setsidpassword", string(curr), string(newPw), dev.Device).Run()
		exec.Command("sedutil-cli", "--setadmin1password", string(curr), string(newPw), dev.Device).Run()
	}
	fmt.Println("\n✅ Password update attempted.")
	time.Sleep(2 * time.Second)
}

func main() {
	go consoleInterface()
	go func() {
		http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
		}))
	}()
	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		var req PasswordRequest
		json.NewDecoder(r.Body).Decode(&req)
		attemptUnlock(req.Password)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UnlockResponse{Message: "Processed", Drives: scanDrives()})
	})
	http.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		var req PasswordRequest
		json.NewDecoder(r.Body).Decode(&req)
		if err := validatePassword(req.NewPassword); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		devices := scanDrives()
		for _, dev := range devices {
			exec.Command("sedutil-cli", "--setsidpassword", req.CurrentPassword, req.NewPassword, dev.Device).Run()
			exec.Command("sedutil-cli", "--setadmin1password", req.CurrentPassword, req.NewPassword, dev.Device).Run()
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "Password update attempted"})
	})
	http.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		go exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run()
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) { go exec.Command("reboot", "-nf").Run() })
	log.Fatal(http.ListenAndServeTLS(":443", "server.crt", "server.key", nil))
}
