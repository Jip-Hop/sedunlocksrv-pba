package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

////////////////////////////////////////////////////////////
// CORE LAYER (single source of truth)
////////////////////////////////////////////////////////////

type DriveStatus struct {
	Device string `json:"device"`
	Locked bool   `json:"locked"`
}

type UnlockResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
}

var unlockMutex sync.Mutex

func scanDrives() []DriveStatus {
	var results []DriveStatus

	out, err := exec.Command("sedutil-cli", "--scan").Output()
	if err != nil {
		log.Println("scan error:", err)
		return results
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		if len(fields) < 1 {
			continue
		}

		dev := fields[0]
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}

		queryOut, err := exec.Command("sedutil-cli", "--query", dev).Output()
		if err != nil {
			continue
		}

		locked := strings.Contains(string(queryOut), "Locked = Y")

		results = append(results, DriveStatus{
			Device: dev,
			Locked: locked,
		})
	}

	return results
}

func unlockDevice(dev string, password string) bool {
	err1 := exec.Command("sedutil-cli", "--setlockingrange", "0", "rw", password, dev).Run()
	err2 := exec.Command("sedutil-cli", "--setmbrdone", "on", password, dev).Run()

	if err1 != nil || err2 != nil {
		return false
	}

	out, err := exec.Command("sedutil-cli", "--query", dev).Output()
	if err != nil {
		return false
	}

	return !strings.Contains(string(out), "Locked = Y")
}

func unlockAll(password string) []UnlockResult {
	unlockMutex.Lock()
	defer unlockMutex.Unlock()

	drives := scanDrives()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []UnlockResult

	for _, d := range drives {
		if !d.Locked {
			continue
		}

		wg.Add(1)
		go func(dev string) {
			defer wg.Done()
			ok := unlockDevice(dev, password)

			mu.Lock()
			results = append(results, UnlockResult{Device: dev, Success: ok})
			mu.Unlock()
		}(d.Device)
	}

	wg.Wait()
	return results
}

////////////////////////////////////////////////////////////
// HTTP API (used by SSH + web UI)
////////////////////////////////////////////////////////////

func startHTTP() {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"drives": scanDrives(),
		})
	})

	mux.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		results := unlockAll(req.Password)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": results,
		})
	})

	mux.HandleFunc("/boot", func(w http.ResponseWriter, r *http.Request) {
		exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run()
	})

	mux.HandleFunc("/reboot", func(w http.ResponseWriter, r *http.Request) {
		exec.Command("reboot", "-nf").Run()
	})

	srv := &http.Server{
		Addr:         ":443",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	log.Println("HTTPS server started")
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

////////////////////////////////////////////////////////////
// TUI (Bubble Tea)
////////////////////////////////////////////////////////////

type model struct {
	drives  []DriveStatus
	mode    string
	cursor  int
	input   string
	results []UnlockResult
}

func initialModel() model {
	return model{
		drives: scanDrives(),
		mode:   "status",
	}
}

func (m model) Init() tea.Cmd {
	return tick()
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return "tick"
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case string:
		if msg == "tick" && m.mode == "status" {
			m.drives = scanDrives()
			return m, tick()
		}

	case tea.KeyMsg:
		switch m.mode {

		case "status":
			switch msg.String() {
			case "q":
				return m, tea.Quit

			case "u":
				m.mode = "unlock"
				m.input = ""

			case "b":
				exec.Command("/bin/sh", "/usr/local/sbin/sedunlocksrv/kexec-boot.sh").Run()

			case "r":
				exec.Command("reboot", "-nf").Run()

			case "s":
				exec.Command("poweroff", "-nf").Run()
			}

		case "unlock":
			switch msg.Type {

			case tea.KeyEnter:
				m.results = unlockAll(m.input)
				m.mode = "result"

			case tea.KeyEsc:
				m.mode = "status"

			case tea.KeyBackspace:
				if len(m.input) > 0 {
					m.input = m.input[:len(m.input)-1]
				}

			default:
				m.input += msg.String()
			}

		case "result":
			if msg.String() == "enter" || msg.String() == "esc" {
				m.mode = "status"
			}
		}
	}

	return m, nil
}

func (m model) View() string {
	var b strings.Builder

	b.WriteString("SED UNLOCK SERVICE\n\n")

	switch m.mode {

	case "status":
		for _, d := range m.drives {
			status := "✅ UNLOCKED"
			if d.Locked {
				status = "❌ LOCKED"
			}
			b.WriteString(fmt.Sprintf("  %-15s %s\n", d.Device, status))
		}

		b.WriteString("\n[U] Unlock  [B] Boot  [R] Reboot  [S] Shutdown  [Q] Quit\n")

	case "unlock":
		b.WriteString("Enter Password: ")
		b.WriteString(strings.Repeat("*", len(m.input)))
		b.WriteString("\n\n[Enter] Submit  [Esc] Cancel\n")

	case "result":
		b.WriteString("Unlock Results:\n\n")
		for _, r := range m.results {
			status := "❌ FAILED"
			if r.Success {
				status = "✅ SUCCESS"
			}
			b.WriteString(fmt.Sprintf("  %-15s %s\n", r.Device, status))
		}
		b.WriteString("\n[Enter] Continue\n")
	}

	return b.String()
}

////////////////////////////////////////////////////////////
// MAIN
////////////////////////////////////////////////////////////

func main() {
	go startHTTP()

	p := tea.NewProgram(initialModel(), tea.WithAltScreen())

	if err := p.Start(); err != nil {
		log.Fatal(err)
	}
}
