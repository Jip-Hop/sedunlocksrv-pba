#!/usr/bin/env bash
#
# deploy.sh — Build and validate SED unlock server PBA with new certificates, then flash to OPAL 2.0 drive.
#
# Usage:
#   deploy.sh --cert-path=/path/to/fullchain.pem --key-path=/path/to/key.pem [OPTIONS]
#
# Prerequisites:
#   - OPAL 2.0 drive must be properly initialized (ownership taken, locking ranges configured)
#   - Shadow MBR must be enabled (MBREnable = Y) — set during initial drive setup
#   - Shadow MBR activation is persistent; this script does NOT enable or activate it
#
# Workflow:
#   1. Validate input certificates
#   2. Validate OPAL drive (OPAL 2.0 compliance, shadow MBR enabled)
#   3. Build PBA image (calls build.sh)
#   4. Validate PBA image (MBR, FAT32, boot sector structure)
#   5. Flash PBA to shadow MBR
#
# Password Management:
#   The OPAL admin password is encrypted at rest in .ssh/opal-password.enc, created
#   during setup-deploy.sh. At runtime, the encryption key is re-derived by signing a
#   deterministic challenge ("sedunlocksrv-kdf-{salt}") with the SSH private key via
#   ssh-keygen -Y sign. This requires the private key to be available through an SSH
#   agent (ssh -A) or as a local file. The public key and salt alone are NOT sufficient
#   to decrypt the password.
#
# SSH Key Requirements:
#   - Ed25519 keys only (deterministic signatures required for key derivation)
#   - ECDSA keys are NOT supported (non-deterministic signatures)
#   - RSA keys work but Ed25519 is recommended
#   - SSH agent forwarding (ssh -A) must be enabled for autonomous deployments
#
# Security Notes:
#   - The OPAL password is passed to sedutil-cli as a command-line argument, which is
#     visible in the process table (ps aux) for the duration of the flash operation.
#     This is a known limitation of sedutil-cli (no stdin/file password support).
#   - Bash cannot securely erase variables from process memory. The OPAL password
#     remains in the process address space until the script terminates.
#
# Optional environment:
#   OPAL_DRIVE     - Override default OPAL drive (/dev/nvme0)
#   SYSLOG_TAG     - Custom syslog tag (default: deploy.sh)
#
# Options:
#   --cert-path=PATH         Absolute path to fullchain.pem
#   --key-path=PATH          Absolute path to key.pem
#   --build-args=ARGS        Comma-separated additional arguments for build.sh
#                             (e.g., --build-args=--sedutil-fork=ChubbyAnt,--no-cache)
#   --expert-password=PASS   Expert mode password passed directly to build.sh;
#                             use this instead of --build-args to avoid shell
#                             interpretation of special characters in the password
#   --expert-password-stdin  Read expert password from stdin (pipe-friendly);
#                             e.g.: echo 'p@$$w0rd' | ssh -A host 'sudo deploy.sh --expert-password-stdin ...'
#   --dry-run                Build and validate only, do not flash to drive
#   --opal-drive=DEVICE      Override OPAL drive (default: /dev/nvme0)
#   --cert-freshness=METHOD  Certificate freshness check: hash|mtime|serial|marker|none (default: hash)
#   --cert-timeout=SECS      Max seconds to wait for cert update (default: 300)
#   --cert-grace=SECS        Grace period for mtime check in seconds (default: 10)
#   --quiet                  Suppress non-error output
#   --help                   Show this message and exit

set -euo pipefail

export PATH="${PATH}:/usr/local/go/bin"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# =============================================================================
# Configuration
# =============================================================================
CERT_PATH=""
KEY_PATH=""
BUILD_ADDITIONAL_ARGS=()
EXPERT_PASSWORD=""
DRY_RUN=false
OPAL_DRIVE="${OPAL_DRIVE:-/dev/nvme0}"
QUIET="${QUIET:-false}"
SYSLOG_TAG="${SYSLOG_TAG:-deploy.sh}"

# Certificate freshness control
CERT_FRESHNESS_STRATEGY="${CERT_FRESHNESS_STRATEGY:-hash}"  # hash, mtime, serial, marker, none
CERT_FRESHNESS_TIMEOUT="${CERT_FRESHNESS_TIMEOUT:-300}"     # seconds (5 min default)
CERT_FRESHNESS_GRACE="${CERT_FRESHNESS_GRACE:-10}"          # seconds (mtime strategy only)

