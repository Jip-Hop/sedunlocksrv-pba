// util.go — Utility functions for SED Unlock Server
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// limitBody caps the request body size for JSON endpoints.
// 64 KB is generous for any JSON payload this server accepts.
const maxJSONBodyBytes int64 = 64 * 1024

func limitBody(r *http.Request) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxJSONBodyBytes)
}

// validDevicePath is a strict allowlist for /dev/ device paths.
var validDevicePath = regexp.MustCompile(`^/dev/[a-zA-Z0-9_-]+$`)

func validateDevicePath(device string) bool {
	cleaned := filepath.Clean(device)
	return cleaned == device && validDevicePath.MatchString(cleaned)
}

// checkOrigin validates the Origin header on state-changing requests.
// Returns true (and writes an error response) if the request should be rejected.
func checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header — same-origin request or non-browser client; allow.
		return false
	}
	// Allow requests whose Origin matches the Host the server is listening on.
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	// Strip port from both for comparison.
	hostNoPort := strings.Split(host, ":")[0]
	// Origin is a full URL like "https://192.168.1.10:443"
	originTrimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	originHost := strings.Split(originTrimmed, ":")[0]
	originHost = strings.TrimSuffix(originHost, "/")
	if strings.EqualFold(originHost, hostNoPort) || strings.EqualFold(originHost, "localhost") || originHost == "127.0.0.1" {
		return false
	}
	jsonResponse(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
	return true
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
	appendBootDebugAtLevel(debug, debugNormal, format, args...)
}

func appendBootDebugVerbose(debug *[]string, format string, args ...interface{}) {
	appendBootDebugAtLevel(debug, debugVerbose, format, args...)
}

func appendBootDebugAtLevel(debug *[]string, level int, format string, args ...interface{}) {
	// This helper keeps the in-memory debug slice and boot-status stream in sync
	// while honoring the global build-time debug level.
	if !shouldEmitDebug(level) {
		return
	}
	line := fmt.Sprintf(format, args...)
	*debug = append(*debug, line)
	if level == debugVerbose {
		recordBootLaunchDebugVerbose(line)
		return
	}
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

// splitOnCRorLF is a bufio.SplitFunc that splits on \r or \n.
// sedutil-cli writes progress using \r (carriage return) to overwrite
// the same line, so the default bufio.ScanLines (which splits on \n)
// would never see intermediate progress updates.
func splitOnCRorLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseProgressPct extracts the integer percentage from a sedutil-cli progress line.
// Format: "61334 of 52428800 0% blk=61334" → 0, true
func parseProgressPct(line string) (int, bool) {
	idx := strings.Index(line, "% ")
	if idx < 1 {
		return 0, false
	}
	// Walk backwards from idx to find start of the number
	start := idx - 1
	for start >= 0 && line[start] >= '0' && line[start] <= '9' {
		start--
	}
	start++
	if start >= idx {
		return 0, false
	}
	pct, err := strconv.Atoi(line[start:idx])
	if err != nil {
		return 0, false
	}
	return pct, true
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

		// Use stdbuf -o0 to disable stdout buffering so we can read
		// sedutil-cli's \r-delimited progress lines in real time.
		cmd := exec.CommandContext(ctx, "stdbuf", "-o0", "sedutil-cli", "--loadpbaimage", password, tmpPath, device)

		// Pipe stdout for progress; capture stderr for LOG messages
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			recordFlashLine("ERROR: failed to create stdout pipe: " + err.Error())
			flashMu.Lock()
			flashState.Error = "failed to create stdout pipe"
			flashMu.Unlock()
			return
		}
		var stderrBuf strings.Builder
		cmd.Stderr = &stderrBuf

		if err := cmd.Start(); err != nil {
			recordFlashLine("ERROR: failed to start sedutil-cli: " + err.Error())
			flashMu.Lock()
			flashState.Error = "failed to start: " + err.Error()
			flashMu.Unlock()
			return
		}

		// Read stdout splitting on \r or \n to capture progress lines
		go func() {
			scanner := bufio.NewScanner(stdoutPipe)
			scanner.Split(splitOnCRorLF)
			lastLoggedPct := -1
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				// Filter progress lines: only log every ~4%
				// Progress format: "61334 of 52428800 0% blk=61334"
				if pct, ok := parseProgressPct(line); ok {
					if pct == 100 || pct-lastLoggedPct >= 4 {
						lastLoggedPct = pct
						recordFlashLine(line)
					}
					continue
				}
				recordFlashLine(line)
			}
		}()

		err = cmd.Wait()

		// Capture any stderr LOG messages
		for _, line := range strings.Split(strings.TrimSpace(stderrBuf.String()), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				recordFlashLine(line)
			}
		}

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
