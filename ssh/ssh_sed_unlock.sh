#!/bin/busybox ash
#
# SSH interface for the SED Unlock Service.
#
# This script is used as the forced command for SSH logins, so users land in
# this menu instead of a shell. It talks to the local sedunlocksrv HTTPS API.

export TMOUT=300

SSH_CURL_INSECURE="true"
[ -f /etc/sedunlocksrv.conf ] && . /etc/sedunlocksrv.conf

API="https://localhost:443"
case "${SSH_CURL_INSECURE}" in
    true)
        CURL="curl -s -k"
        ;;
    *)
        CURL="curl -s"
        ;;
esac

AUTH_TOKEN=""
STATUS_NOTICE=""

CLR_RESET="$(printf '\033[0m')"
CLR_BLUE="$(printf '\033[38;5;32m')"
CLR_PURPLE="$(printf '\033[38;5;91m')"
CLR_ORANGE="$(printf '\033[38;5;208m')"
CLR_DIM="$(printf '\033[38;5;245m')"

have_command() {
    command -v "$1" >/dev/null 2>&1
}

status_fallback_json() {
    printf '%s\n' '{"drives":[],"interfaces":[]}'
}

fetch_status_json() {
    STATUS_NOTICE=""

    if ! have_command curl; then
        STATUS_NOTICE="Status unavailable: curl is not installed in this build."
        status_fallback_json
        return 1
    fi

    if ! have_command jq; then
        STATUS_NOTICE="Status unavailable: jq is not installed in this build."
        status_fallback_json
        return 1
    fi

    local response
    response=$($CURL "$API/status" 2>/dev/null) || response=""
    if [ -z "${response}" ] || ! echo "${response}" | jq -e . >/dev/null 2>&1; then
        STATUS_NOTICE="Status unavailable: unable to reach the local unlock service."
        status_fallback_json
        return 1
    fi

    printf '%s\n' "${response}"
    return 0
}

print_drives() {
    local count
    count=$(echo "$1" | jq -r '.drives | length' 2>/dev/null || echo 0)
    if [ "${count}" -eq 0 ]; then
        echo "  No OPAL drives detected."
        return 0
    fi

    echo "$1" | jq -r '
        .drives[] |
        "  " +
        (if .locked then "[locked] " else "[open]   " end) +
        .device + "  " +
        (if .locked then "LOCKED  " else "UNLOCKED" end) + "  " +
        (if .opal then "OPAL" else "NON-OPAL" end)
    '
}

print_interfaces() {
    local count
    count=$(echo "$1" | jq -r '.interfaces | length' 2>/dev/null || echo 0)
    if [ "${count}" -eq 0 ]; then
        echo "  No network interfaces reported."
        return 0
    fi

    echo "$1" | jq -r '
        .interfaces[] |
        "  " + .name +
        "  " + .state +
        (if .carrier then "  link" else "  no-link" end) +
        (if .mac != "" then "  " + .mac else "" end) +
        (if (.addresses | length) > 0 then "  " + (.addresses | join(", ")) else "" end) +
        (if .loopback then "  loopback" else "" end)
    '
}

has_error() {
    echo "$1" | jq -e '.error' > /dev/null 2>&1
}

get_error() {
    echo "$1" | jq -r '.error // empty'
}

has_results() {
    echo "$1" | jq -e '(.results // []) | length > 0' > /dev/null 2>&1
}

post_with_auth() {
    if [ -n "$AUTH_TOKEN" ]; then
        $CURL -X POST -H "X-Auth-Token: $AUTH_TOKEN" "$1"
    else
        $CURL -X POST "$1"
    fi
}

while true; do
    clear
    echo "${CLR_BLUE}SED UNLOCK (SSH)${CLR_RESET}"
    echo "${CLR_DIM}Auto logout: 5m | Refresh: 10s${CLR_RESET}"
    echo

    STATUS_JSON=$(fetch_status_json)
    if [ -n "${STATUS_NOTICE}" ]; then
        echo "${CLR_DIM}${STATUS_NOTICE}${CLR_RESET}"
        echo
    fi

    print_drives "${STATUS_JSON}"
    echo
    echo "${CLR_DIM}Network Interfaces (use these names for NET_IFACES / EXCLUDE_NETDEV):${CLR_RESET}"
    print_interfaces "${STATUS_JSON}"
    echo

    echo "[U] ${CLR_PURPLE}Unlock${CLR_RESET}  [B] ${CLR_BLUE}Boot${CLR_RESET}  [R] ${CLR_ORANGE}Reboot${CLR_RESET}  [S] ${CLR_BLUE}Shutdown${CLR_RESET}  [Q] Quit"
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

            RESP=$($CURL -X POST \
                -H "Content-Type: application/json" \
                -d "$(jq -n --arg p "$pass" '{password: $p}')" \
                "$API/unlock" 2>/dev/null)

            if [ -z "${RESP}" ]; then
                echo "Service unavailable."
            elif has_error "$RESP"; then
                echo "Error: $(get_error "$RESP")"
            else
                AUTH_TOKEN=$(echo "$RESP" | jq -r '.token // empty')
                if has_results "$RESP"; then
                    echo "$RESP" | jq -r '
                        .results[] |
                        "  " + (if .success then "[ok] " else "[failed] " end) + .device
                    '
                elif [ -n "${AUTH_TOKEN}" ]; then
                    echo "Unlock accepted."
                else
                    echo "No unlockable OPAL drives were reported."
                fi
            fi
            sleep 2
            ;;

        B)
            echo -n "Boot OS? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$(post_with_auth "$API/boot" 2>/dev/null)
                if [ -z "${RESP}" ]; then
                    echo "Service unavailable."
                    sleep 2
                elif has_error "$RESP"; then
                    echo "Error: $(get_error "$RESP")"
                    sleep 2
                else
                    WARN=$(echo "$RESP" | jq -r '.warning // empty')
                    [ -n "$WARN" ] && echo "Warning: $WARN"
                    echo "Booting..."
                    exit 0
                fi
            fi
            ;;

        R)
            echo -n "Reboot? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$(post_with_auth "$API/reboot" 2>/dev/null)
                if [ -z "${RESP}" ]; then
                    echo "Service unavailable."
                    sleep 2
                elif has_error "$RESP"; then
                    echo "Error: $(get_error "$RESP")"
                    sleep 2
                else
                    echo "$(echo "$RESP" | jq -r '.status // "rebooting"')..."
                    exit 0
                fi
            fi
            ;;

        S)
            echo -n "Shutdown? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$(post_with_auth "$API/poweroff" 2>/dev/null)
                if [ -z "${RESP}" ]; then
                    echo "Service unavailable."
                    sleep 2
                elif has_error "$RESP"; then
                    echo "Error: $(get_error "$RESP")"
                    sleep 2
                else
                    echo "$(echo "$RESP" | jq -r '.status // "powering off"')..."
                    exit 0
                fi
            fi
            ;;

        Q)
            exit 0
            ;;
    esac
done
