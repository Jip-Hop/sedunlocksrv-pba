#!/bin/bash
#
# setup-deploy.sh — Interactive setup helper for deploy.sh environment
#
# Configures service account, sudoers, password storage, and SSH key signing.
# Run as root from the deploy/ directory: sudo ./setup-deploy.sh
# (Automatically detects installation path based on script location)
#
# SSH Key Requirements:
#   - Only Ed25519 keys are supported
#   - ECDSA and RSA keys are NOT supported (ECDSA is non-deterministic; RSA is too slow)
#   - Private key access is required during setup (via agent, file path, or paste)
#   - Only the public key is stored on disk; the private key is never persisted
#
# Private Key Input Methods:
#   1. SSH agent (recommended) — key already loaded, works locally or via ssh -A
#   2. File path — provide path to private key file
#   3. Paste — paste private key into terminal (held in RAM only, never written to disk)
#
# How It Works:
#   The OPAL password is encrypted using AES-256-CBC with a key derived from an SSH
#   signature. A random salt is generated, and the challenge "sedunlocksrv-kdf-{salt}"
#   is signed with the user's Ed25519 private key. The SHA-256 hash of the signature
#   becomes the encryption key. Decryption requires the same private key to reproduce
#   the signature — knowledge of the public key and salt alone is NOT sufficient.

set -eu

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}ℹ${NC} $*"; }
log_warn() { echo -e "${YELLOW}⚠${NC} $*"; }
log_err() { echo -e "${RED}✗${NC} $*"; }
log_ok() { echo -e "${GREEN}✓${NC} $*"; }

# Check if running as root
if [ "${EUID}" -ne 0 ]; then
    log_err "This script must be run as root"
    exit 1
fi

echo "=========================================="
echo "   sedunlocksrv Deploy Setup Helper"
echo "=========================================="
echo ""
log_warn "⚠️  IMPORTANT: This script assumes you have already built at least one PBA image"
log_warn "           using ./build.sh with the desired configuration."
log_warn "           The sedutil-cli version will be checked for compatibility."
echo ""

# =============================================================================
# Configuration Paths (Detect from script location)
# =============================================================================
# Detect the directory where this script is located
SCRIPT_DIR=$(dirname "$(realpath "$0")")
SEDUNLOCKSRV_BASE=$(dirname "$SCRIPT_DIR")
DEPLOY_SCRIPT="${SEDUNLOCKSRV_BASE}/deploy/deploy.sh"

# Validate the base directory structure
if [ ! -f "${DEPLOY_SCRIPT}" ]; then
    log_err "deploy.sh not found at ${DEPLOY_SCRIPT}"
    log_err "This script must be run from the deploy/ directory of the sedunlocksrv repository"
    exit 1
fi

# =============================================================================
# Check for Required Tools (setup-deploy and deploy)
# =============================================================================
echo "Checking for required tools..."
echo "-----"

# First, load build.conf to determine sedutil version compatibility
BUILD_CONF="${SEDUNLOCKSRV_BASE}/build.conf"
SEDUTIL_FORK=""  # Will be set by build.conf or remain empty for Drive-Trust-Alliance default

if [ -f "${BUILD_CONF}" ]; then
    log_info "Loading build.conf to check sedutil version..."
    # Source only SEDUTIL_FORK variable from build.conf
    # Use a subshell to avoid sourcing the entire script
    SEDUTIL_FORK=$(grep "^SEDUTIL_FORK=" "${BUILD_CONF}" 2>/dev/null | cut -d'=' -f2 | tr -d '"' || true)
    
    if [ -n "${SEDUTIL_FORK}" ]; then
        log_info "Configured sedutil fork: ${SEDUTIL_FORK}"
    else
        log_info "No custom sedutil fork configured; using Drive-Trust-Alliance default"
    fi
else
    log_warn "build.conf not found; assuming Drive-Trust-Alliance default"
fi

# Normalize fork name
SEDUTIL_FORK=$(echo "${SEDUTIL_FORK}" | tr '[:upper:]' '[:lower:]')

# Determine sedutil version based on fork
case "${SEDUTIL_FORK}" in
    chubbyant)
        EXPECTED_SEDUTIL_VERSION="1.15-5ad84d8"
        SEDUTIL_SOURCE="ChubbyAnt fork"
        ;;
    *)
        EXPECTED_SEDUTIL_VERSION="master(Drive-Trust-Alliance)"
        SEDUTIL_SOURCE="Drive-Trust-Alliance (default)"
        ;;
esac

log_info "Expected sedutil-cli: ${EXPECTED_SEDUTIL_VERSION} from ${SEDUTIL_SOURCE}"
echo ""

