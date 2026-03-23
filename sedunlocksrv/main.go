package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
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
		statuses = append(statuses, DriveStatus{
			Device: dev,
			Locked: isLocked,
		})
	}
	return statuses
}

func main() {
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)

	http.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req PasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		drivesBefore := scanDrives()
		for _, drive := range drivesBefore {
			if drive.Locked {
				exec.Command("sedutil-cli", "--setlockingrange", "0", "rw", req.Password, drive.Device).Run()
				exec.Command("sedutil-cli", "--setmbrdone", "on", req.Password, drive.Device).Run()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UnlockResponse{
			Message: "Unlock attempt processed",
			Drives:  scanDrives(),
		})
	})

	http.HandleFunc("/change-password", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req PasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		devices := scanDrives()
		successCount := 0
		for _, dev := range devices {
			cmd := exec.Command("sedutil-cli", "--setsidpassword", req.CurrentPassword, req.NewPassword, dev.Device)
			if err := cmd.Run(); err == nil {
				exec.Command("sedutil-cli", "--setadmin1password", req.CurrentPassword, req.NewPassword, dev.Device).Run()
				successCount++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if successCount > 0 {
			json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("Password updated on %d drive(s)", successCount)})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Failed to update password on any drives"})
		}
	})

	http.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		log.Println("Boot command received. Triggering kexec...")
		go func() {
			cmd := exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()
		}()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Boot sequence initiated")
	})

	http.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) {
		log.Println("Rebooting...")
		w.WriteHeader(http.StatusOK)
		go exec.Command("reboot", "-nf").Run()
	})

	// --- START HTTP REDIRECT SERVER ---
	go func() {
		log.Println("HTTP redirect server starting on :80...")
		err := http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Redirect any HTTP request to the HTTPS equivalent
			target := "https://" + r.Host + r.URL.String()
			http.Redirect(w, r, target, http.StatusMovedPermanently)
		}))
		if err != nil {
			log.Fatalf("HTTP redirect server failed: %v", err)
		}
	}()

	// --- START HTTPS SERVER ---
	log.Println("HTTPS server starting on :443...")
	log.Fatal(http.ListenAndServeTLS(":443", "server.crt", "server.key", nil))
}
