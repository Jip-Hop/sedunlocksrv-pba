#!/bin/sh
# SSH Wrapper for SED Unlock Service (jq version)

export TMOUT=300

API="https://127.0.0.1:443"
CURL_OPTS="-s -k --fail"

# ---------------------------------------------------------
# Retry helper (handles transient failures)
# ---------------------------------------------------------
retry() {
    for i in 1 2 3; do
        curl "$@" && return 0
        sleep 1
    done
    return 1
}

# ---------------------------------------------------------
# Fetch drive status safely
# ---------------------------------------------------------
get_status() {
    retry $CURL_OPTS "$API/status" 2>/dev/null
}

# ---------------------------------------------------------
# Render drive status using jq
# ---------------------------------------------------------
print_status() {
    echo "$1" | jq -r '
        .drives[] |
        "  \(.device): " +
        (if .locked then "❌ LOCKED" else "✅ UNLOCKED" end)
    '
}

# ---------------------------------------------------------
# Check if ALL drives are unlocked
# ---------------------------------------------------------
all_unlocked() {
    echo "$1" | jq -e '[.drives[].locked] | any | not' >/dev/null 2>&1
}

# ---------------------------------------------------------
# Unlock drives
# ---------------------------------------------------------
unlock_drives() {
    echo -n "Enter SED Password: "
    stty -echo
    read -r pass
    stty sane
    echo ""

    RESP=$(printf '{"password":"%s"}' "$pass" | \
        curl $CURL_OPTS -X POST \
        -H "Content-Type: application/json" \
        --data-binary @- \
        "$API/unlock")

    unset pass

    echo ""
    echo "--- Unlock Results ---"

    echo "$RESP" | jq -r '
        .results[] |
        "  \(.device): " +
        (if .success then "✅ SUCCESS" else "❌ FAILED" end)
    '

    sleep 2
}

# ---------------------------------------------------------
# MAIN LOOP
# ---------------------------------------------------------
while true; do
    clear
    echo "🔑 SED UNLOCK SERVICE - REMOTE SSH"
    echo "Status refreshes every 30s | Auto-logout: 5m"
    echo "API: $API"

    STATUS_JSON=$(get_status)

    if [ -z "$STATUS_JSON" ]; then
        echo "❌ Cannot reach API"
        sleep 2
        continue
    fi

    echo ""
    echo "--- Drive Status ---"
    print_status "$STATUS_JSON"

    echo ""

    if all_unlocked "$STATUS_JSON"; then
        IS_UNLOCKED=true
    else
        IS_UNLOCKED=false
    fi

    MENU="[U] Unlock Drive(s)  "
    [ "$IS_UNLOCKED" = true ] && MENU="${MENU}[B] Boot OS  "
    MENU="${MENU}[R] Reboot  [S] Shutdown  [Q] Quit"

    echo "$MENU"
    echo -n "Selection (Auto-refresh in 30s): "

    if read -t 30 -r choice; then
        [ -z "$choice" ] && continue

        case $(echo "$choice" | tr '[:lower:]' '[:upper:]') in
            U)
                unlock_drives
                ;;
            B)
                if [ "$IS_UNLOCKED" = true ]; then
                    echo -n "Confirm Boot? (y/n): "
                    read -r conf
                    if [ "$conf" = "y" ]; then
                        curl $CURL_OPTS -X POST "$API/boot"
                        exit 0
                    fi
                fi
                ;;
            R)
                echo -n "Confirm Reboot? (y/n): "
                read -r conf
                if [ "$conf" = "y" ]; then
                    curl $CURL_OPTS -X POST "$API/reboot"
                    exit 0
                fi
                ;;
            S)
                echo -n "Confirm Shutdown? (y/n): "
                read -r conf
                if [ "$conf" = "y" ]; then
                    poweroff -nf
                fi
                ;;
            Q)
                exit 0
                ;;
            *)
                echo "Invalid selection."
                sleep 1
                ;;
        esac
    fi
done
