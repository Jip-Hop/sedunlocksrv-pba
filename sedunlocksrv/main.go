package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term" // Required for masked console input
)

type DriveStatus struct {
	Device string `json:"device"`
	Locked bool   `json:"locked"`
}

type UnlockResponse struct {
	Message string        `json:"message"`
	Drives  []DriveStatus `json:"drives"`
	Error   string        `json:"error,omitempty"`
}

type PasswordRequest struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// Global channel to signal a successful unlock to all listeners
var unlockDone = make(chan bool)

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
			// Unlock and reveal partitions
			exec.Command("sedutil-cli", "--setlockingrange", "0", "rw", password, drive.Device).Run()
			exec.Command("sedutil-cli", "--setmbrdone", "on", password, drive.Device).Run()
			success = true
		}
	}
	return success
}

func consoleListener() {
	for {
		fmt.Print("\n🔑 Enter SED password (Console): ")
		
		// term.ReadPassword provides secure input without echoing to screen
		// Note: Most standard Linux terminals don't show '*' for ReadPassword
		// To show '*' manually requires a custom byte-by-byte loop (similar to ash)
		bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			log.Println("Console read error:", err)
			continue
		}
		
		password := string(bytePassword)
		if attemptUnlock(password) {
			fmt.Println("\n✅ Drive(s) unlocked via Console!")
			unlockDone <- true
			return
		}
		fmt.Println("\n❌ Invalid password. Try again.")
	}
}

func main() {
	// 1. Start the Console Listener in the background
	go consoleListener()

	// 2. HTTP to HTTPS Redirect (Port 80)
	go func() {
		log.Println("HTTP redirect server on :80...")
		http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
		}))
	}()

	// 3. Main Handlers (Port 443)
	http.Handle("/", http.FileServer(http.Dir(".")))

	http.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		var req PasswordRequest
		json.NewDecoder(r.Body).Decode(&req)
		if attemptUnlock(req.Password) {
			unlockDone <- true
		}
		json.NewEncoder(w).Encode(UnlockResponse{Message: "Processed", Drives: scanDrives()})
	})

	http.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		log.Println("Triggering kexec...")
		go exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run()
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) {
		go exec.Command("reboot", "-nf").Run()
		w.WriteHeader(http.StatusOK)
	})

	// 4. Start HTTPS Server
	log.Println("HTTPS server on :443...")
	log.Fatal(http.ListenAndServeTLS(":443", "server.crt", "server.key", nil))
}
