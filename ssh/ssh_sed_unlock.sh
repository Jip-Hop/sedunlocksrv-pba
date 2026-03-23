#!/bin/sh
# SSH Wrapper for SED Unlock Service - 30s Refresh & 5m Timeout

# Global Timeout (5 minutes of total inactivity closes SSH)
export TMOUT=300
API="https://localhost:443"
CURL_OPTS="-s -k" # -k bypasses certificate name mismatch on localhost

while true; do
    clear
    echo "🔑 SED UNLOCK SERVICE - REMOTE SSH"
    echo "Status refreshes every 30s | Auto-logout: 5m"
    
    # 1. Fetch status and check unlock state
    STATUS_JSON=$(curl $CURL_OPTS "$API/status")
    IS_UNLOCKED=$(echo "$STATUS_JSON" | grep -q '"locked":false' && echo "true" || echo "false")

    printf "\n--- Drive Status ---\n"
    echo "$STATUS_JSON" | tr ',' '\n' | sed 's/[{}"]//g' | awk -F: '
        /device/ { dev=$2 }
        /locked/ { 
            status = ($2 == "false") ? "✅ UNLOCKED" : "❌ LOCKED"
            printf "  %s: %s\n", dev, status
        }
    '

    echo ""
    MENU="[U] Unlock Drive(s)  "
    [ "$IS_UNLOCKED" = "true" ] && MENU="${MENU}[B] Boot OS  "
    MENU="${MENU}[R] Reboot  [S] Shutdown  [Q] Quit"
    
    echo "$MENU"
    echo -n "Selection (Auto-refresh in 30s): "
    
    # -t 30 waits 30 seconds for input. If it times out, it returns non-zero.
    if read -t 30 -r choice; then
        case $(echo "$choice" | tr '[:lower:]' '[:upper:]') in
            U)
                echo -n "Enter SED Password: "
                stty -echo; read -r pass; stty sane; echo ""
                curl $CURL_OPTS -X POST -d "{\"password\":\"$pass\"}" "$API/unlock"
                sleep 2
                ;;
            B)
                if [ "$IS_UNLOCKED" = "true" ]; then
                    echo -n "Confirm Boot? (y/n): "
                    read -r conf
                    [ "$conf" = "y" ] && curl $CURL_OPTS -X POST "$API/boot" && exit 0
                fi
                ;;
            R)
                echo -n "Confirm Reboot? (y/n): "
                read -r conf
                [ "$conf" = "y" ] && curl $CURL_OPTS -X POST "$API/reboot" && exit 0
                ;;
            S)
                echo -n "Confirm Shutdown? (y/n): "
                read -r conf
                [ "$conf" = "y" ] && poweroff -nf
                ;;
            Q) exit 0 ;;
            *) echo "Invalid selection."; sleep 1 ;;
        esac
    else
        # read -t 30 timed out, loop restarts and refreshes status automatically
        continue
    fi
done