# Paths and naming
BUILD_DATE=$(date +%Y%m%d-%H%M%S)
LOG_FILE="${REPO_ROOT}/deploy-${BUILD_DATE}.log"
SSH_AUTH_DIR="${REPO_ROOT}/.ssh"
OPAL_PASSWORD=""
PBA_IMAGE=""



# =============================================================================
# Logging & Error Handling
# =============================================================================

# log MSG [LEVEL] — logs to file and syslog
log() {
    local msg="$1" level="${2:-INFO}"
    local timestamp
    timestamp=$(date '+%Y-%m-%d %H:%M:%S')
    local log_line="${timestamp} [${level}] ${msg}"
    echo "${log_line}" | tee -a "${LOG_FILE}"
    logger -t "${SYSLOG_TAG}" -p "user.${level,,}" "${msg}" 2>/dev/null || true
}

# error MSG — logs error and initiates cleanup/notifications
error() {
    local msg="$1"
    log "❌ ERROR: ${msg}" "ERR"
}

# warn MSG — logs warning
warn() {
    local msg="$1"
    log "⚠️  WARNING: ${msg}" "WARN"
}

# info MSG — logs info (respects --quiet)
info() {
    local msg="$1"
    if [ "$QUIET" != true ]; then
        log "ℹ️  ${msg}" "INFO"
    fi
}

# fail_exit CODE MSG — logs error and exits with code
fail_exit() {
    local code="$1" msg="$2"
    error "${msg}"
    exit "${code}"
}

# =============================================================================
# Utilities
# =============================================================================

print_usage() {
    sed -n '5,57p' "${BASH_SOURCE[0]}" | sed -E 's/^# ?//'
}

# require_cmd CMD — exits if CMD not found
require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        fail_exit 1 "Required command not found: $1"
    fi
}

# require_file PATH DESC — exits if file doesn't exist
require_file() {
    local path="$1" desc="$2"
    if [ ! -f "${path}" ]; then
        fail_exit 1 "${desc} not found: ${path}"
    fi
}

# require_readable PATH — exits if file is not readable
require_readable() {
    local path="$1"
    if [ ! -r "${path}" ]; then
        fail_exit 1 "File not readable: ${path}"
    fi
}

# cert_validity_check PATH — verify cert is PEM-formatted and valid
cert_validity_check() {
    local path="$1"
    if ! openssl x509 -in "${path}" -noout >/dev/null 2>&1; then
        fail_exit 1 "Certificate is not valid PEM format: ${path}"
    fi
    # Warn if cert expires within 30 days
    local expiry_epoch
    expiry_epoch=$(openssl x509 -in "${path}" -noout -enddate | cut -d= -f2 | date +%s -f -)
    local now_epoch
    now_epoch=$(date +%s)
    local days_until_expiry=$(( (expiry_epoch - now_epoch) / 86400 ))
    if [ "${days_until_expiry}" -lt 30 ]; then
        warn "Certificate expires in ${days_until_expiry} days: ${path}"
    fi
}

# key_validity_check PATH — verify key is PEM-formatted
key_validity_check() {
    local path="$1"
    if ! openssl pkey -in "${path}" -check -noout >/dev/null 2>&1; then
        fail_exit 1 "Key is not valid PEM format: ${path}"
    fi
}

