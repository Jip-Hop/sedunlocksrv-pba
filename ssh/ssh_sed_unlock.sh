#!/bin/busybox ash
# ssh_sed_unlock.sh — SSH interface for the SED Unlock Service
#
# This script is the command that runs when an authorized user connects via SSH.
# It is locked in as the forced command in authorized_keys, meaning SSH users
# cannot run arbitrary shell commands — they get only this menu.
#
# It communicates with the Go server (sedunlocksrv) running on localhost:443
# via its JSON API using curl. All three interfaces (web UI, SSH, console TUI)
# share the same API, so rate limiting and drive state are consistent across them.
#
# jq is used for all JSON parsing. It is included as a build dependency in
# build.sh (jq.tcz extension) and is always available in the PBA environment.
#
# The session auto-logs out after 5 minutes of total inactivity (TMOUT) and
# the menu refreshes its drive status display every 10 seconds while waiting
# for input (read -t 10 timeout).
#
# Typical SSH connection:
#   ssh -p 2222 tc@<server-ip>
# (Port 2222 is used to avoid conflicts with any SSH service on the booted OS.)

# Auto-logout after 5 minutes of shell inactivity.
export TMOUT=300

# Base URL for the Go server's JSON API (always localhost).
# -s suppresses curl progress output; -k skips TLS cert verification since
# the server uses a self-signed certificate generated at build time.
API="https://localhost:443"
CURL="curl -s -k"

# ======================================================
# HELPERS
# ======================================================

# print_drives — display the current state of all drives.
# Reads from a JSON string passed as $1 (the output of GET /status).
# Produces one line per drive, e.g.:
#   ✅ /dev/sda  UNLOCKED  🔐 OPAL
#   ❌ /dev/sdb  LOCKED    🔐 OPAL
#   ⚠  /dev/sdc  LOCKED    ⚠ NON-OPAL
print_drives() {
    echo "$1" | jq -r '
        .drives[] |
        if .locked then "  ❌ " else "  ✅ " end +
        .device + "  " +
        if .locked then "LOCKED  " else "UNLOCKED" end + "  " +
        if .opal then "🔐 OPAL" else "⚠  NON-OPAL" end
    '
}

# has_error — returns 0 (true) if the JSON response contains an "error" key.
# Uses jq -e which sets a non-zero exit code when the result is null/false,
# so this correctly distinguishes a missing key from one with a value.
has_error() {
    echo "$1" | jq -e '.error' > /dev/null 2>&1
}

# get_error — extracts the error message string from a JSON response.
# Prints nothing if the key is absent.
get_error() {
    echo "$1" | jq -r '.error // empty'
}

# ======================================================
# MAIN LOOP
# ======================================================

while true; do
    clear
    echo "🔑 SED UNLOCK (SSH)"
    echo "Auto logout: 5m | Refresh: 10s"
    echo

    # --- Drive Status Display ---
    # Fetch current drive state from GET /status and display it.
    # Response format: {"drives":[{"device":"/dev/sda","locked":false,"opal":true},...]}
    STATUS_JSON=$($CURL "$API/status")
    print_drives "$STATUS_JSON"
    echo

    # --- Menu ---
    echo "[U] Unlock  [B] Boot  [R] Reboot  [S] Shutdown  [Q] Quit"
    echo -n "Choice: "

    # Wait up to 10 seconds for input. If no key is pressed, loop back to
    # the top and refresh the drive status display.
    if ! read -t 10 choice; then
        continue
    fi

    # Normalize input to uppercase so both "u" and "U" are accepted.
    case $(echo "$choice" | tr '[:lower:]' '[:upper:]') in

        U) # Unlock — POST /unlock with the password
            echo -n "Password: "
            stty -echo    # disable echo so the password isn't displayed
            read -r pass
            stty sane     # restore normal terminal settings
            echo

            # Use jq to build the request body so that passwords containing
            # quotes, backslashes, or other JSON-special characters are safely
            # escaped. Shell string interpolation into raw JSON would break on
            # such characters and produce a malformed request body.
            RESP=$($CURL -X POST \
                -H "Content-Type: application/json" \
                -d "$(jq -n --arg p "$pass" '{password: $p}')" \
                "$API/unlock")

            if has_error "$RESP"; then
                # Display the specific error from the server (e.g. "maximum
                # failed attempts reached", "password cannot be blank").
                echo "❌ $(get_error "$RESP")"
            else
                # Show per-drive results: one line per drive indicating success
                # or failure. Format: ✅ /dev/sda  or  ❌ /dev/sda
                echo "$RESP" | jq -r '
                    .results[] |
                    if .success then "  ✅ " else "  ❌ " end + .device
                '
            fi
            sleep 2
            ;;

        B) # Boot — POST /boot to load the Proxmox kernel via kexec.
            # Unlike Reboot, this is a warm kernel switch that keeps drives unlocked.
            echo -n "Boot OS? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$($CURL -X POST "$API/boot")
                if has_error "$RESP"; then
                    echo "❌ $(get_error "$RESP")"
                    sleep 2
                else
                    # Display a warning if any drives were still locked at boot time.
                    # The Go server includes this in the response when fullyUnlocked=false.
                    WARN=$(echo "$RESP" | jq -r '.warning // empty')
                    [ -n "$WARN" ] && echo "⚠  $WARN"
                    echo "🚀 Booting..."
                    exit 0
                fi
            fi
            ;;

        R) # Reboot — POST /reboot for a cold reboot.
            # Note: unlike Boot (kexec), this re-locks the drives and brings
            # the PBA up again from scratch.
            echo -n "Reboot? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$($CURL -X POST "$API/reboot")
                echo "$(echo "$RESP" | jq -r '.status // "rebooting"')..."
                exit 0
            fi
            ;;

        S) # Shutdown — POST /poweroff to cut power.
            echo -n "Shutdown? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$($CURL -X POST "$API/poweroff")
                echo "$(echo "$RESP" | jq -r '.status // "powering off"')..."
                exit 0
            fi
            ;;

        Q) # Quit — exit the SSH session cleanly.
            exit 0
            ;;

    esac
done
