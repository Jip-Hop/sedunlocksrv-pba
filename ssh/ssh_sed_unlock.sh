#!/bin/sh

export TMOUT=300
API="https://127.0.0.1:443"
CURL_OPTS="-s -k --fail"

retry() {
    for i in 1 2 3; do
        curl "$@" && return 0
        sleep 1
    done
    return 1
}

while true; do
    clear
    echo "🔑 SED UNLOCK SERVICE - REMOTE SSH"
    echo "Status refreshes every 30s | Auto-logout: 5m"
    echo "API: $API"

    STATUS_JSON=$(retry $CURL_OPTS "$API/status" 2>/dev/null) || {
        echo "❌ Cannot reach API"
        sleep 2
        continue
    }

    IS_UNLOCKED=$(echo "$STATUS_JSON" | grep -q '"locked":false' && echo "true" || echo "false")

    printf "\n--- Drive Status ---\n"
    echo "$STATUS_JSON" | grep -oE '"device":"[^"]+"|"locked":[^,]+' | \
    awk -F: '
        /device/ { gsub(/"/, "", $2); dev=$2 }
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

    if read -t 30 -r choice; then
        [ -z "$choice" ] && continue

        case $(echo "$choice" | tr '[:lower:]' '[:upper:]') in
            U)
                echo -n "Enter SED Password: "
                stty -echo; read -r pass; stty sane; echo ""

                RESP=$(printf '{"password":"%s"}' "$pass" | \
                    curl $CURL_OPTS -X POST -H "Content-Type: application/json" \
                    --data-binary @- "$API/unlock")

                unset pass

                echo "$RESP" | grep -oE '"device":"[^"]+"|"success":[^,]+' | \
                awk -F: '
                    /device/ { gsub(/"/, "", $2); dev=$2 }
                    /success/ {
                        status = ($2 == "true") ? "✅ SUCCESS" : "❌ FAILED"
                        printf "  %s: %s\n", dev, status
                    }
                '

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
    fi
done
