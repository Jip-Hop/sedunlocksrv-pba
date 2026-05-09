#!/bin/busybox ash
#
# SSH interface for the SED Unlock Service.
#
# This script is used as the forced command for SSH logins, so users land in
# this menu instead of a shell. It talks to the local sedunlocksrv HTTPS API.

export TMOUT=300

TLS_SERVER_NAME="localhost"
SSH_CURL_INSECURE="auto"
[ -f /etc/sedunlocksrv.conf ] && . /etc/sedunlocksrv.conf

TLS_CERT_FILE="/usr/local/sbin/sedunlocksrv/server.crt"
TLS_CA_CERT_FILE="/usr/local/etc/ssl/sedunlocksrv-ca.pem"

AUTH_TOKEN=""
STATUS_NOTICE=""
STATUS_JSON=""
DIAG_NOTICE=""
DIAG_JSON=""
BOOT_STATUS_JSON=""
KERNEL_INDEX_MAP=""

CLR_RESET="$(printf '\033[0m')"
CLR_BLUE="$(printf '\033[38;5;32m')"
CLR_PURPLE="$(printf '\033[38;5;91m')"
CLR_ORANGE="$(printf '\033[38;5;208m')"
CLR_DIM="$(printf '\033[38;5;245m')"

# have_command CMD — returns 0 if CMD is on the PATH.
have_command() {
    command -v "$1" >/dev/null 2>&1
}

# status_fallback_json — stores a minimal empty-status JSON object.
# Used when curl or jq is missing or the API is unreachable.
status_fallback_json() {
    STATUS_JSON='{"drives":[],"interfaces":[]}'
}

# use_local_cert_pin returns 0 when the SSH helper should trust the bundled
# cert file directly. This is the clean path for the generated self-signed cert
# because make-cert.sh includes localhost-style SANs but the cert is not in a
# global CA store.
use_local_cert_pin() {
    [ "${SSH_CURL_INSECURE}" = "auto" ] || return 1
    [ -f "${TLS_CERT_FILE}" ] || return 1
    case "${TLS_SERVER_NAME}" in
        localhost|127.0.0.1|sedunlocksrv) return 0 ;;
    esac
    return 1
}

# use_custom_ca_bundle returns 0 when the builder supplied an explicit CA file
# for internal/private PKI. This lets curl verify the custom cert chain without
# modifying Tiny Core's global CA store.
use_custom_ca_bundle() {
    [ "${SSH_CURL_INSECURE}" = "true" ] && return 1
    [ -f "${TLS_CA_CERT_FILE}" ]
}

# api_request METHOD PATH [curl args...] — connects to 127.0.0.1:443 while
# still presenting/verifying TLS_SERVER_NAME. curl --resolve gives us both:
# the TCP connection stays on loopback, but SNI and hostname verification use
# the configured certificate name instead of "localhost".
api_request() {
    local method="$1" path="$2"
    shift 2

    if [ "${SSH_CURL_INSECURE}" = "true" ]; then
        curl -s -k -X "${method}" \
            --resolve "${TLS_SERVER_NAME}:443:127.0.0.1" \
            "$@" \
            "https://${TLS_SERVER_NAME}:443${path}" 2>/dev/null
        return $?
    fi

    if use_custom_ca_bundle; then
        curl -s --cacert "${TLS_CA_CERT_FILE}" -X "${method}" \
            --resolve "${TLS_SERVER_NAME}:443:127.0.0.1" \
            "$@" \
            "https://${TLS_SERVER_NAME}:443${path}" 2>/dev/null
        return $?
    fi

    if use_local_cert_pin; then
        curl -s --cacert "${TLS_CERT_FILE}" -X "${method}" \
            --resolve "${TLS_SERVER_NAME}:443:127.0.0.1" \
            "$@" \
            "https://${TLS_SERVER_NAME}:443${path}" 2>/dev/null
        return $?
    fi

    curl -s -X "${method}" \
        --resolve "${TLS_SERVER_NAME}:443:127.0.0.1" \
        "$@" \
        "https://${TLS_SERVER_NAME}:443${path}" 2>/dev/null
}