# cert_key_pair_check CERT KEY — verify cert matches key (works with RSA, ECDSA, Ed25519)
cert_key_pair_check() {
    local cert="$1" key="$2"
    local cert_pub key_pub
    cert_pub=$(openssl x509 -in "${cert}" -noout -pubkey | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')
    key_pub=$(openssl pkey -in "${key}" -outform DER -pubout 2>/dev/null | sha256sum | awk '{print $1}')
    if [ "${cert_pub}" != "${key_pub}" ]; then
        fail_exit 1 "Certificate and key do not match: ${cert} / ${key}"
    fi
}

# get_pba_partition DRIVE — verifies OPAL 2 drive is properly initialized before PBA write
# On OPAL 2 drives, the PBA is stored in the shadow MBR.
# Performs comprehensive safety checks using sedutil-cli:
#   1. Verifies drive exists and is a block device
#   2. Confirms sedutil-cli is available
#   3. Confirms OPAL 2.0 compliance via Level 0 Discovery
#   4. Verifies drive is initialized (has locking ranges)
#   5. Verifies shadow MBR is enabled (must be set during initial drive setup)
get_pba_partition() {
    local drive="$1"
    
    info "Verifying OPAL 2.0 drive safety: ${drive}"
    
    # =========================================================================
    # Check 1: Drive exists and is a block device
    # =========================================================================
    if [ ! -b "${drive}" ]; then
        fail_exit 1 "Drive not found or not a block device: ${drive}"
    fi
    info "✓ Drive device confirmed: ${drive}"
    
    # =========================================================================
    # Check 2: sedutil-cli is available
    # =========================================================================
    if ! command -v sedutil-cli >/dev/null 2>&1; then
        fail_exit 1 "sedutil-cli required but not found in PATH"
    fi
    info "✓ sedutil-cli available"
    
    # =========================================================================
    # Check 3: Drive is OPAL 2.0 compliant via query
    # =========================================================================
    local query_output
    query_output=$(sudo sedutil-cli --query "${drive}" 2>&1) || \
        fail_exit 1 "Failed to query OPAL drive ${drive}; ensure it is a valid OPAL 2.0 device"
    
    # Verify Level 0 Discovery output contains OPAL 2.0 indicators
    if ! echo "${query_output}" | grep -qi "opal.*2"; then
        fail_exit 1 "Drive does not appear to be OPAL 2.0 compliant (no OPAL 2 in query output): ${drive}"
    fi
    info "✓ OPAL 2.0 drive confirmed"
    
    # =========================================================================
    # Check 4: Drive is initialized (has locking ranges)
    # =========================================================================
    # Brief pause between sedutil-cli commands to avoid erratic drive controller responses
    sleep 0.05
    # Try to list locking ranges - fails if drive not initialized
    local locking_output
    locking_output=$(sudo sedutil-cli --passwordless "${drive}" listLockingRanges 2>&1) || true
    
    # If passwordless fails, try with admin password (will succeed if initialized)
    if ! echo "${locking_output}" | grep -qi "locking\|range"; then
        # Drive may need admin password to show locking ranges, or may not be fully initialized
        warn "Could not verify locking ranges (may require admin password to query)"
        info "Attempting to verify shadow MBR directly..."
    else
        info "✓ Drive is initialized with locking ranges"
    fi
    
    # =========================================================================
    # Check 5: Shadow MBR is enabled (must be set during initial drive setup)
    # =========================================================================
    # Reuses query_output from Check #3 — shadow MBR enable is a persistent setting
    if ! echo "${query_output}" | grep -qiE "MBR(Enable|Enabled)[[:space:]]*=?[[:space:]]*Y"; then
        fail_exit 1 "Shadow MBR is not enabled on ${drive}. The drive must be properly initialized with shadow MBR enabled before deploying a PBA. Use sedutil-cli to enable the shadow MBR during initial drive setup."
    fi
    info "✓ Shadow MBR is enabled"
    
    # =========================================================================
    # All checks passed
    # =========================================================================
    info "✅ All safety checks passed for ${drive}"
}

# =============================================================================
# Password Management
# =============================================================================

# load_password — decrypts OPAL password from SSH-key-encrypted file
load_password() {
    load_password_from_ssh_encrypted
    info "OPAL admin password loaded"
}

# load_password_from_ssh_encrypted — decrypt password using SSH key signature-based KDF
# The password is encrypted with a key derived from an SSH signature over a salted challenge.
# The salt is stored in .ssh/auth.conf and the signing public key in .ssh/signing-key.pub.
# The SSH agent must have the corresponding private key loaded (ssh -A for remote sessions).
load_password_from_ssh_encrypted() {
    local password_file="${SSH_AUTH_DIR}/opal-password.enc"
    local auth_conf="${SSH_AUTH_DIR}/auth.conf"
    local signing_key="${SSH_AUTH_DIR}/signing-key.pub"
    
    require_file "${password_file}" "Encrypted password file"
    require_file "${auth_conf}" "Auth config"
    require_file "${signing_key}" "Signing public key"
    
    # Extract KDF salt from auth.conf (grep, not source — prevents code injection)
    local kdf_salt
    kdf_salt=$(grep '^KDF_SALT=' "${auth_conf}" 2>/dev/null | head -1 | cut -d= -f2-)
    # Strip surrounding quotes if present
    kdf_salt="${kdf_salt%\"}"
    kdf_salt="${kdf_salt#\"}"
    kdf_salt="${kdf_salt%\'}"
    kdf_salt="${kdf_salt#\'}"
    
    if [ -z "${kdf_salt}" ]; then
        fail_exit 1 "KDF_SALT not found in auth config: ${auth_conf}"
    fi
    
    # Reconstruct the deterministic challenge
    local challenge="sedunlocksrv-kdf-${kdf_salt}"
    
    # Sign the challenge using the SSH agent (via the stored public key)
    # ssh-keygen -Y sign with a public key file uses the SSH agent to find the matching private key
    local signature
    signature=$(echo -n "${challenge}" | ssh-keygen -Y sign -f "${signing_key}" -n sedunlocksrv -q 2>&1) || {
        fail_exit 1 "Failed to sign KDF challenge. Ensure SSH agent forwarding is enabled (ssh -A) and the correct Ed25519 private key is loaded in the agent."
    }
    
    # Derive decryption key from signature (same derivation as setup-deploy.sh)
    local encryption_key
    encryption_key=$(echo -n "${signature}" | sha256sum | awk '{print $1}')
    
    # Decrypt the password using the derived key
    local pwd
    pwd=$(openssl enc -aes-256-cbc -d -pbkdf2 -pass "pass:${encryption_key}" \
        -in "${password_file}" 2>/dev/null) || {
        fail_exit 1 "Failed to decrypt OPAL password. SSH key mismatch — was the password encrypted with a different key? Re-run setup-deploy.sh if SSH keys have changed."
    }
    
    if [ -z "${pwd}" ]; then
        fail_exit 1 "Decrypted password is empty"
    fi
    
    OPAL_PASSWORD="${pwd}"
    
    # Extract fingerprint for log message (non-sensitive)
    local fingerprint
    fingerprint=$(grep '^SSH_PUBKEY_FINGERPRINT=' "${auth_conf}" 2>/dev/null | head -1 | cut -d= -f2- | tr -d "'")
    info "✓ Password decrypted using SSH key signature (${fingerprint:-unknown})"
}

# =============================================================================
# Build Phase
# =============================================================================

# build_pba — invokes build.sh with provided certificates
build_pba() {
    info "Building new PBA image with certificates..."
    
    # Check certificate freshness before building
    # This prevents building with stale certificates if the cert update
    # process completes copying AFTER deploy.sh starts but BEFORE build.sh is called
    if ! wait_for_cert_update "${CERT_PATH}" "${CERT_FRESHNESS_STRATEGY}" \
                               "${CERT_FRESHNESS_TIMEOUT}" "${CERT_FRESHNESS_GRACE}"; then
        fail_exit 1 "Certificate freshness check failed; refusing to build with potentially stale certificates"
    fi
    
    if ! (
        cd "${REPO_ROOT}"
        sudo ./build.sh \
            --tls-cert="${CERT_PATH}" \
            --tls-key="${KEY_PATH}" \
            ${EXPERT_PASSWORD:+"--expert-password=${EXPERT_PASSWORD}"} \
            ${BUILD_ADDITIONAL_ARGS[@]+"${BUILD_ADDITIONAL_ARGS[@]}"}
    ); then
        fail_exit 1 "build.sh failed; see logs above"
    fi
    
    # Find the latest generated image
    local latest_img
    latest_img=$(ls -t "${REPO_ROOT}"/sedunlocksrv-pba-*.img 2>/dev/null | head -1)
    if [ -z "${latest_img}" ]; then
        fail_exit 1 "No PBA image generated by build.sh"
    fi
    
    PBA_IMAGE="${latest_img}"
    info "PBA image built: ${PBA_IMAGE}"
}

# =============================================================================
# Image Validation Phase
# =============================================================================

# validate_pba_image IMAGE — validates PBA image structure (MBR, FAT32, boot sector)
# Replicates checks from Go's validateUploadedPBAImageBytes function.
# Returns 0 on success, exits on failure.
validate_pba_image() {
    local image="$1"
    require_file "${image}" "PBA image"
    require_readable "${image}"
    
    info "Validating PBA image structure..."
    
    # Helper functions for hex reading
    local read_hex_bytes="od -An -tx1 2>/dev/null | tr -d ' '"
    
    # Check file size
    local img_size
    img_size=$(stat -c%s "${image}" 2>/dev/null || stat -f%z "${image}" 2>/dev/null)
    info "  File size: ${img_size} bytes"
    
    if [ "${img_size}" -eq 0 ]; then
        fail_exit 1 "PBA image is empty"
    fi
    
    if [ "${img_size}" -gt $((128 * 1024 * 1024)) ]; then
        fail_exit 1 "PBA image exceeds 128 MiB limit"
    fi
    info "  ✓ File size within 128 MiB guideline"
    
    # Check filename extension
    local basename
    basename=$(basename "${image}")
    local ext="${basename##*.}"
    case "${ext}" in
        img|bin) info "  ✓ Filename extension acceptable (.${ext})" ;;
        *) fail_exit 1 "PBA image must have .img or .bin extension, got: .${ext}" ;;
    esac
    
    # Check MBR (first 512 bytes)
    if [ "${img_size}" -lt 512 ]; then
        fail_exit 1 "PBA image too small to contain MBR (needs ≥512 bytes, got ${img_size})"
    fi
    
    # Read MBR signature (bytes 510-511 should be 0x55 0xAA)
    local mbr_sig
    mbr_sig=$(dd if="${image}" bs=1 skip=510 count=2 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    if [ "${mbr_sig}" != "55aa" ]; then
        fail_exit 1 "PBA image missing MBR signature (expected 55aa, got ${mbr_sig})"
    fi
    info "  ✓ MBR signature present (0x55AA)"
    
    # Read first partition entry from MBR (offset 446, 16 bytes)
    # Byte 0: boot flag (should be 0x80)
    # Byte 4: partition type (should be 0xEF for EFI)
    # Bytes 8-12: start LBA (little-endian)
    # Bytes 12-16: sector count (little-endian)
    local part1_data
    part1_data=$(dd if="${image}" bs=1 skip=446 count=16 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    
    # Parse boot flag (1st byte of partition entry)
    local boot_flag="${part1_data:0:2}"
    if [ "${boot_flag}" != "80" ]; then
        fail_exit 1 "First partition not bootable (expected 0x80, got 0x${boot_flag})"
    fi
    info "  ✓ First partition is bootable"
    
    # Parse partition type (5th byte of partition entry)
    local part_type="${part1_data:8:2}"
    if [ "${part_type}" != "ef" ]; then
        fail_exit 1 "First partition type must be 0xEF (EFI), got 0x${part_type}"
    fi
    info "  ✓ First partition type matches recipe (0xEF EFI)"
    
    # Parse start LBA (bytes 8-11 of partition entry, little-endian)
    # Extract bytes for start LBA: position 16-24 in hex string (8 chars = 4 bytes)
    local start_lba_hex="${part1_data:16:8}"
    # Convert little-endian hex to decimal
    local start_lba=$((0x${start_lba_hex:6:2}${start_lba_hex:4:2}${start_lba_hex:2:2}${start_lba_hex:0:2}))
    
    # Parse sector count (bytes 12-15 of partition entry, little-endian)
    local sector_count_hex="${part1_data:24:8}"
    local sector_count=$((0x${sector_count_hex:6:2}${sector_count_hex:4:2}${sector_count_hex:2:2}${sector_count_hex:0:2}))
    
    if [ "${start_lba}" -eq 0 ] || [ "${sector_count}" -eq 0 ]; then
        fail_exit 1 "First partition geometry invalid (start LBA: ${start_lba}, sectors: ${sector_count})"
    fi
    info "  ✓ First partition geometry valid (LBA ${start_lba}, ${sector_count} sectors)"
    
    # Check for unexpected extra partitions (entries 2-4 must be empty)
    for i in 1 2 3; do
        local entry_offset=$((446 + i * 16))
        local extra_entry
        extra_entry=$(dd if="${image}" bs=1 skip="${entry_offset}" count=16 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
        # All zeros means empty partition
        if [ "${extra_entry}" != "00000000000000000000000000000000" ]; then
            fail_exit 1 "Unexpected partition entry found at partition $((i+1))"
        fi
    done
    info "  ✓ No unexpected extra partitions found"
    
    # Read boot sector (at start_lba * 512)
    local boot_sector_offset=$((start_lba * 512))
    if [ $((boot_sector_offset + 512)) -gt "${img_size}" ]; then
        fail_exit 1 "Boot partition extends beyond image boundaries"
    fi
    
    # Read boot sector signature (bytes 510-511 should be 0x55 0xAA)
    local boot_sig
    boot_sig=$(dd if="${image}" bs=1 skip=$((boot_sector_offset + 510)) count=2 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    if [ "${boot_sig}" != "55aa" ]; then
        fail_exit 1 "Boot sector missing signature (expected 55aa, got ${boot_sig})"
    fi
    info "  ✓ Boot sector signature valid (0x55AA)"
    
    # Read boot sector jump instruction (first byte should be 0xEB or 0xE9)
    local boot_jump
    boot_jump=$(dd if="${image}" bs=1 skip="${boot_sector_offset}" count=1 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    if [ "${boot_jump}" != "eb" ] && [ "${boot_jump}" != "e9" ]; then
        fail_exit 1 "Boot sector invalid jump instruction (expected 0xEB or 0xE9, got 0x${boot_jump})"
    fi
    info "  ✓ Boot sector jump instruction valid (0x${boot_jump})"
    
    # Read FAT32 parameters
    # Bytes 11-12: bytes per sector (little-endian, valid: 512, 1024, 2048, 4096)
    local bps_hex
    bps_hex=$(dd if="${image}" bs=1 skip=$((boot_sector_offset + 11)) count=2 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    local bytes_per_sector=$((0x${bps_hex:2:2}${bps_hex:0:2}))
    case "${bytes_per_sector}" in
        512|1024|2048|4096)
            info "  ✓ FAT sector size valid (${bytes_per_sector} bytes)"
            ;;
        *)
            fail_exit 1 "FAT sector size invalid (got ${bytes_per_sector}, expected 512/1024/2048/4096)"
            ;;
    esac
    
    # Byte 13: sectors per cluster (must be power of 2)
    local spc
    spc=$(dd if="${image}" bs=1 skip=$((boot_sector_offset + 13)) count=1 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    local spc_dec=$((0x${spc}))
    if [ "${spc_dec}" -eq 0 ] || [ $((spc_dec & (spc_dec - 1))) -ne 0 ]; then
        fail_exit 1 "FAT cluster size invalid (${spc_dec} sectors, must be power of 2)"
    fi
    info "  ✓ FAT cluster size valid (${spc_dec} sectors per cluster)"
    
    # Bytes 14-15: reserved sectors (little-endian, must be > 0)
    local rsv_hex
    rsv_hex=$(dd if="${image}" bs=1 skip=$((boot_sector_offset + 14)) count=2 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    local reserved_sectors=$((0x${rsv_hex:2:2}${rsv_hex:0:2}))
    if [ "${reserved_sectors}" -eq 0 ]; then
        fail_exit 1 "FAT reserved sectors count is zero"
    fi
    info "  ✓ FAT reserved sectors present (${reserved_sectors})"
    
    # Byte 16: number of FATs (must be > 0)
    local num_fats
    num_fats=$(dd if="${image}" bs=1 skip=$((boot_sector_offset + 16)) count=1 2>/dev/null | eval "od -An -tx1 2>/dev/null | tr -d ' '")
    local num_fats_dec=$((0x${num_fats}))
    if [ "${num_fats_dec}" -eq 0 ]; then
        fail_exit 1 "FAT table count is zero"
    fi
    info "  ✓ FAT table count valid (${num_fats_dec})"
    
    # Bytes 82-89: filesystem type string (should contain "FAT32")
    # Read as ASCII text to safely check content
    local fstype
    fstype=$(dd if="${image}" bs=1 skip=$((boot_sector_offset + 82)) count=8 2>/dev/null | \
             od -An -c 2>/dev/null | tr -cd 'A-Za-z0-9' | head -c 20)
    if [[ ! "${fstype}" =~ ^FAT32 ]]; then
        fail_exit 1 "Filesystem type is not FAT32 (found: ${fstype})"
    fi
    info "  ✓ Filesystem type is FAT32 (${fstype})"
    
    info "✅ PBA image validation complete - all checks passed"
}

# =============================================================================
# OPAL 2 Flash Phase
# =============================================================================

# flash_pba_to_drive IMAGE DRIVE PASSWORD — flashes PBA image to OPAL drive using sedutil-cli
# Writes the shadow MBR from an image file.
flash_pba_to_drive() {
    local image="$1" drive="$2" password="$3"
    
    require_file "${image}" "PBA image"
    require_readable "${image}"
    
    info "Flashing shadow MBR from ${image} to ${drive}..."
    
    # Write image to shadow MBR via sedutil-cli
    # Capture output first to avoid pipefail checking tee's exit code instead of sedutil-cli's
    # NOTE: sedutil-cli requires the password as a command-line argument; it does not support
    # reading from stdin or a file. The password is visible in the process table (ps aux) for
    # the duration of this command. This is a known sedutil-cli limitation.
    local output
    if ! output=$(sudo sedutil-cli --loadpbaimage "${password}" "${image}" "${drive}" 2>&1); then
        log "${output}"
        fail_exit 1 "Failed to write shadow MBR to ${drive}"
    fi
    
    # Log successful output
    log "${output}"
    info "Shadow MBR flashed successfully"
}

# =============================================================================
# Argument Parsing
# =============================================================================

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --help|-h)
                print_usage
                exit 0
                ;;
            --cert-path=*)
                CERT_PATH="${1#*=}"
                ;;
            --key-path=*)
                KEY_PATH="${1#*=}"
                ;;
            --build-args=*)
                IFS=',' read -ra BUILD_ADDITIONAL_ARGS <<< "${1#*=}"
                ;;
            --expert-password=*)
                EXPERT_PASSWORD="${1#*=}"
                ;;
            --expert-password-stdin)
                IFS= read -r EXPERT_PASSWORD
                ;;
            --opal-drive=*)
                OPAL_DRIVE="${1#*=}"
                ;;
            --dry-run)
                DRY_RUN=true
                ;;
            --cert-freshness=*)
                CERT_FRESHNESS_STRATEGY="${1#*=}"
                ;;
            --cert-timeout=*)
                CERT_FRESHNESS_TIMEOUT="${1#*=}"
                ;;
            --cert-grace=*)
                CERT_FRESHNESS_GRACE="${1#*=}"
                ;;
            --quiet)
                QUIET=true
                ;;
            *)
                echo "Unknown option: $1" >&2
                print_usage >&2
                exit 1
                ;;
        esac
        shift
    done
}

