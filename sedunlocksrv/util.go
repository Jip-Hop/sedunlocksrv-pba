// util.go — Utility functions for SED Unlock Server
package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(imageData); err != nil {
		tmp.Close()
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"error":      "failed to write image to temporary file",
			"validation": validation,
		})
		return
	}
	if err := tmp.Close(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"error":      "failed to finalize temporary image file",
			"validation": validation,
		})
		return
	}

	out, err := runSedutil(2*time.Minute, "-vvv", "--loadpbaimage", password, tmpPath, device)
	resp := map[string]string{
		"command": "sedutil-cli -vvv --loadpbaimage <password> <uploaded-image> " + device,
		"output":  strings.TrimSpace(out),
	}
	respAny := map[string]interface{}{
		"command":    resp["command"],
		"output":     resp["output"],
		"validation": validation,
	}
	if err != nil {
		respAny["error"] = err.Error()
		jsonResponse(w, http.StatusBadRequest, respAny)
		return
	}
	respAny["status"] = "ok"
	jsonResponse(w, http.StatusOK, respAny)
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
