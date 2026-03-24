#!/bin/sh
# Minimal SSH UI for SED Unlock Service

export TMOUT=300

API="https://localhost:443"
CURL="curl -s -k"

while true; do
    clear
    echo "🔑 SED UNLOCK (SSH)"
    echo "Auto logout: 5m | Refresh: 10s"
    echo

    STATUS_JSON=$($CURL "$API/status")

    echo "$STATUS_JSON" | tr ',' '\n' | sed 's/[{}"]//g' | awk -F: '
        /device/ { dev=$2 }
        /locked/ {
            if ($2=="false")
                printf "  ✅ %s\n", dev;
            else
                printf "  ❌ %s\n", dev;
        }
    '

    echo
    echo "[U] Unlock  [B] Boot  [R] Reboot  [S] Shutdown  [Q] Quit"
    echo -n "Choice: "

    if ! read -t 10 choice; then
        continue
    fi

    case $(echo "$choice" | tr '[:lower:]' '[:upper:]') in

        U)
            echo -n "Password: "
            stty -echo
            read -r pass
            stty sane
            echo

            RESP=$($CURL -X POST -d "{\"password\":\"$pass\"}" "$API/unlock")

            echo "$RESP" | grep -q error && echo "❌ Failed"
            sleep 2
            ;;

        B)
            echo -n "Boot OS? (y/n): "
            read -r c
            [ "$c" = "y" ] && $CURL -X POST "$API/boot" && exit 0
            ;;

        R)
            echo -n "Reboot? (y/n): "
            read -r c
            [ "$c" = "y" ] && $CURL -X POST "$API/reboot" && exit 0
            ;;

        S)
            echo -n "Shutdown? (y/n): "
            read -r c
            [ "$c" = "y" ] && $CURL -X POST "$API/poweroff" && exit 0
            ;;

        Q)
            exit 0
            ;;

    esac
done
