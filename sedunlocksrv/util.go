// util.go — Utility functions for SED Unlock Server
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ============================================================
// HTTP HELPERS
// ============================================================

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

// ============================================================
// SEDUTIL HELPERS
// ============================================================

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

func runCommandTimeout(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out", name)
	}
	if err != nil {
		extra := strings.TrimSpace(string(out))
		if extra != "" {
			return fmt.Errorf("%s failed: %v (%s)", name, err, extra)
		}
		return fmt.Errorf("%s failed: %v", name, err)
	}
	return nil
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

// ============================================================
// FILE SYSTEM HELPERS
// ============================================================

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

func haveRuntimeCommand(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func availableRAMBytes() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
}

// ============================================================
// BOOT DEBUG HELPERS
// ============================================================

func appendBootDebug(debug *[]string, format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	*debug = append(*debug, line)
	recordBootLaunchDebug(line)
}

// ============================================================
// COMMAND EXECUTION
// ============================================================

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

func recordFlashLine(line string) {
	flashMu.Lock()
	defer flashMu.Unlock()
	if flashState.InProgress {
		flashState.Lines = append(flashState.Lines, line)
	}
}

func runExpertPBAFlashBytes(w http.ResponseWriter, password string, imageData []byte, device string, validation []string) {
	// Write image data to temporary file for sedutil to read
	tmp, err := os.CreateTemp("", "sedunlocksrv-pba-*.img")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"error":      "failed to prepare temporary image file",
			"validation": validation,
		})
		return
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(imageData); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"error":      "failed to write image to temporary file",
			"validation": validation,
		})
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"error":      "failed to finalize temporary image file",
			"validation": validation,
		})
		return
	}

	// Initialize flash state
	flashMu.Lock()
	if flashState.InProgress {
		flashMu.Unlock()
		os.Remove(tmpPath)
		jsonResponse(w, http.StatusConflict, map[string]string{"error": "a flash operation is already in progress"})
		return
	}
	flashState = FlashStatus{InProgress: true, Lines: []string{}}
	flashMu.Unlock()

	// Return immediately — the flash runs in the background
	jsonResponse(w, http.StatusOK, map[string]string{"status": "flash started"})

	go func() {
		defer os.Remove(tmpPath)
		defer func() {
			flashMu.Lock()
			flashState.InProgress = false
			flashState.Done = true
			flashMu.Unlock()
		}()

		for _, v := range validation {
			recordFlashLine("preflight: " + v)
		}

		// Pause to allow NVMe controller to fully reset state after preflight queries.
		recordFlashLine("Waiting for NVMe controller settle...")
		time.Sleep(1 * time.Second)

		recordFlashLine(fmt.Sprintf("Running: sedutil-cli --loadpbaimage <password> <image> %s", device))

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sedutil-cli", "--loadpbaimage", password, tmpPath, device)

		// Merge stdout and stderr via a pipe so we can read line-by-line
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			flashMu.Lock()
			flashState.Error = "failed to create stdout pipe: " + err.Error()
			flashMu.Unlock()
			return
		}
		cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

		if err := cmd.Start(); err != nil {
			flashMu.Lock()
			flashState.Error = "failed to start sedutil-cli: " + err.Error()
			flashMu.Unlock()
			return
		}

		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				recordFlashLine(line)
			}
		}
		// Also capture any remaining bytes if scanner stopped early
		if remaining, err := io.ReadAll(stdoutPipe); err == nil && len(remaining) > 0 {
			for _, line := range strings.Split(strings.TrimSpace(string(remaining)), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					recordFlashLine(line)
				}
			}
		}

		err = cmd.Wait()
		flashMu.Lock()
		if ctx.Err() == context.DeadlineExceeded {
			flashState.Error = "sedutil-cli timed out"
			flashState.Lines = append(flashState.Lines, "ERROR: sedutil-cli timed out after 2 minutes")
		} else if err != nil {
			flashState.Error = "sedutil-cli failed: " + err.Error()
			flashState.Lines = append(flashState.Lines, "ERROR: "+err.Error())
		} else {
			flashState.Success = true
			flashState.Lines = append(flashState.Lines, "PBA image flashed successfully.")
		}
		flashMu.Unlock()
	}()
}

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