# fetch_status_json — queries the local API for the current system status.
# Sets STATUS_NOTICE and STATUS_JSON directly so the caller keeps both.
fetch_status_json() {
    STATUS_NOTICE=""
    STATUS_JSON=""

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
    response=$(api_request GET "/status") || response=""
    if [ -z "${response}" ] || ! echo "${response}" | jq -e . >/dev/null 2>&1; then
        STATUS_NOTICE="Status unavailable: unable to reach the local unlock service."
        status_fallback_json
        return 1
    fi

    STATUS_JSON="${response}"
    return 0
}

# print_drives JSON — renders the drive list from a /status JSON response
# as indented lines with lock state and OPAL capability.
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

# print_interfaces JSON — renders the network interface list from a /status
# JSON response with link state, MAC, and IP addresses.
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

# print_status_summary JSON — renders build info, drive counts, boot readiness,
# and remaining unlock attempts in a compact block above the detailed lists.
print_status_summary() {
    echo "$1" | jq -r '
        def drives: (.drives // []);
        def opal: (drives | map(select(.opal)));
        def locked: (opal | map(select(.locked)));
        "Build: " + (.build // "unknown"),
        ("Drive summary: " + ((drives | length) | tostring) + " total, " + ((opal | length) | tostring) + " OPAL, " + ((locked | length) | tostring) + " locked"),
        ("Boot-ready drives: " + (if .bootReady then ((.bootDrives // []) | join(", ")) else "none yet" end)),
        ("Unlock attempts: " + ((.failedAttempts // 0) | tostring) + "/" + ((.maxAttempts // 0) | tostring) + " failed (" + ((.attemptsRemaining // 0) | tostring) + " remaining)")
    ' 2>/dev/null
}

# fetch_diagnostics_json — queries /diagnostics. On failure it sets DIAG_NOTICE
# and falls back to an empty diagnostics payload.
fetch_diagnostics_json() {
    DIAG_NOTICE=""
    DIAG_JSON='{"drives":[]}'

    if ! have_command curl || ! have_command jq; then
        DIAG_NOTICE="Diagnostics unavailable: curl or jq is not installed in this build."
        return 1
    fi

    local response
    response=$(api_request GET "/diagnostics") || response=""
    if [ -z "${response}" ] || ! echo "${response}" | jq -e . >/dev/null 2>&1; then
        DIAG_NOTICE="Diagnostics unavailable: unable to reach the local unlock service."
        return 1
    fi

    DIAG_JSON="${response}"
    return 0
}

# print_drive_diagnostics JSON — renders compact OPAL query summaries.
print_drive_diagnostics() {
    local count
    count=$(echo "$1" | jq -r '.drives | length' 2>/dev/null || echo 0)
    if [ "${count}" -eq 0 ]; then
        echo "  No OPAL diagnostics available."
        return 0
    fi

    echo "$1" | jq -r '
        .drives[] |
        "  " + .device +
        "  opal=" + (.opal|tostring) +
        "  locked=" + (.locked|tostring) +
        "  locking=" + (.lockingSupported // "unknown") +
        "  enabled=" + (.lockingEnabled // "unknown") +
        "  mbrEnabled=" + (.mbrEnabled // "unknown") +
        "  mbrDone=" + (.mbrDone // "unknown") +
        "  mediaEncrypt=" + (.mediaEncrypt // "unknown") +
        "  queryLocked=" + (.lockingRange0Locked // "unknown")
    '
}

# show_diagnostics_screen — renders status, network interfaces, and OPAL
# diagnostics in one place for headless troubleshooting.
show_diagnostics_screen() {
    clear
    echo "${CLR_BLUE}DIAGNOSTICS${CLR_RESET}"
    echo
    fetch_status_json
    fetch_diagnostics_json

    if [ -n "${STATUS_NOTICE}" ]; then
        echo "${CLR_DIM}${STATUS_NOTICE}${CLR_RESET}"
        echo
    fi
    print_status_summary "${STATUS_JSON}"
    echo
    print_drives "${STATUS_JSON}"
    echo
    echo "${CLR_DIM}Network Interfaces (use these names for NET_IFACES / EXCLUDE_NETDEV):${CLR_RESET}"
    print_interfaces "${STATUS_JSON}"
    echo
    if [ -n "${DIAG_NOTICE}" ]; then
        echo "${CLR_DIM}${DIAG_NOTICE}${CLR_RESET}"
        echo
    fi
    echo "${CLR_DIM}Drive Diagnostics:${CLR_RESET}"
    print_drive_diagnostics "${DIAG_JSON}"
    echo
    echo -n "Press Enter to return: "
    read -r _
}

# has_error JSON — returns 0 if the JSON object contains an "error" key.
has_error() {
    echo "$1" | jq -e '.error' > /dev/null 2>&1
}

# get_error JSON — extracts and prints the "error" value from a JSON object.
get_error() {
    echo "$1" | jq -r '.error // empty'
}

# has_results JSON — returns 0 if the JSON contains a non-empty "results" array.
has_results() {
    echo "$1" | jq -e '(.results // []) | length > 0' > /dev/null 2>&1
}

# api_post PATH [curl args...] — helper for API POSTs through the same loopback
# TLS path used by status polling.
api_post() {
    local path="$1"
    shift
    api_request POST "${path}" "$@"
}

# post_with_auth PATH — POSTs to the API, including the session auth token
# header when one has been obtained from a successful unlock.
post_with_auth() {
    if [ -n "$AUTH_TOKEN" ]; then
        api_post "$1" -H "X-Auth-Token: $AUTH_TOKEN"
    else
        api_post "$1"
    fi
}

# fetch_boot_status_json — reads /boot-status into BOOT_STATUS_JSON.
fetch_boot_status_json() {
    BOOT_STATUS_JSON='{}'
    local response
    response=$(api_request GET "/boot-status") || response=""
    if [ -z "${response}" ] || ! echo "${response}" | jq -e . >/dev/null 2>&1; then
        return 1
    fi
    BOOT_STATUS_JSON="${response}"
    return 0
}

# print_boot_debug JSON — renders any boot debug lines present in a boot-status
# or boot-result payload.
print_boot_debug() {
    echo "$1" | jq -r '
        if (.debug // []) | length > 0 then
            "Boot debug:",
            ((.debug // [])[] | "  " + .)
        else empty end
    ' 2>/dev/null
}

# discover_kernels_json — starts /boot-list and waits for /boot-status to
# return discovered kernels. Stores the kernel array JSON in KERNELS_JSON.
discover_kernels_json() {
    KERNELS_JSON='[]'

    local response
    response=$(post_with_auth "/boot-list")
    if [ -z "${response}" ]; then
        echo "Service unavailable."
        return 1
    fi
    if has_error "${response}"; then
        echo "Error: $(get_error "${response}")"
        return 1
    fi

    echo "Searching for bootable kernels..."
    while :; do
        sleep 1
        if ! fetch_boot_status_json; then
            echo "Status unavailable while discovering kernels."
            return 1
        fi
        if echo "${BOOT_STATUS_JSON}" | jq -e '.inProgress' >/dev/null 2>&1; then
            continue
        fi
        if has_error "${BOOT_STATUS_JSON}"; then
            echo "Error: $(get_error "${BOOT_STATUS_JSON}")"
            print_boot_debug "${BOOT_STATUS_JSON}"
            return 1
        fi
        KERNELS_JSON=$(echo "${BOOT_STATUS_JSON}" | jq -c '.kernels // []')
        if [ "$(echo "${KERNELS_JSON}" | jq -r 'length')" -eq 0 ]; then
            echo "No bootable kernels found."
            print_boot_debug "${BOOT_STATUS_JSON}"
            return 1
        fi
        print_boot_debug "${BOOT_STATUS_JSON}"
        return 0
    done
}

# render_kernel_choices JSON SHOW_RECOVERY — prints visible kernels and stores
# their real indices into KERNEL_INDEX_MAP for later selection.
render_kernel_choices() {
    local kernels_json="$1" show_recovery="$2" entries display real_idx label source cmdline_short recovery_tag
    KERNEL_INDEX_MAP=""
    display=0
    entries=$(echo "${kernels_json}" | SHOW_RECOVERY="${show_recovery}" jq -r '
        to_entries[]
        | select((env.SHOW_RECOVERY == "1") or (.value.recovery | not))
        | [
            .key,
            (.value.kernelName // .value.kernel // "Kernel"),
            (.value.source // ""),
            (if (.value.cmdline // "") | length > 60 then (.value.cmdline[0:60] + "...") else (.value.cmdline // "") end),
            (if .value.recovery then "1" else "0" end)
          ] | @tsv
    ')
    if [ -z "${entries}" ]; then
        echo "  No kernels visible with recovery hidden."
        return 1
    fi
    while IFS="$(printf '\t')" read -r real_idx label source cmdline_short recovery_tag; do
        [ -n "${real_idx}" ] || continue
        display=$((display + 1))
        KERNEL_INDEX_MAP="${KERNEL_INDEX_MAP} ${real_idx}"
        if [ "${recovery_tag}" = "1" ]; then
            label="[Recovery] ${label}"
        fi
        printf '  %d. %s' "${display}" "${label}"
        [ -n "${source}" ] && printf '  (%s' "${source}" || printf '  ('
        [ -n "${cmdline_short}" ] && printf ' | %s' "${cmdline_short}"
        printf ')\n'
    done <<EOF
${entries}
EOF
    return 0
}

# kernel_real_index DISPLAY_INDEX INDEX_MAP... — resolves the displayed menu
# number to the original kernel index expected by /boot.
kernel_real_index() {
    local wanted="$1" idx=1 item
    shift
    for item in "$@"; do
        if [ "${idx}" -eq "${wanted}" ]; then
            printf '%s\n' "${item}"
            return 0
        fi
        idx=$((idx + 1))
    done
    return 1
}

# request_boot_with_kernel REAL_INDEX — POSTs /boot with the selected kernel
# index and waits for the boot-status transition to success or error.
request_boot_with_kernel() {
    local real_index="$1" response payload warn
    payload=$(jq -n --argjson i "${real_index}" '{kernelIndex: $i}')
    response=$(api_post "/boot" -H "Content-Type: application/json" -d "${payload}")
    if [ -z "${response}" ]; then
        echo "Service unavailable."
        return 1
    fi
    if has_error "${response}"; then
        echo "Error: $(get_error "${response}")"
        return 1
    fi

    echo "Booting selected kernel..."
    while :; do
        sleep 1
        if ! fetch_boot_status_json; then
            echo "OS handoff may already be in progress..."
            return 0
        fi
        if echo "${BOOT_STATUS_JSON}" | jq -e '.inProgress' >/dev/null 2>&1; then
            continue
        fi
        if has_error "${BOOT_STATUS_JSON}"; then
            echo "Error: $(get_error "${BOOT_STATUS_JSON}")"
            print_boot_debug "${BOOT_STATUS_JSON}"
            return 1
        fi
        if echo "${BOOT_STATUS_JSON}" | jq -e '.accepted' >/dev/null 2>&1; then
            warn=$(echo "${BOOT_STATUS_JSON}" | jq -r '.result.warning // empty')
            [ -n "${warn}" ] && echo "Warning: ${warn}"
            print_boot_debug "$(echo "${BOOT_STATUS_JSON}" | jq -c '.result // {}')"
            echo "OS handoff initiated successfully."
            return 0
        fi
        echo "Boot status unavailable."
        return 1
    done
}

# show_boot_menu — mirrors the console/web boot flow: discover kernels first,
# then let the operator select which one to kexec into.
show_boot_menu() {
    local show_recovery="0" choice visible_count real_index
    if ! discover_kernels_json; then
        echo -n "Press Enter to return: "
        read -r _
        return
    fi

    while :; do
        fetch_status_json
        clear
        echo "${CLR_BLUE}BOOT SELECTION${CLR_RESET}"
        echo
        if [ -n "${STATUS_NOTICE}" ]; then
            echo "${CLR_DIM}${STATUS_NOTICE}${CLR_RESET}"
            echo
        fi
        print_status_summary "${STATUS_JSON}"
        echo
        echo "Discovered kernels:"
        if ! render_kernel_choices "${KERNELS_JSON}" "${show_recovery}"; then
            :
        fi
        echo
        if [ "${show_recovery}" = "1" ]; then
            echo "[H] Hide recovery  [Q] Cancel"
        else
            echo "[H] Show recovery  [Q] Cancel"
        fi
        echo -n "Kernel number [Enter=1]: "
        read -r choice
        choice=$(echo "${choice}" | tr '[:lower:]' '[:upper:]')
        case "${choice}" in
            Q) return ;;
            H)
                if [ "${show_recovery}" = "1" ]; then
                    show_recovery="0"
                else
                    show_recovery="1"
                fi
                continue
                ;;
            "")
                choice="1"
                ;;
        esac
        case "${choice}" in
            *[!0-9]*)
                echo "Invalid selection."
                sleep 2
                continue
                ;;
        esac
        set -- ${KERNEL_INDEX_MAP}
        visible_count=$#
        if [ "${visible_count}" -eq 0 ] || [ "${choice}" -lt 1 ] || [ "${choice}" -gt "${visible_count}" ]; then
            echo "Choose a number between 1 and ${visible_count}."
            sleep 2
            continue
        fi
        real_index=$(kernel_real_index "${choice}" "$@") || real_index=""
        if [ -z "${real_index}" ]; then
            echo "Failed to resolve selected kernel."
            sleep 2
            continue
        fi
        if request_boot_with_kernel "${real_index}"; then
            exit 0
        fi
        echo -n "Press Enter to return: "
        read -r _
        return
    done
}

# ---------------------------------------------------------------------------
# Main loop — interactive menu for unlock, boot, reboot, shutdown, and quit.
# Refreshes drive/network status on every iteration; auto-loops on 10s timeout.
# ---------------------------------------------------------------------------
while true; do
    clear
    echo "${CLR_BLUE}SED UNLOCK (SSH)${CLR_RESET}"
    echo "${CLR_DIM}Auto logout: 5m | Refresh: 10s${CLR_RESET}"
    echo

    fetch_status_json
    if [ -n "${STATUS_NOTICE}" ]; then
        echo "${CLR_DIM}${STATUS_NOTICE}${CLR_RESET}"
        echo
    fi

    print_status_summary "${STATUS_JSON}"
    echo
    print_drives "${STATUS_JSON}"
    echo
    echo "${CLR_DIM}Network Interfaces (use these names for NET_IFACES / EXCLUDE_NETDEV):${CLR_RESET}"
    print_interfaces "${STATUS_JSON}"
    echo

    echo "[U] ${CLR_PURPLE}Unlock${CLR_RESET}  [B] ${CLR_BLUE}Boot${CLR_RESET}  [D] ${CLR_BLUE}Diagnostics${CLR_RESET}  [R] ${CLR_ORANGE}Reboot${CLR_RESET}  [S] ${CLR_BLUE}Shutdown${CLR_RESET}  [Q] Quit"
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

            RESP=$(api_post "/unlock" \
                -H "Content-Type: application/json" \
                -d "$(jq -n --arg p "$pass" '{password: $p}')")

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
            echo -n "Press Enter to continue: "
            read -r _
            ;;

        B)
            show_boot_menu
            ;;

        D)
            show_diagnostics_screen
            ;;

        R)
            echo -n "Reboot? (y/n): "
            read -r c
            if [ "$c" = "y" ]; then
                RESP=$(post_with_auth "/reboot")
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
                RESP=$(post_with_auth "/poweroff")
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