# =============================================================================
# Function to download and install sedutil-cli from the correct fork
# =============================================================================
install_sedutil_cli_from_fork() {
    local fork="$1"
    local temp_dir
    temp_dir=$(mktemp -d)
    trap "rm -rf ${temp_dir}" EXIT
    
    log_info "Downloading sedutil-cli from ${SEDUTIL_SOURCE}..."
    
    if [ "$fork" = "chubbyant" ]; then
        # ChubbyAnt fork
        local url="https://github.com/ChubbyAnt/sedutil/releases/download/1.15-5ad84d8/sedutil-cli-1.15-5ad84d8.zip"
        local archive="${temp_dir}/sedutil-cli-1.15-5ad84d8.zip"
        
        if ! curl -fsSL -o "${archive}" "${url}"; then
            log_err "Failed to download sedutil-cli from ${url}"
            return 1
        fi
        
        # Extract and find the binary
        if ! unzip -q "${archive}" -d "${temp_dir}"; then
            log_err "Failed to extract sedutil-cli archive"
            return 1
        fi
        
        # Find sedutil-cli binary in the extracted files
        local sedutil_bin
        sedutil_bin=$(find "${temp_dir}" -name "sedutil-cli" -type f 2>/dev/null | head -1)
        
        if [ -z "$sedutil_bin" ]; then
            log_err "sedutil-cli binary not found in archive"
            return 1
        fi
        
    else
        # Drive-Trust-Alliance (default)
        local url="https://raw.githubusercontent.com/Drive-Trust-Alliance/exec/master/sedutil_LINUX.tgz"
        local archive="${temp_dir}/sedutil_LINUX.tgz"
        
        if ! curl -fsSL -o "${archive}" "${url}"; then
            log_err "Failed to download sedutil-cli from ${url}"
            return 1
        fi
        
        # Extract and find the binary
        if ! tar -xzf "${archive}" -C "${temp_dir}"; then
            log_err "Failed to extract sedutil-cli archive"
            return 1
        fi
        
        # Find sedutil-cli binary in the expected location
        local sedutil_bin="${temp_dir}/sedutil/Release_x86_64/sedutil-cli"
        
        if [ ! -f "$sedutil_bin" ]; then
            log_err "sedutil-cli binary not found in archive at: $sedutil_bin"
            return 1
        fi
    fi
    
    # Make executable
    chmod +x "${sedutil_bin}"
    
    # Install to /usr/local/bin (higher priority in PATH)
    if ! cp "${sedutil_bin}" /usr/local/bin/sedutil-cli; then
        log_err "Failed to install sedutil-cli to /usr/local/bin/"
        return 1
    fi
    
    chmod +x /usr/local/bin/sedutil-cli
    log_ok "sedutil-cli installed to /usr/local/bin/sedutil-cli"
    
    # Verify it works
    if ! /usr/local/bin/sedutil-cli --version &>/dev/null; then
        log_warn "sedutil-cli installed but --version check failed (this may be normal)"
    fi
    
    return 0
}

declare -a REQUIRED_TOOLS=(
    # Essential for both scripts
    "openssl:openssl"
    "ssh-keygen:openssh-client"
    "jq:jq"
    "curl:curl"
    
    # Essential for deploy.sh (note: sedutil-cli handled separately below)
    "dd:coreutils"
    "blockdev:util-linux"
    "lsblk:util-linux"
    "file:file"
    
    # Essential for build.sh
    "tar:tar"
    "gzip:gzip"
    "mktemp:coreutils"
    
    # Required for mount operations
    "losetup:util-linux"
    "mount:util-linux"
    "umount:util-linux"
    
    # Additional tools for sedutil-cli installation
    "unzip:unzip"
    
    # Text processing (usually present)
    "grep:grep"
    "sed:sed"
    "awk:gawk"
    "cut:coreutils"
)

declare -a MISSING_TOOLS=()

for tool_entry in "${REQUIRED_TOOLS[@]}"; do
    IFS=':' read -r tool_cmd tool_pkg <<< "$tool_entry"
    
    if ! command -v "$tool_cmd" &>/dev/null; then
        MISSING_TOOLS+=("$tool_cmd:$tool_pkg")
        log_warn "Missing tool: $tool_cmd (from package: $tool_pkg)"
    else
        log_ok "Found: $tool_cmd"
    fi
done

