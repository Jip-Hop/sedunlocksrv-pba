#!/bin/sh
# SSH Wrapper for SED Unlock Service - Conditional Menu

API="https://localhost:443"
CURL_OPTS="-s -k"

while true; do
    clear
    echo "🔑 SED UNLOCK SERVICE - REMOTE SSH"
    
    # 1. Fetch status and check if ANY drive is unlocked
    STATUS_JSON=$(curl $CURL_OPTS "$API/status")
    
    # Simple check for unlocked state in JSON
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
    # 2. Conditional Menu Display
    MENU="[U] Unlock Drive(s)  "
    if [ "$IS_UNLOCKED" = "true" ]; then
        MENU="${MENU}[B] Boot OS (kexec)  "
    fi
    MENU="${MENU}[R] Reboot  [S] Shutdown  [Q] Quit"
    
    echo "$MENU"
    echo -n "Selection: "
    read -r choice

    case $(echo "$choice" | tr '[:lower:]' '[:upper:]') in
        U)
            echo -n "Enter SED Password: "
            stty -echo; read -r pass; stty sane
            echo ""
            curl $CURL_OPTS -X POST -d "{\"password\":\"$pass\"}" "$API/unlock"
            sleep 2
            ;;
        B)
            if [ "$IS_UNLOCKED" = "true" ]; then
                echo -n "Confirm Boot to Proxmox? (y/n): "
                read -r conf
                if [ "$conf" = "y" ]; then
                    echo "🚀 Jumping to OS..."
                    curl $CURL_OPTS -X POST "$API/boot"
                    exit 0
                fi
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
    esac
done