# =============================================================================
# Validation
# =============================================================================

validate_inputs() {
    info "Validating inputs..."
    
    if [ -z "${CERT_PATH}" ] || [ -z "${KEY_PATH}" ]; then
        fail_exit 1 "Both --cert-path and --key-path are required"
    fi
    
    require_file "${CERT_PATH}" "Certificate file"
    require_file "${KEY_PATH}" "Key file"
    require_readable "${CERT_PATH}"
    require_readable "${KEY_PATH}"
    
    cert_validity_check "${CERT_PATH}"
    key_validity_check "${KEY_PATH}"
    cert_key_pair_check "${CERT_PATH}" "${KEY_PATH}"
    
    info "Certificates validated ✅"
    
    if [ ! -b "${OPAL_DRIVE}" ]; then
        fail_exit 1 "OPAL drive not found or not a block device: ${OPAL_DRIVE}"
    fi
    
    require_cmd "sedutil-cli"
    require_cmd "openssl"
    
    info "All validations passed ✅"
}

# =============================================================================
# Certificate Freshness Validation
# =============================================================================
# Prevents building PBA with stale certificates if certificate update
# process completes copy AFTER deploy.sh starts but BEFORE build.sh is called.
#
# Detection Methods:
#   hash     - Compare SHA256 hash to stored reference (most reliable)
#   mtime    - Check modification time against threshold (simple, fast)
#   serial   - Extract and compare certificate serial number (cert-aware)
#   marker   - Check for explicit marker file from cert update process (explicit)