if [ ${#MISSING_TOOLS[@]} -gt 0 ]; then
    echo ""
    log_warn "Some tools are missing. Attempting to install..."
    echo ""
    
    # Detect package manager
    if command -v apt-get &>/dev/null; then
        PKG_MANAGER="apt-get"
        PKG_UPDATE="apt-get update"
        PKG_INSTALL="apt-get install -y"
    elif command -v yum &>/dev/null; then
        PKG_MANAGER="yum"
        PKG_UPDATE="yum check-update"
        PKG_INSTALL="yum install -y"
    elif command -v apk &>/dev/null; then
        PKG_MANAGER="apk"
        PKG_UPDATE="apk update"
        PKG_INSTALL="apk add"
    elif command -v pacman &>/dev/null; then
        PKG_MANAGER="pacman"
        PKG_UPDATE="pacman -Sy"
        PKG_INSTALL="pacman -S --noconfirm"
    else
        log_err "No supported package manager found (apt, yum, apk, pacman)"
        log_err "Please manually install: ${MISSING_TOOLS[*]}"
        exit 1
    fi
    
    log_info "Using package manager: $PKG_MANAGER"
    
    # Update package index
    log_info "Updating package index..."
    if ! $PKG_UPDATE &>/dev/null; then
        log_warn "Failed to update package index (continuing anyway)"
    fi
    
    # Install missing tools
    declare -a PACKAGES_TO_INSTALL=()
    for tool_entry in "${MISSING_TOOLS[@]}"; do
        IFS=':' read -r tool_cmd tool_pkg <<< "$tool_entry"
        PACKAGES_TO_INSTALL+=("$tool_pkg")
    done
    
    log_info "Installing packages: ${PACKAGES_TO_INSTALL[*]}"
    if $PKG_INSTALL "${PACKAGES_TO_INSTALL[@]}" 2>&1 | tail -5; then
        log_ok "Packages installed successfully"
    else
        log_err "Failed to install some packages"
        log_err "Please manually install: ${MISSING_TOOLS[*]}"
        exit 1
    fi
    
    # Verify tools were installed
    echo ""
    log_info "Verifying installation..."
    declare -a STILL_MISSING=()
    
    for tool_entry in "${MISSING_TOOLS[@]}"; do
        IFS=':' read -r tool_cmd tool_pkg <<< "$tool_entry"
        if ! command -v "$tool_cmd" &>/dev/null; then
            STILL_MISSING+=("$tool_cmd")
            log_err "Still missing: $tool_cmd"
        else
            log_ok "Now available: $tool_cmd"
        fi
    done
    
    if [ ${#STILL_MISSING[@]} -gt 0 ]; then
        log_err "Failed to install: ${STILL_MISSING[*]}"
        exit 1
    fi
fi

echo ""

# =============================================================================
# Validate sedutil-cli Version Compatibility
# =============================================================================
# =============================================================================
# Validate and Install sedutil-cli (handled separately due to custom download)
# =============================================================================
# sedutil versions are NOT compatible with each other. Verify the installed
# version matches what was used to build the PBA image.
echo "Checking sedutil-cli..."
echo "-----"

log_info "Expected sedutil-cli: ${EXPECTED_SEDUTIL_VERSION} from ${SEDUTIL_SOURCE}"

if ! command -v sedutil-cli &>/dev/null; then
    # sedutil-cli not found, download and install from the correct fork
    log_warn "sedutil-cli not found. Downloading from ${SEDUTIL_SOURCE}..."
    
    if install_sedutil_cli_from_fork "${SEDUTIL_FORK}"; then
        log_ok "sedutil-cli installed successfully"
    else
        log_err "Failed to install sedutil-cli"
        log_err "Tried downloading from: ${SEDUTIL_SOURCE}"
        log_err "Make sure curl and unzip/tar are available, and check your internet connection"
        exit 1
    fi
fi

# Verify sedutil-cli is now available
if command -v sedutil-cli &>/dev/null; then
    INSTALLED_SEDUTIL_VERSION=$(sedutil-cli --version 2>&1 | head -1 || echo "unknown")
    log_info "Installed sedutil-cli: ${INSTALLED_SEDUTIL_VERSION}"
    
    # Version check strategy depends on which fork is used
    if [ "${SEDUTIL_FORK}" = "chubbyant" ]; then
        # For ChubbyAnt, check for the specific version string
        if echo "${INSTALLED_SEDUTIL_VERSION}" | grep -q "1.15-5ad84d8"; then
            log_ok "sedutil-cli version matches: ChubbyAnt 1.15-5ad84d8"
        else
            log_warn "sedutil-cli version mismatch detected!"
            log_warn "  Expected: ChubbyAnt fork (1.15-5ad84d8)"
            log_warn "  Found: ${INSTALLED_SEDUTIL_VERSION}"
            log_warn ""
            read -p "Do you want to proceed anyway? (y/N): " -r proceed_mismatch
            if [[ ! "$proceed_mismatch" =~ ^[Yy]$ ]]; then
                log_err "Setup aborted due to version mismatch."
                log_err "To use ChubbyAnt fork, rebuild the PBA with: sudo ./build.sh --sedutil-fork=ChubbyAnt ..."
                exit 1
            fi
        fi
    else
        # For Drive-Trust-Alliance (default), just verify it's installed
        # Exact version is harder to detect from master branch
        log_ok "sedutil-cli installed (Drive-Trust-Alliance default)"
        log_warn "Note: Verify this matches the version used to build your PBA image"
    fi
else
    # This shouldn't happen, but safeguard
    log_err "sedutil-cli still not found after installation"
    exit 1
fi

echo ""

# =============================================================================
# Service Account Configuration (Allow User Customization)
# =============================================================================
echo "Service Account Configuration"
echo "-----"

read -p "Enter service account name (default: _sedunlocksrv): " -r SERVICE_USER
SERVICE_USER="${SERVICE_USER:-_sedunlocksrv}"
log_ok "Using service account: ${SERVICE_USER}"

echo ""

# =============================================================================
# 1. Service Account & Sudo Configuration
# =============================================================================
echo "Step 1: Service Account & Sudo Configuration"
echo "-----"

if id "${SERVICE_USER}" &>/dev/null 2>&1; then
    log_ok "Service user ${SERVICE_USER} already exists"
else
    log_info "Creating service user ${SERVICE_USER}..."
    useradd -r -s /bin/bash -d "${SEDUNLOCKSRV_BASE}" -m "${SERVICE_USER}"
    log_ok "Created service user"
fi

log_info "Setting up directory structure..."
mkdir -p "${SEDUNLOCKSRV_BASE}"/{certs/latest,logs}
chown -R "${SERVICE_USER}:${SERVICE_USER}" "${SEDUNLOCKSRV_BASE}"
chmod 750 "${SEDUNLOCKSRV_BASE}"
chmod 700 "${SEDUNLOCKSRV_BASE}/certs"
chmod 700 "${SEDUNLOCKSRV_BASE}/logs"
log_ok "Directory structure created"

log_info "Configuring sudoers..."
SUDOERS_FILE="/etc/sudoers.d/sedunlocksrv"

if [ -f "${SUDOERS_FILE}" ]; then
    log_ok "Sudoers file already exists"
else
    log_info "Creating sudoers configuration..."
    cat > "${SUDOERS_FILE}" <<EOF
# Allow ${SERVICE_USER} to run deploy.sh with minimal privilege escalation
${SERVICE_USER} ALL=(ALL) NOPASSWD: ${DEPLOY_SCRIPT}, \\
                                  ${SEDUNLOCKSRV_BASE}/build.sh, \\
                                  /usr/bin/sedutil-cli

# Service user configuration
Defaults:${SERVICE_USER} !requiretty

# Reset environment after command
Defaults:${SERVICE_USER} env_reset
EOF
    chmod 440 "${SUDOERS_FILE}"
    log_ok "Sudoers file created"
fi

if ! visudo -c &>/dev/null; then
    log_err "Sudoers syntax error!"
    exit 1
fi
log_ok "Sudoers validation passed"

echo ""

# =============================================================================
# 2. SSH Key Signing Setup (Ed25519 Only)
# =============================================================================
echo "Step 2: SSH Key Signing Setup"
echo "-----"

log_info "This deployment encrypts the OPAL password using a key derived from an SSH signature."
log_info "Only Ed25519 SSH keys are supported. Other key types will be rejected."
log_info "The private key is needed during setup (via agent, file path, or paste)."
log_info "Only the public key is stored on disk; the private key is never persisted."
log_info ""

# Create .ssh directory if needed
SSH_DIR="${SEDUNLOCKSRV_BASE}/.ssh"
mkdir -p "${SSH_DIR}"
chmod 700 "${SSH_DIR}"
chown root:root "${SSH_DIR}"

# Extract available SSH public keys (from authorized_keys or *.pub files)
log_info "Searching for SSH public keys..."
echo ""

declare -a FOUND_PUBKEYS=()
declare -a PUBKEY_SOURCES=()  # Track where each key came from
declare -a PUBKEY_CONTENT=()  # Store the actual public key content

# Determine the home directory of the service account — that is the correct
# place to look for SSH keys, since deploy.sh runs as SERVICE_USER.
REAL_USER="${SERVICE_USER}"
REAL_HOME=$(getent passwd "${SERVICE_USER}" | cut -d: -f6)
if [ -z "${REAL_HOME}" ]; then
    # Service user not yet created or has no home; fall back to invoking user.
    if [ -n "${SUDO_USER:-}" ]; then
        REAL_USER="${SUDO_USER}"
        REAL_HOME=$(getent passwd "${SUDO_USER}" | cut -d: -f6)
    else
        REAL_USER="$(whoami)"
        REAL_HOME="${HOME}"
    fi
fi

# Strategy 1: Extract Ed25519 keys from the invoking user's authorized_keys.
_auth_keys="${REAL_HOME}/.ssh/authorized_keys"
if [ -f "${_auth_keys}" ]; then
    line_num=0
    while IFS= read -r line; do
        line_num=$((line_num + 1))
        [[ "$line" =~ ^[[:space:]]*# ]] && continue
        [[ -z "$line" ]] && continue
        key_type=$(echo "$line" | awk '{print $1}')
        [[ "${key_type}" != "ssh-ed25519" ]] && continue
        identifier=$(echo "$line" | awk 'NF > 2 {print $(NF)}')
        FOUND_PUBKEYS+=("${_auth_keys}")
        if [ -n "$identifier" ]; then
            PUBKEY_SOURCES+=("${REAL_USER} authorized_keys (line $line_num) - $identifier")
        else
            PUBKEY_SOURCES+=("${REAL_USER} authorized_keys (line $line_num)")
        fi
        PUBKEY_CONTENT+=("$line")
    done < "${_auth_keys}"
fi

# Strategy 2: Look for Ed25519 .pub files in the invoking user's ~/.ssh.
while IFS= read -r -d '' file; do
    if [ -f "$file" ]; then
        pub_key_type=$(awk '{print $1}' "$file")
        [[ "${pub_key_type}" != "ssh-ed25519" ]] && continue
        pub_content=$(cat "$file")
        # Avoid duplicating a key already found in authorized_keys.
        already=false
        for existing in "${PUBKEY_CONTENT[@]+"${PUBKEY_CONTENT[@]}"}"; do
            [[ "$existing" == "$pub_content" ]] && { already=true; break; }
        done
        $already && continue
        FOUND_PUBKEYS+=("$file")
        PUBKEY_SOURCES+=("${REAL_USER} ~/.ssh/$(basename "$file")")
        PUBKEY_CONTENT+=("$pub_content")
    fi
done < <(find "${REAL_HOME}/.ssh" -maxdepth 1 -name "*.pub" -type f -print0 2>/dev/null)

SSH_PUBKEY_PATH=""
SSH_PUBKEY_CONTENT=""
SSH_SOURCE=""

if [ ${#FOUND_PUBKEYS[@]} -eq 0 ]; then
    # No Ed25519 keys found, ask for manual path
    log_warn "No Ed25519 SSH public keys found in ${REAL_HOME}/.ssh/"
    echo ""
    read -p "Enter path to SSH public key file (e.g., ~/.ssh/id_ed25519.pub): " -r SSH_PUBKEY_PATH
    SSH_PUBKEY_PATH="${SSH_PUBKEY_PATH/#\~/$HOME}"
    
    if [ ! -f "${SSH_PUBKEY_PATH}" ]; then
        log_err "File not found: ${SSH_PUBKEY_PATH}"
        exit 1
    fi
    SSH_PUBKEY_CONTENT=$(cat "${SSH_PUBKEY_PATH}")
    SSH_SOURCE="Manual path: $(basename "$SSH_PUBKEY_PATH")"
    
elif [ ${#FOUND_PUBKEYS[@]} -eq 1 ]; then
    # One key found, use as default with confirmation
    log_ok "Found 1 SSH public key: ${PUBKEY_SOURCES[0]}"
    echo ""
    read -p "Use this key? (Y/n): " -r use_default
    
    if [ -z "$use_default" ] || [[ "$use_default" =~ ^[Yy]$ ]]; then
        SSH_PUBKEY_PATH="${FOUND_PUBKEYS[0]}"
        SSH_PUBKEY_CONTENT="${PUBKEY_CONTENT[0]}"
        SSH_SOURCE="${PUBKEY_SOURCES[0]}"
    else
        read -p "Enter path to different SSH public key: " -r SSH_PUBKEY_PATH
        SSH_PUBKEY_PATH="${SSH_PUBKEY_PATH/#\~/$HOME}"
        if [ ! -f "${SSH_PUBKEY_PATH}" ]; then
            log_err "File not found: ${SSH_PUBKEY_PATH}"
            exit 1
        fi
        SSH_PUBKEY_CONTENT=$(cat "${SSH_PUBKEY_PATH}")
        SSH_SOURCE="Manual path: $(basename "$SSH_PUBKEY_PATH")"
    fi
    
else
    # Multiple keys found, show menu
    log_ok "Found ${#FOUND_PUBKEYS[@]} SSH public keys:"
    echo ""
    
    i=1
    for source in "${PUBKEY_SOURCES[@]}"; do
        echo "  $i) $source"
        i=$((i + 1))
    done
    echo "  $i) Enter custom path"
    echo ""
    
    read -p "Select SSH public key (1-$i): " -r choice
    
    if [ "$choice" -eq "$i" ] 2>/dev/null; then
        # Custom path selected
        read -p "Enter path to SSH public key: " -r SSH_PUBKEY_PATH
        SSH_PUBKEY_PATH="${SSH_PUBKEY_PATH/#\~/$HOME}"
        if [ ! -f "${SSH_PUBKEY_PATH}" ]; then
            log_err "File not found: ${SSH_PUBKEY_PATH}"
            exit 1
        fi
        SSH_PUBKEY_CONTENT=$(cat "${SSH_PUBKEY_PATH}")
        SSH_SOURCE="Manual path: $(basename "$SSH_PUBKEY_PATH")"
    elif [ "$choice" -ge 1 ] && [ "$choice" -lt "$i" ] 2>/dev/null; then
        # Menu selection
        SSH_PUBKEY_PATH="${FOUND_PUBKEYS[$((choice - 1))]}"
        SSH_PUBKEY_CONTENT="${PUBKEY_CONTENT[$((choice - 1))]}"
        SSH_SOURCE="${PUBKEY_SOURCES[$((choice - 1))]}"
    else
        log_err "Invalid selection"
        exit 1
    fi
fi

# Validate SSH public key and extract fingerprint
if [ -z "${SSH_PUBKEY_CONTENT}" ]; then
    log_err "SSH public key content is empty"
    exit 1
fi

# Validate key type — only Ed25519 is supported for KDF
SSH_KEY_TYPE=$(echo "${SSH_PUBKEY_CONTENT}" | awk '{print $1}')
if [ "${SSH_KEY_TYPE}" != "ssh-ed25519" ]; then
    log_err "Only Ed25519 keys are supported. Got: ${SSH_KEY_TYPE}"
    log_err "ECDSA/RSA signatures cannot be used for deterministic key derivation."
    log_err ""
    log_err "Generate an Ed25519 key:  ssh-keygen -t ed25519"
    log_err "Then re-run setup-deploy.sh with the new key."
    exit 1
fi
log_ok "Key type: Ed25519"

# Extract fingerprint from the public key content (works for both .pub files and authorized_keys entries)
# ssh-keygen can read from stdin using "-f -"
SSH_FINGERPRINT=$(echo "${SSH_PUBKEY_CONTENT}" | ssh-keygen -lf - 2>/dev/null | awk '{print $2}')
if [ -z "${SSH_FINGERPRINT}" ]; then
    log_err "Failed to extract SSH public key fingerprint from: ${SSH_SOURCE}"
    exit 1
fi

log_ok "SSH source: ${SSH_SOURCE}"
log_ok "SSH public key fingerprint: ${SSH_FINGERPRINT}"

# =========================================================================
# Private Key Access for Signing
# =========================================================================
echo ""
log_info "The private key is needed to sign a cryptographic challenge."
log_info "This signature is used to derive the encryption key for the OPAL password."
echo ""

# Detect if SSH agent has the selected key loaded
SIGNING_KEY_FILE=""
SIGNING_CLEANUP="" # Command to run for cleanup

_agent_has_key() {
    local pubkey_b64
    pubkey_b64=$(echo "${SSH_PUBKEY_CONTENT}" | awk '{print $2}')
    ssh-add -L 2>/dev/null | grep -qF "${pubkey_b64}"
}

if _agent_has_key; then
    log_ok "SSH agent has this key loaded"
    echo "  1) Use SSH agent (recommended — private key already available)"
else
    log_info "SSH agent does not have this key loaded (or no agent running)"
    echo "  1) Use SSH agent (not available — key not loaded)"
fi
echo "  2) Provide private key file path"
echo "  3) Paste private key (held in RAM only, never stored to disk)"
echo ""

read -p "Select private key access method (1-3): " -r pk_method

case "${pk_method}" in
    1)
        if ! _agent_has_key; then
            log_err "SSH agent does not have this key loaded."
            log_err "Load it with: ssh-add ~/.ssh/id_ed25519"
            log_err "Or for remote sessions, connect with: ssh -A ..."
            exit 1
        fi
        # Write pubkey to a temp file — ssh-keygen -Y sign uses agent when given a pubkey
        SIGNING_KEY_FILE="${SSH_DIR}/signing-key.pub"
        echo "${SSH_PUBKEY_CONTENT}" > "${SIGNING_KEY_FILE}"
        chmod 600 "${SIGNING_KEY_FILE}"
        log_ok "Using SSH agent for signing"
        ;;
    2)
        read -p "Enter path to SSH private key: " -r PRIVATE_KEY_PATH
        PRIVATE_KEY_PATH="${PRIVATE_KEY_PATH/#\~/$HOME}"
        if [ ! -f "${PRIVATE_KEY_PATH}" ]; then
            log_err "Private key not found: ${PRIVATE_KEY_PATH}"
            exit 1
        fi
        SIGNING_KEY_FILE="${PRIVATE_KEY_PATH}"
        log_ok "Using private key file: ${PRIVATE_KEY_PATH}"
        ;;
    3)
        # Check /dev/shm is available (tmpfs — RAM-only, never written to physical disk)
        if [ ! -d /dev/shm ]; then
            log_err "/dev/shm not available. Cannot safely hold private key in RAM."
            log_err "Use method 1 (agent) or 2 (file path) instead."
            exit 1
        fi
        echo ""
        log_info "Paste your Ed25519 private key below."
        log_info "(Includes -----BEGIN OPENSSH PRIVATE KEY----- through -----END OPENSSH PRIVATE KEY-----)"
        log_info "Press Ctrl-D on a blank line when done:"
        echo ""
        PRIVKEY_CONTENT=$(cat)
        
        # Write to RAM-only tmpfs (never persisted to physical disk)
        SIGNING_KEY_FILE=$(mktemp /dev/shm/sedunlocksrv-key.XXXXXX)
        chmod 600 "${SIGNING_KEY_FILE}"
        echo "${PRIVKEY_CONTENT}" > "${SIGNING_KEY_FILE}"
        SIGNING_CLEANUP="${SIGNING_KEY_FILE}" # Remember to clean up
        unset PRIVKEY_CONTENT
        log_ok "Private key stored in RAM (/dev/shm) temporarily"
        ;;
    *)
        log_err "Invalid selection"
        exit 1
        ;;
esac

# =========================================================================
# Generate salt and derive encryption key via SSH signature
# =========================================================================
KDF_SALT=$(openssl rand -hex 32)
CHALLENGE="sedunlocksrv-kdf-${KDF_SALT}"

# Sign the challenge with the private key
# For Ed25519: signature is deterministic (same key + same input = same output)
SIGNATURE=$(echo -n "${CHALLENGE}" | ssh-keygen -Y sign -f "${SIGNING_KEY_FILE}" -n sedunlocksrv -q 2>&1) || {
    log_err "Failed to sign KDF challenge with private key."
    log_err "Ensure the private key matches the selected public key."
    # Clean up temp key if needed
    if [ -n "${SIGNING_CLEANUP}" ] && [ -f "${SIGNING_CLEANUP}" ]; then
        dd if=/dev/zero of="${SIGNING_CLEANUP}" bs=$(stat -c%s "${SIGNING_CLEANUP}") count=1 2>/dev/null
        rm -f "${SIGNING_CLEANUP}"
    fi
    exit 1
}

# Clean up pasted private key from RAM immediately after signing
if [ -n "${SIGNING_CLEANUP}" ] && [ -f "${SIGNING_CLEANUP}" ]; then
    dd if=/dev/zero of="${SIGNING_CLEANUP}" bs=$(stat -c%s "${SIGNING_CLEANUP}") count=1 2>/dev/null
    rm -f "${SIGNING_CLEANUP}"
    log_ok "Temporary private key securely removed from RAM"
fi

# Derive encryption key from signature
ENCRYPTION_KEY=$(echo -n "${SIGNATURE}" | sha256sum | awk '{print $1}')
log_ok "Encryption key derived from SSH signature"

# Prompt for OPAL password
echo ""
read -sp "Enter OPAL admin password: " OPAL_PASSWORD
echo
read -sp "Confirm OPAL admin password: " OPAL_PASSWORD_CONFIRM
echo

if [ "${OPAL_PASSWORD}" != "${OPAL_PASSWORD_CONFIRM}" ]; then
    log_err "Passwords do not match"
    exit 1
fi

# Encrypt password with derived key
PASSWORD_FILE="${SSH_DIR}/opal-password.enc"
AUTH_CONF="${SSH_DIR}/auth.conf"

echo -n "${OPAL_PASSWORD}" | \
    openssl enc -aes-256-cbc -pbkdf2 -P -pass "pass:${ENCRYPTION_KEY}" -out "${PASSWORD_FILE}" 2>/dev/null || {
    log_err "Failed to encrypt password"
    exit 1
}

chmod 600 "${PASSWORD_FILE}"
chown root:root "${PASSWORD_FILE}"

log_ok "Password encrypted and stored: ${PASSWORD_FILE}"

# Store signing public key for deploy.sh to use with SSH agent
SIGNING_PUBKEY_FILE="${SSH_DIR}/signing-key.pub"
echo "${SSH_PUBKEY_CONTENT}" > "${SIGNING_PUBKEY_FILE}"
chmod 600 "${SIGNING_PUBKEY_FILE}"
chown root:root "${SIGNING_PUBKEY_FILE}"
log_ok "Signing public key stored: ${SIGNING_PUBKEY_FILE}"

# Store authentication metadata
cat > "${AUTH_CONF}" <<EOF
# SSH Key Signing Configuration
# Generated by setup-deploy.sh
# The KDF_SALT and public key are NOT sufficient to decrypt the password.
# The corresponding Ed25519 private key is required to reproduce the signature.

KDF_SALT="${KDF_SALT}"
SSH_PUBKEY_FINGERPRINT="${SSH_FINGERPRINT}"
SSH_KEY_TYPE="${SSH_KEY_TYPE}"
CREATED=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
EOF

chmod 644 "${AUTH_CONF}"
chown root:root "${AUTH_CONF}"

log_ok "Authentication configuration stored: ${AUTH_CONF}"

# Clear sensitive variables
OPAL_PASSWORD=""
OPAL_PASSWORD_CONFIRM=""
ENCRYPTION_KEY=""
SIGNATURE=""
KDF_SALT=""

echo ""

# =============================================================================
# 3. Verify Installation
# =============================================================================
echo "Step 3: Verification"
echo "-----"

ERRORS=0

# Check deploy.sh exists
if [ -f "${DEPLOY_SCRIPT}" ]; then
    log_ok "deploy.sh found"
else
    log_err "deploy.sh not found at ${DEPLOY_SCRIPT}"
    ERRORS=$((ERRORS + 1))
fi

# Check service user
if id "${SERVICE_USER}" &>/dev/null; then
    log_ok "Service user ${SERVICE_USER} exists"
else
    log_err "Service user ${SERVICE_USER} not found"
    ERRORS=$((ERRORS + 1))
fi

# Check sudoers
if sudo -l -U "${SERVICE_USER}" deploy.sh &>/dev/null 2>&1; then
    log_ok "Sudoers configuration valid"
else
    log_warn "Sudoers may not be configured correctly (this is normal)"
fi

# Check directories
for dir in "${SEDUNLOCKSRV_BASE}"{,/certs,/certs/latest,/logs,.ssh}; do
    if [ -d "${dir}" ]; then
        log_ok "Directory exists: ${dir}"
    else
        log_err "Directory missing: ${dir}"
        ERRORS=$((ERRORS + 1))
    fi
done

# Check SSH encryption files
if [ -f "${SSH_DIR}/opal-password.enc" ]; then
    log_ok "SSH encrypted password file exists"
else
    log_err "SSH encrypted password file not found"
    ERRORS=$((ERRORS + 1))
fi

if [ -f "${SSH_DIR}/auth.conf" ]; then
    log_ok "SSH authentication config exists"
else
    log_err "SSH authentication config not found"
    ERRORS=$((ERRORS + 1))
fi

if [ -f "${SSH_DIR}/signing-key.pub" ]; then
    log_ok "Signing public key exists"
else
    log_err "Signing public key not found"
    ERRORS=$((ERRORS + 1))
fi

# Tools were already verified at script start
log_ok "All required tools are available (verified at script startup)"

echo ""

# =============================================================================
# Summary
# =============================================================================
if [ "${ERRORS}" -eq 0 ]; then
    log_ok "Setup completed successfully!"
    echo ""
    echo "Next steps:"
    echo "  1. Deploy files are in: ${SEDUNLOCKSRV_BASE}/deploy/"
    echo "  2. OPAL password encrypted with SSH key signature: ${SSH_FINGERPRINT}"
    echo "  3. From source machine, deploy via SSH (agent forwarding required):"
    echo "     ssh -A -i ~/.ssh/id_ed25519 deploy@target \\"
    echo "       '${DEPLOY_SCRIPT} --cert-path=... --key-path=...'"
    echo ""
    echo "  4. Password is decrypted automatically using the SSH agent's private key"
    echo "  5. To rotate password or SSH key, re-run: sudo ./setup-deploy.sh"
    echo ""
    echo "  NOTE: The SSH session must have agent forwarding enabled (ssh -A) so that"
    echo "        deploy.sh can sign the KDF challenge with the private key."
    echo "        Only Ed25519 keys are supported."
    echo ""
else
    log_err "Setup completed with ${ERRORS} error(s)"
    exit 1
fi
