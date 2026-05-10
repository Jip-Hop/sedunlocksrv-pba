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

// jsonResponse writes a JSON-encoded value with the given HTTP status code.
func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requireMethod rejects the request if r.Method does not match method.
// Returns true (and writes an error response) when the request should be rejected.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		jsonResponse(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return true
	}
	return false
}

// 64 KB is generous for any JSON payload this server accepts.
const maxJSONBodyBytes int64 = 64 * 1024

// limitBody caps the request body to maxJSONBodyBytes to prevent oversized payloads.
func limitBody(r *http.Request) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxJSONBodyBytes)
}

// validDevicePath is a strict allowlist for /dev/ device paths.
var validDevicePath = regexp.MustCompile(`^/dev/[a-zA-Z0-9_-]+$`)

// validateDevicePath returns true if device is a clean /dev/ path matching the
// strict allowlist (alphanumerics, hyphens, and underscores only).
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
	if strings.EqualFold(originHost, hostNoPort) {
		return false
	}
	jsonResponse(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
	return true
}

// ============================================================
// SEDUTIL HELPERS
// ============================================================

// runSedutil executes sedutil-cli with the given arguments and a timeout.
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

// runCommandTimeout runs an arbitrary command with a timeout, returning any error.
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

// queryDrive runs sedutil-cli --query against a device and returns the raw output.
func queryDrive(dev string) (string, error) {
	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer qcancel()
	query, err := exec.CommandContext(qctx, "sedutil-cli", "--query", dev).CombinedOutput()
	return string(query), err
}

// queryField extracts a named field value from sedutil-cli --query output.
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

// firstExistingPath returns the first path in the list that exists on disk,
// or an empty string if none exist.
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

// haveRuntimeCommand returns true if cmd is found on the system PATH.
func haveRuntimeCommand(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// availableRAMBytes returns the MemAvailable value from /proc/meminfo in bytes.
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

// appendBootDebug adds a normal-level debug line to the in-memory boot log.
func appendBootDebug(debug *[]string, format string, args ...interface{}) {
	appendBootDebugAtLevel(debug, debugNormal, format, args...)
}

// appendBootDebugVerbose adds a verbose-level debug line to the in-memory boot log.
func appendBootDebugVerbose(debug *[]string, format string, args ...interface{}) {
	appendBootDebugAtLevel(debug, debugVerbose, format, args...)
}

// appendBootDebugAtLevel adds a debug line at the specified verbosity level,
// keeping the in-memory slice and the live boot-status stream in sync.
func appendBootDebugAtLevel(debug *[]string, level int, format string, args ...interface{}) {
	// This helper keeps the in-memory debug slice and boot-status stream in sync
	// while honoring the global build-time debug level.
	if !shouldEmitDebug(level) {
		return
	}
	line := fmt.Sprintf(format, args...)
	*debug = append(*debug, line)
	if level == debugVerbose {
		recordBootDebugVerbose(line)
		return
	}
	recordBootDebug(line)
}

// ============================================================
// COMMAND EXECUTION
// ============================================================

func redactedSedutilCommand(args []string) string {
	redacted := append([]string{"sedutil-cli"}, args...)
	for i, arg := range redacted {
		switch arg {
		case "--reverttper", "--yesIreallywanttoERASEALLmydatausingthePSID":
			if i+1 < len(redacted) {
				redacted[i+1] = "<redacted>"
			}
		}
	}
	return strings.Join(redacted, " ")
}

// runExpertCommand executes a sedutil-cli command and writes the result as JSON.
func runExpertCommand(w http.ResponseWriter, args ...string) {
	out, err := runSedutil(45*time.Second, args...)
	resp := map[string]string{
		"command": redactedSedutilCommand(args),
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

// recordFlashLine appends a progress line to the in-flight PBA flash status.
func recordFlashLine(line string) {
	flashMu.Lock()
	defer flashMu.Unlock()
	if flashState.InProgress {
		flashState.Lines = append(flashState.Lines, line)
	}
}

// runExpertPBAFlashImagePath invokes sedutil-cli --loadpbaimage against an
// already-written temporary image file, streaming progress into the flash log.
func runExpertPBAFlashImagePath(w http.ResponseWriter, password, imagePath, device string, validation []string) {
	// Initialize flash state
	flashMu.Lock()
	if flashState.InProgress {
		flashMu.Unlock()
		os.Remove(imagePath)
		jsonResponse(w, http.StatusConflict, map[string]string{"error": "a flash operation is already in progress"})
		return
	}
	flashState = FlashStatus{InProgress: true, Lines: []string{}}
	flashMu.Unlock()

	// Return immediately — the flash runs in the background
	jsonResponse(w, http.StatusOK, map[string]string{"status": "flash started"})

	go func() {
		defer os.Remove(imagePath)
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
		cmd := exec.CommandContext(ctx, "stdbuf", "-o0", "sedutil-cli", "--loadpbaimage", password, imagePath, device)

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

// newSystemActionHandler returns an HTTP handler that responds with a status
// message and then executes a system command (e.g. reboot, poweroff) after
// a brief delay so the response can be flushed.
//
// Intentional security tradeoff: reboot and poweroff must remain available even
// before any drive is unlocked, when no web session token can exist yet. Any
// network client that can reach the PBA can request these POST endpoints. Do not
// add session-token auth here unless another pre-unlock recovery path is kept.
func newSystemActionHandler(label string, args ...string) http.HandlerFunc {
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