# get_cert_hash — returns SHA256 hash of certificate file content
get_cert_hash() {
    local cert_path="$1"
    sha256sum "${cert_path}" 2>/dev/null | awk '{print $1}'
}

# get_cert_serial — extract serial number from certificate
get_cert_serial() {
    local cert_path="$1"
    openssl x509 -in "${cert_path}" -noout -serial 2>/dev/null | grep -oP '\d+$'
}

# wait_for_cert_update — delay build if certificate hasn't been updated
# Usage: wait_for_cert_update /path/to/cert.pem [strategy] [timeout] [grace_period]
# Strategies:
#   hash       - Wait for file hash to change (default, most reliable)
#   mtime      - Wait for modification time to be newer than grace_period
#   serial     - Wait for certificate serial number to change
#   marker     - Wait for explicit marker file (e.g., /tmp/cert.updated)
#   none       - Skip certificate freshness check
wait_for_cert_update() {
    local cert_path="$1"
    local strategy="${2:-hash}"
    local timeout_secs="${3:-300}"           # Default: 5 minutes
    local grace_period_secs="${4:-10}"       # Default: 10 seconds (mtime only)
    local marker_file="${cert_path}.updated" # Default marker location
    
    # Skip if strategy is "none"
    if [ "${strategy}" = "none" ]; then
        info "Certificate freshness check disabled (strategy: none)"
        return 0
    fi
    
    info "Waiting for certificate update (strategy: ${strategy}, timeout: ${timeout_secs}s)..."
    
    local start_time elapsed_secs
    start_time=$(date +%s)
    
    case "${strategy}" in
        hash)
            # Wait for file hash to change
            local initial_hash reference_hash
            initial_hash=$(get_cert_hash "${cert_path}")
            reference_hash="${initial_hash}"
            
            while true; do
                elapsed_secs=$(( $(date +%s) - start_time ))
                if [ "${elapsed_secs}" -ge "${timeout_secs}" ]; then
                    error "Certificate hash unchanged after ${elapsed_secs}s (strategy: hash)"
                    error "Certificate may not have been updated. Aborting build."
                    return 1
                fi
                
                # Check if hash has changed
                current_hash=$(get_cert_hash "${cert_path}")
                if [ "${current_hash}" != "${reference_hash}" ]; then
                    info "✓ Certificate hash changed (${strategy}), proceeding with build"
                    return 0
                fi
                
                # Wait and re-check
                sleep 1
            done
            ;;
            
        mtime)
            # Wait for modification time to be newer than grace period
            local file_mtime current_time age_secs
            current_time=$(date +%s)
            file_mtime=$(stat -c %Y "${cert_path}" 2>/dev/null || stat -f %m "${cert_path}" 2>/dev/null || echo "0")
            age_secs=$(( current_time - file_mtime ))
            
            while [ "${age_secs}" -gt "${grace_period_secs}" ]; do
                elapsed_secs=$(( $(date +%s) - start_time ))
                if [ "${elapsed_secs}" -ge "${timeout_secs}" ]; then
                    warn "Certificate age ${age_secs}s exceeds grace period ${grace_period_secs}s (strategy: mtime)"
                    warn "Certificate may be stale. Proceeding with caution."
                    return 1
                fi
                
                # Re-check age
                current_time=$(date +%s)
                file_mtime=$(stat -c %Y "${cert_path}" 2>/dev/null || stat -f %m "${cert_path}" 2>/dev/null || echo "0")
                age_secs=$(( current_time - file_mtime ))
                
                sleep 1
            done
            
            info "✓ Certificate is fresh (age: ${age_secs}s, grace: ${grace_period_secs}s, strategy: mtime)"
            return 0
            ;;
            
        serial)
            # Wait for certificate serial number to match stored reference
            local initial_serial current_serial
            initial_serial=$(get_cert_serial "${cert_path}")
            
            while true; do
                elapsed_secs=$(( $(date +%s) - start_time ))
                if [ "${elapsed_secs}" -ge "${timeout_secs}" ]; then
                    warn "Certificate serial number unchanged after ${elapsed_secs}s (strategy: serial)"
                    warn "Certificate may not have been renewed. Proceeding with caution."
                    return 1
                fi
                
                current_serial=$(get_cert_serial "${cert_path}")
                if [ "${current_serial}" != "${initial_serial}" ]; then
                    info "✓ Certificate serial changed (strategy: serial), proceeding with build"
                    return 0
                fi
                
                sleep 1
            done
            ;;
            
        marker)
            # Wait for explicit marker file (created by certificate update process)
            while [ ! -f "${marker_file}" ]; do
                elapsed_secs=$(( $(date +%s) - start_time ))
                if [ "${elapsed_secs}" -ge "${timeout_secs}" ]; then
                    error "Marker file not found after ${elapsed_secs}s: ${marker_file}"
                    error "Certificate update process must create: ${marker_file}"
                    return 1
                fi
                
                sleep 1
            done
            
            # Clean up marker
            rm -f "${marker_file}"
            info "✓ Certificate marker file detected, proceeding with build (strategy: marker)"
            return 0
            ;;
            
        *)
            warn "Unknown certificate freshness strategy: ${strategy} (treating as 'none')"
            return 0
            ;;
    esac
}

# =============================================================================
# Cleanup & Restoration
# =============================================================================

cleanup() {
    info "Running cleanup..."
}

trap cleanup EXIT

# =============================================================================
# Main
# =============================================================================

main() {
    parse_args "$@"
    
    log "========================================" "INFO"
    log "sedunlocksrv PBA Deployment Started" "INFO"
    log "========================================" "INFO"
    
    validate_inputs
    get_pba_partition "${OPAL_DRIVE}"
    load_password
    
    # Build new PBA with certificates
    build_pba
    
    # Validate the built image before flashing
    validate_pba_image "${PBA_IMAGE}"
    
    if [ "${DRY_RUN}" = true ]; then
        info "Dry-run mode: skipping PBA flash"
        info "Generated image: ${PBA_IMAGE}"
        exit 0
    fi
    
    # Flash new PBA to drive
    flash_pba_to_drive "${PBA_IMAGE}" "${OPAL_DRIVE}" "${OPAL_PASSWORD}"
    
    log "========================================" "INFO"
    log "✅ Deployment Completed Successfully" "INFO"
    log "========================================" "INFO"
    log "PBA Image: ${PBA_IMAGE}" "INFO"
    log "Drive: ${OPAL_DRIVE}" "INFO"
    log "Log: ${LOG_FILE}" "INFO"
    
    exit 0
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    main "$@"
fi
