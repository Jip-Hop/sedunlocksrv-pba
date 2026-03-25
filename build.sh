#!/usr/bin/env bash
#
# sedunlocksrv-pba — build a TinyCore-based Pre-Boot Authentication disk image.
#
# High-level flow:
#   1. --config / build.conf → CLI overrides → 2. Host tools → 3. Compile Go
#   4. Cache ISO & sedutil → 5. Unpack kernel/initrd → 6. Merge app + kexec + TCZs
#   7. Repack initrd → 8. Partition loop image → GRUB → output .img + symlink
#
set -euox pipefail

export PATH="${PATH}:/usr/local/go/bin"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${BUILD_CONFIG:-${SCRIPT_DIR}/build.conf}"

# -----------------------------------------------------------------------------
# Defaults (override via build.conf, then via CLI — CLI wins)
# -----------------------------------------------------------------------------
CACHEDIR="cache"
# Renamed from TMPDIR to avoid shadowing the standard POSIX $TMPDIR variable
# used by mktemp, sort, and other host tools invoked during the build. (Fix #8)
BUILD_TMPDIR="img.tmp"

BUILD_DATE=$(date +%Y%m%d-%H%M)
OUTPUTIMG="sedunlocksrv-pba-${BUILD_DATE}.img"
LATEST_LINK="sedunlocksrv-pba-latest.img"

# Extra MB reserved for bootloader/EFI partition content.
# Final image size is filesystem size + GRUBSIZE + 2MB safety margin.
# OPAL 2 PBA size limit is typically 128MB, so keep this conservative.
GRUBSIZE=32
KEXEC_VER="2.0.28"

TCURL="http://distro.ibiblio.org/tinycorelinux/15.x/x86_64"
INPUTISO="TinyCorePure64-current.iso"
BOOTARGS="libata.allow_tpm=1"

SEDUTILBINFILENAME="sedutil-cli"
EXTENSIONS="jq.tcz"

CLEAN_MODE=false
SSHBUILD=false
KEYMAP=""
EXCLUDE_NETDEV=""
NET_MODE="single"
NET_IFACES=""
NET_DHCP="true"
IP_ADDR=""
NETMASK=""
GATEWAY=""
DNS=""
BOND_MODE="4"
BOND_MIIMON="100"
BOND_LACP_RATE="1"
BOND_XMIT_HASH_POLICY="1"
TLS_CERT_PATH=""
TLS_KEY_PATH=""
SSH_CURL_INSECURE="auto"
EXPERT_PASSWORD=""
EXPERT_PASSWORD_HASH=""

SEDUTIL_FORK=""
SEDUTILURL=""
SEDUTILPATHINTAR=""

TC_KERNEL_VERSION=""
LOOP_DEVICE_HDD=""

REMAINING_ARGS=()

have_command() {
    command -v "$1" >/dev/null 2>&1
}

require_file() {
    local path="$1" description="$2"
    if [ ! -f "${path}" ]; then
        echo "❌ ERROR: ${description} not found: ${path}" >&2
        exit 1
    fi
}

# require_numeric NAME VALUE — exits with an error if VALUE is not a non-negative integer.
# Replaces four identical case blocks in validate_network_settings. (Size reduction)
require_numeric() {
    local name="$1" value="$2"
    case "${value}" in
        ''|*[!0-9]*)
            echo "${name} must be numeric (current: ${value})" >&2
            exit 1
            ;;
    esac
}

# -----------------------------------------------------------------------------
# sedutil upstream (env SEDUTIL_FORK=ChubbyAnt or chubbyant)
# -----------------------------------------------------------------------------
configure_sedutil_source() {
    case "$(echo "${SEDUTIL_FORK-}" | tr '[:upper:]' '[:lower:]')" in
        chubbyant)
            SEDUTIL_FORK="ChubbyAnt"
            SEDUTILURL="https://github.com/ChubbyAnt/sedutil/releases/download/1.15-5ad84d8/sedutil-cli-1.15-5ad84d8.zip"
            SEDUTIL_ARCHIVE_NAME="$(basename "${SEDUTILURL}")"
            ;;
        *)
            SEDUTIL_FORK="Drive-Trust-Alliance"
            SEDUTILURL="https://raw.githubusercontent.com/Drive-Trust-Alliance/exec/master/sedutil_LINUX.tgz"
            SEDUTILPATHINTAR="sedutil/Release_x86_64/${SEDUTILBINFILENAME}"
            SEDUTIL_ARCHIVE_NAME="$(basename "${SEDUTILURL}")"
            ;;
    esac
}

# -----------------------------------------------------------------------------
# Config file (optional) then CLI (CLI overrides file)
# -----------------------------------------------------------------------------
extract_config_path_from_args() {
    REMAINING_ARGS=()
    for arg in "$@"; do
        case "$arg" in
            --config=*) CONFIG_FILE="${arg#*=}" ;;
            *)          REMAINING_ARGS+=("$arg") ;;
        esac
    done
}

load_config_file() {
    if [ ! -f "${CONFIG_FILE}" ]; then
        return 0
    fi
    echo "--- Loading build config: ${CONFIG_FILE} ---"
    # shellcheck disable=SC1090
    source "${CONFIG_FILE}"
}

require_root() {
    if [ "${EUID}" -ne 0 ]; then
        echo "❌ ERROR: build.sh must be run as root." >&2
        echo "Run it with sudo, for example: sudo ./build.sh" >&2
        exit 1
    fi
}

print_usage() {
    echo "Usage: $0 [--config=FILE] [--clean] [--ssh] [--keymap=NAME]" >&2
    echo "          [--bootargs=KERNEL_CMDLINE] [--exclude-netdev=DEVS]" >&2
    echo "          [--net-mode=bond|single] [--net-ifaces=DEVS]" >&2
    echo "          [--net-addressing=dhcp|static] [--ip-addr=ADDR] [--netmask=MASK]" >&2
    echo "          [--bond-mode=MODE] [--bond-miimon=MS] [--bond-lacp-rate=VAL]" >&2
    echo "          [--bond-xmit-hash-policy=VAL]" >&2
    echo "          [--tls-cert=PATH] [--tls-key=PATH] [--ssh-curl-insecure=auto|true|false]" >&2
    echo "          [--expert-password=VALUE]  # if omitted, a random 16-digit password is generated" >&2
    echo "          [--gateway=ADDR] [--dns=ADDRS] [--sedutil-fork=ChubbyAnt]" >&2
}

parse_args() {
    for arg in "$@"; do
        case "$arg" in
            --help|-h)               print_usage; exit 0 ;;
            --clean)                 CLEAN_MODE=true ;;
            --ssh)                   SSHBUILD=true ;;
            --keymap=*)              KEYMAP="${arg#*=}" ;;
            --bootargs=*)            BOOTARGS="${arg#*=}" ;;
            --exclude-netdev=*)      EXCLUDE_NETDEV="${arg#*=}" ;;
            --net-mode=*)            NET_MODE="${arg#*=}" ;;
            --net-ifaces=*)          NET_IFACES="${arg#*=}" ;;
            --net-addressing=*)
                case "${arg#*=}" in
                    dhcp)   NET_DHCP="true" ;;
                    static) NET_DHCP="false" ;;
                    *)
                        echo "Unknown network addressing mode: ${arg#*=}" >&2
                        print_usage; exit 1
                        ;;
                esac
                ;;
            --ip-addr=*)             IP_ADDR="${arg#*=}" ;;
            --netmask=*)             NETMASK="${arg#*=}" ;;
            --gateway=*)             GATEWAY="${arg#*=}" ;;
            --dns=*)                 DNS="${arg#*=}" ;;
            --tls-cert=*)            TLS_CERT_PATH="${arg#*=}" ;;
            --tls-key=*)             TLS_KEY_PATH="${arg#*=}" ;;
            --ssh-curl-insecure=*)   SSH_CURL_INSECURE="${arg#*=}" ;;
            --expert-password=*)     EXPERT_PASSWORD="${arg#*=}" ;;
            --bond-mode=*)           BOND_MODE="${arg#*=}" ;;
            --bond-miimon=*)         BOND_MIIMON="${arg#*=}" ;;
            --bond-lacp-rate=*)      BOND_LACP_RATE="${arg#*=}" ;;
            --bond-xmit-hash-policy=*) BOND_XMIT_HASH_POLICY="${arg#*=}" ;;
            --sedutil-fork=*)        SEDUTIL_FORK="${arg#*=}" ;;
            *)
                echo "Unknown option: $arg" >&2
                print_usage; exit 1
                ;;
        esac
    done
}

validate_network_settings() {
    case "${NET_MODE}" in
        bond|single) ;;
        *) echo "Unknown network mode: ${NET_MODE}" >&2; print_usage; exit 1 ;;
    esac

    case "${NET_DHCP}" in
        true|false) ;;
        *) echo "NET_DHCP must be true or false (current: ${NET_DHCP})" >&2; exit 1 ;;
    esac

    if [ "${NET_DHCP}" = "false" ] && [ -z "${IP_ADDR}" -o -z "${NETMASK}" ]; then
        echo "Static networking requires IP_ADDR and NETMASK." >&2; exit 1
    fi

    # Four identical numeric-validation blocks replaced by require_numeric. (Size reduction)
    require_numeric "BOND_MODE"             "${BOND_MODE}"
    require_numeric "BOND_MIIMON"           "${BOND_MIIMON}"
    require_numeric "BOND_LACP_RATE"        "${BOND_LACP_RATE}"
    require_numeric "BOND_XMIT_HASH_POLICY" "${BOND_XMIT_HASH_POLICY}"

    if { [ -n "${TLS_CERT_PATH}" ] && [ -z "${TLS_KEY_PATH}" ]; } || \
       { [ -z "${TLS_CERT_PATH}" ] && [ -n "${TLS_KEY_PATH}" ]; }; then
        echo "TLS_CERT_PATH and TLS_KEY_PATH must be set together." >&2; exit 1
    fi

    [ -n "${TLS_CERT_PATH}" ] && require_file "${TLS_CERT_PATH}" "TLS cert file"
    [ -n "${TLS_KEY_PATH}"  ] && require_file "${TLS_KEY_PATH}"  "TLS key file"

    case "${SSH_CURL_INSECURE}" in
        auto|true|false) ;;
        *) echo "SSH_CURL_INSECURE must be auto, true, or false (current: ${SSH_CURL_INSECURE})" >&2; exit 1 ;;
    esac

}

apply_extension_flags() {
    if [ "$SSHBUILD" = true ]; then
        EXTENSIONS="${EXTENSIONS} dropbear.tcz"
    fi
    if [ -n "${KEYMAP:-}" ]; then
        EXTENSIONS="${EXTENSIONS} kmaps.tcz"
    fi
}

clean_workspace_artifacts() {
    rm -rf "${CACHEDIR}" "${BUILD_TMPDIR}"

    # Build outputs and links
    rm -f sedunlocksrv-pba-*.img "${LATEST_LINK}"

    # Generated TLS and app binary
    rm -f sedunlocksrv/server.crt sedunlocksrv/server.key sedunlocksrv/sedunlocksrv

    # Generated SSH artifacts
    rm -f ssh/dropbear_ecdsa_host_key ssh/dropbear_rsa_host_key ssh/authorized_keys

    # Local build override file
    rm -f build.conf
}

maybe_clean_workspace() {
    if [ "$CLEAN_MODE" != true ]; then
        return 0
    fi
    echo "🧹 Cleaning workspace artifacts to repo-like state..."
    clean_workspace_artifacts
}

# -----------------------------------------------------------------------------
# Cleanup on exit (trap).
# -----------------------------------------------------------------------------
cleanup() {
    echo "Cleaning up..."
    umount "${BUILD_TMPDIR-}/img" 2>/dev/null || true
    if [ -n "${LOOP_DEVICE_HDD-}" ]; then
        losetup -d "${LOOP_DEVICE_HDD}" 2>/dev/null || true
    fi
    rm -rf "${BUILD_TMPDIR-}"
}

# -----------------------------------------------------------------------------
# Host dependency check
# -----------------------------------------------------------------------------
check_host_dependencies() {
    echo "--- Checking build dependencies ---"
    local REQUIRED_TOOLS="
        gcc make curl tar xorriso bsdtar go cpio xz sfdisk
        sed awk cut tr date chown chmod
        basename mktemp find realpath zcat rsync nproc sort cat head tail
        od
        touch mkdir cp rm mv mount umount chroot
        du dd losetup lsblk mknod mkfs.fat
        grub-install sync sleep ln env openssl
    "
    local missing="" tool
    for tool in ${REQUIRED_TOOLS}; do
        if ! have_command "$tool"; then
            missing="${missing} ${tool}"
        fi
    done
    if [ -n "${missing}" ]; then
        echo "❌ ERROR: Missing required build tools:${missing}"
        echo "On Debian/Ubuntu:"
        echo "  sudo apt update && sudo apt install -y \\"
        echo "    build-essential coreutils findutils curl xorriso bsdtar cpio xz-utils fdisk \\"
        echo "    golang-go gzip rsync util-linux dosfstools openssl \\"
        echo "    grub-common grub-pc-bin grub-efi-amd64-bin grub-efi-ia32-bin"
        exit 1
    fi
    if [ "$SSHBUILD" = true ] && ! have_command dropbearkey; then
        echo "❌ ERROR: dropbearkey required for --ssh (e.g. apt install dropbear-bin)"
        exit 1
    fi
    echo "✅ All required tools present."
}

# -----------------------------------------------------------------------------
# Work directories
# -----------------------------------------------------------------------------
setup_workdirs() {
    mkdir -p "${BUILD_TMPDIR}/fs/boot" "${BUILD_TMPDIR}/core" "${BUILD_TMPDIR}/img"
    mkdir -p "${CACHEDIR}/iso" "${CACHEDIR}/tcz" "${CACHEDIR}/dep" "${CACHEDIR}/iso-extracted"
    mkdir -p "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
}

# -----------------------------------------------------------------------------
# Go: vet + linux/amd64 release binary
# -----------------------------------------------------------------------------
build_sedunlocksrv_go() {
    (
        cd ./sedunlocksrv
        echo "--- Verifying Go toolchain and code ---"
        local go_version maj min
        go_version=$(go version | awk '{print $3}' | sed 's/go//')
        maj=$(echo "${go_version}" | cut -d. -f1)
        min=$(echo "${go_version}" | cut -d. -f2)
        if [ "${maj}" -lt 1 ] || [ "${min}" -lt 21 ]; then
            echo "❌ Go 1.21+ required (found: ${go_version})"
            exit 1
        fi
        [ -f go.mod ] || go mod init sedunlocksrv
        go get golang.org/x/term
        go mod tidy
        if ! go vet ./...; then
            echo "❌ go vet failed"; exit 1
        fi
        if ! go build -o /dev/null .; then
            echo "❌ go build (test compile) failed"; exit 1
        fi
        echo "--- Building sedunlocksrv (linux/amd64) ---"
        if env GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o sedunlocksrv; then
            chown 1001:50 sedunlocksrv
            chmod +x sedunlocksrv
        else
            exit 1
        fi
    )
}

ensure_tls_certs() {
    if [ -n "${TLS_CERT_PATH}" ] && [ -n "${TLS_KEY_PATH}" ]; then
        require_file "${TLS_CERT_PATH}" "TLS cert file"
        require_file "${TLS_KEY_PATH}"  "TLS key file"
        cp -f "${TLS_CERT_PATH}" sedunlocksrv/server.crt
        cp -f "${TLS_KEY_PATH}"  sedunlocksrv/server.key
        chmod 600 sedunlocksrv/server.key
        return 0
    fi
    if [[ -f sedunlocksrv/server.crt && -f sedunlocksrv/server.key ]]; then
        return 0
    fi
    ./make-cert.sh
}

# -----------------------------------------------------------------------------
# fetch_cached_url URL DEST_PATH
# Downloads URL to DEST_PATH only if DEST_PATH does not already exist.
# Replaces duplicated "check cache → curl if missing" logic throughout the
# script. (Size reduction)
# -----------------------------------------------------------------------------
fetch_cached_url() {
    local url="$1" dest="$2"
    if [ -s "${dest}" ]; then
        echo "📦 Using cached: $(basename "${dest}")"
        return 0
    fi
    echo "Fetching: $(basename "${dest}")..."
    curl -fL "${url}" -o "${dest}"
}

# -----------------------------------------------------------------------------
# Tiny Core extension cache helper (.tcz and .dep)
# -----------------------------------------------------------------------------
cachetcfile() {
    local filename="$1" local_dir="$2" remote_type="$3"
    local local_path="${CACHEDIR}/${local_dir}/${filename}"
    if [ -s "${local_path}" ]; then
        echo "📦 Using cached: ${filename}"
        return 0
    fi
    echo "Fetching: ${filename}..."
    curl -fL "${TCURL}/${remote_type}/${filename}" -o "${local_path}" || {
        [[ "${local_dir}" == "dep" ]] && touch "${local_path}"
    }
}

cleanup_mount_dir() {
    local mount_dir="$1"
    umount "${mount_dir}" 2>/dev/null || true
    rm -rf "${mount_dir}"
}

# -----------------------------------------------------------------------------
# ISO: download if needed, extract once into CACHEDIR/iso-extracted
# -----------------------------------------------------------------------------
prepare_tinycore_iso() {
    cachetcfile "${INPUTISO}" iso release
    rm -rf "${CACHEDIR}/iso-extracting"
    mkdir -p "${CACHEDIR}/iso-extracting"
    xorriso -osirrox on \
        -indev "${CACHEDIR}/iso/${INPUTISO}" \
        -extract / "${CACHEDIR}/iso-extracting"
    rm -rf "${CACHEDIR}/iso-extracted"
    mv "${CACHEDIR}/iso-extracting" "${CACHEDIR}/iso-extracted"
}

# -----------------------------------------------------------------------------
# sedutil-cli binary → cache, then copied into initrd later
# -----------------------------------------------------------------------------
fetch_sedutil_cli() {
    case "${SEDUTIL_FORK}" in
        ChubbyAnt)
            local dest zip extract_dir bin
            dest="${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}"
            zip="${CACHEDIR}/sedutil/${SEDUTIL_FORK}/$(basename "${SEDUTILURL}")"
            if [ -s "${dest}" ]; then
                echo "📦 Using cached: ${SEDUTILBINFILENAME} (ChubbyAnt)"
                return 0
            fi
            fetch_cached_url "${SEDUTILURL}" "${zip}"
            extract_dir="$(mktemp -d "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/extract.XXXXXX")"
            bsdtar -xf "${zip}" -C "${extract_dir}"
            bin="$(find "${extract_dir}" -type f -name "${SEDUTILBINFILENAME}" | head -n1)"
            if [ -z "${bin}" ] || [ ! -f "${bin}" ]; then
                echo "❌ ${SEDUTILBINFILENAME} not found in ChubbyAnt archive"
                rm -rf "${extract_dir}"
                exit 1
            fi
            cp -f "${bin}" "${dest}"
            chmod +x "${dest}"
            rm -rf "${extract_dir}"
            ;;
        *)
            local slashes depth archive_path
            slashes="${SEDUTILPATHINTAR//[^\/]/}"
            depth="${#slashes}"
            archive_path="${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTIL_ARCHIVE_NAME}"
            fetch_cached_url "${SEDUTILURL}" "${archive_path}"
            bsdtar -xf "${archive_path}" -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}" \
                --strip-components="${depth}" "${SEDUTILPATHINTAR}"
            ;;
    esac
}

# -----------------------------------------------------------------------------
# Kernel + initrd from ISO
# -----------------------------------------------------------------------------
stage_kernel_and_initrd() {
    local kernel_path core_path
    kernel_path=$(find "${CACHEDIR}/iso-extracted" -type f -name "vmlinuz64" | head -n1 || true)
    if [ -z "${kernel_path}" ] || [ ! -f "${kernel_path}" ]; then
        echo "❌ vmlinuz64 not found in ISO extract"; exit 1
    fi
    echo "✅ Kernel: ${kernel_path}"
    cp "${kernel_path}" "${BUILD_TMPDIR}/fs/boot/vmlinuz64"

    core_path=$(find "${CACHEDIR}/iso-extracted" -type f -name "corepure64.gz" | head -n1 || true)
    if [ -z "${core_path}" ] || [ ! -f "${core_path}" ]; then
        echo "❌ corepure64.gz not found in ISO extract"; exit 1
    fi
    echo "✅ Initrd: ${core_path}"
    if ! (cd "${BUILD_TMPDIR}/core" && zcat "${core_path}" | cpio -id -H newc); then
        echo "❌ Failed to extract initrd from: ${core_path}" >&2
        exit 1
    fi

    TC_KERNEL_VERSION=$(ls "${BUILD_TMPDIR}/core/lib/modules")
    EXTENSIONS="${EXTENSIONS} scsi-${TC_KERNEL_VERSION}.tcz"
}

# -----------------------------------------------------------------------------
# App payload
# -----------------------------------------------------------------------------
quote_sh_value() {
    printf "'%s'" "$(printf "%s" "${1-}" | sed "s/'/'\\\\''/g")"
}

write_runtime_network_config() {
    local effective_ssh_curl_insecure
    case "${SSH_CURL_INSECURE}" in
        auto)
            if [ -n "${TLS_CERT_PATH}" ] && [ -n "${TLS_KEY_PATH}" ]; then
                effective_ssh_curl_insecure="false"
            else
                effective_ssh_curl_insecure="true"
            fi
            ;;
        *) effective_ssh_curl_insecure="${SSH_CURL_INSECURE}" ;;
    esac

    cat > "${BUILD_TMPDIR}/core/etc/sedunlocksrv.conf" <<EOF
NET_MODE=$(quote_sh_value "${NET_MODE}")
NET_IFACES=$(quote_sh_value "${NET_IFACES}")
NET_EXCLUDE=$(quote_sh_value "${EXCLUDE_NETDEV}")
NET_DHCP=$(quote_sh_value "${NET_DHCP}")
IP_ADDR=$(quote_sh_value "${IP_ADDR}")
NETMASK=$(quote_sh_value "${NETMASK}")
GATEWAY=$(quote_sh_value "${GATEWAY}")
DNS=$(quote_sh_value "${DNS}")
BOND_MODE=$(quote_sh_value "${BOND_MODE}")
BOND_MIIMON=$(quote_sh_value "${BOND_MIIMON}")
BOND_LACP_RATE=$(quote_sh_value "${BOND_LACP_RATE}")
BOND_XMIT_HASH_POLICY=$(quote_sh_value "${BOND_XMIT_HASH_POLICY}")
SSH_CURL_INSECURE=$(quote_sh_value "${effective_ssh_curl_insecure}")
EXPERT_PASSWORD_HASH=$(quote_sh_value "${EXPERT_PASSWORD_HASH}")
EOF
}

generate_random_16_digit_password() {
    local digits=""
    while [ "${#digits}" -lt 16 ]; do
        digits="${digits}$(od -An -N8 -tu8 /dev/urandom | tr -cd '0-9')"
    done
    printf '%s' "${digits:0:16}"
}

prepare_expert_password_hash() {
    local had_xtrace=false
    case "$-" in
        *x*) had_xtrace=true; set +x ;;
    esac

    if [ -z "${EXPERT_PASSWORD}" ]; then
        EXPERT_PASSWORD="$(generate_random_16_digit_password)"
        echo "Generated expert password for this build: ${EXPERT_PASSWORD}"
    fi

    EXPERT_PASSWORD_HASH="$(
        cd ./sedunlocksrv
        EXPERT_PASSWORD_INPUT="${EXPERT_PASSWORD}" go run ./cmd/hash-password
    )"
    if [ -z "${EXPERT_PASSWORD_HASH}" ]; then
        echo "Failed to generate EXPERT_PASSWORD_HASH." >&2
        exit 1
    fi

    if [ "${had_xtrace}" = true ]; then
        set -x
    fi
}

populate_initrd_application_tree() {
    mkdir -p "${BUILD_TMPDIR}/core/usr/local/sbin/"
    cp "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}" "${BUILD_TMPDIR}/core/usr/local/sbin/"
    rsync -avr \
        --exclude='sedunlocksrv/main.go' \
        --exclude='sedunlocksrv/go.mod' \
        'sedunlocksrv' "${BUILD_TMPDIR}/core/usr/local/sbin/"
    mkdir -p "${BUILD_TMPDIR}/core/usr/local/sbin/sedunlocksrv/static"
    cp ./sedunlocksrv/index.html "${BUILD_TMPDIR}/core/usr/local/sbin/sedunlocksrv/static/index.html"
    cp ./tc/tc-config "${BUILD_TMPDIR}/core/etc/init.d/tc-config"
    write_runtime_network_config
}

install_bonding_module_if_needed() {
    local tcz_name mount_dir bonding_module dest_dir
    [ "${NET_MODE}" = "bond" ] || return 0

    tcz_name="ipv6-netfilter-${TC_KERNEL_VERSION}.tcz"
    cachetcfile "${tcz_name}" tcz tcz

    mount_dir="$(mktemp -d --tmpdir="$(pwd)" 'mnt.XXXXXX')"
    mount -o loop "${CACHEDIR}/tcz/${tcz_name}" "${mount_dir}"
    bonding_module="$(find "${mount_dir}" -path "*/kernel/drivers/net/bonding/bonding.ko*" | head -n1)"
    if [ -z "${bonding_module}" ] || [ ! -f "${bonding_module}" ]; then
        cleanup_mount_dir "${mount_dir}"
        echo "❌ bonding.ko not found in ${tcz_name}"; exit 1
    fi
    dest_dir="${BUILD_TMPDIR}/core/lib/modules/${TC_KERNEL_VERSION}/kernel/drivers/net/bonding"
    mkdir -p "${dest_dir}"
    cp -f "${bonding_module}" "${dest_dir}/"
    cleanup_mount_dir "${mount_dir}"
}

# -----------------------------------------------------------------------------
# kexec static binary — cached in CACHEDIR so it is only compiled once.
# The binary is copied into BUILD_TMPDIR on every build, but the expensive
# configure+make step is skipped when the cached binary is present. (Fix #11)
# -----------------------------------------------------------------------------
build_kexec_tools() {
    local kexec_cache="${CACHEDIR}/src/kexec-tools-${KEXEC_VER}/build/sbin/kexec"

    if [ -x "${kexec_cache}" ]; then
        echo "📦 Using cached kexec ${KEXEC_VER}"
    else
        echo "--- Building kexec-tools ${KEXEC_VER} ---"
        (
            mkdir -p "${CACHEDIR}/src"
            cd "${CACHEDIR}/src"
            fetch_cached_url \
                "https://www.kernel.org/pub/linux/utils/kernel/kexec/kexec-tools-${KEXEC_VER}.tar.xz" \
                "kexec-tools-${KEXEC_VER}.tar.xz"
            tar -xf "kexec-tools-${KEXEC_VER}.tar.xz"
            cd "kexec-tools-${KEXEC_VER}"
            ./configure --prefix=/usr/local
            make -j"$(nproc)"
        )
    fi

    cp "${kexec_cache}" "${BUILD_TMPDIR}/core/usr/local/sbin/kexec"
    chmod +x "${BUILD_TMPDIR}/core/usr/local/sbin/kexec"
}

# -----------------------------------------------------------------------------
# Merge .tcz trees + recursive .dep
# install_tcz_extensions now tracks visited extensions to guard against
# circular dependencies and prevent an infinite loop. (Fix #10)
# -----------------------------------------------------------------------------
install_tcz_extensions() {
    local ext mount_dir deps visited=""
    local max_rounds=50 round=0

    while [ -n "${EXTENSIONS}" ]; do
        if [ "${round}" -ge "${max_rounds}" ]; then
            echo "❌ install_tcz_extensions: exceeded ${max_rounds} rounds — possible circular dependency in: ${EXTENSIONS}" >&2
            exit 1
        fi
        round=$((round + 1))

        deps=""
        for ext in ${EXTENSIONS}; do
            # Skip extensions already installed.
            case " ${visited} " in
                *" ${ext} "*) continue ;;
            esac
            visited="${visited} ${ext}"

            mount_dir="$(mktemp -d --tmpdir="$(pwd)" 'mnt.XXXXXX')"
            cachetcfile "${ext}" tcz tcz
            cachetcfile "${ext}.dep" dep tcz
            mount -o loop "${CACHEDIR}/tcz/${ext}" "${mount_dir}"
            cp -r "${mount_dir}/"* "${BUILD_TMPDIR}/core/"
            cleanup_mount_dir "${mount_dir}"
            deps=$(echo "${deps}" | cat - "${CACHEDIR}/dep/${ext}.dep" | sort -u)
        done
        EXTENSIONS="${deps}"
    done
}

apply_keymap_file() {
    [ -n "${KEYMAP:-}" ] || return 0
    mkdir -p "${BUILD_TMPDIR}/core/home/tc"
    printf '%s\n' "${KEYMAP}" > "${BUILD_TMPDIR}/core/home/tc/keymap"
}

apply_ssh_bundle() {
    [ "$SSHBUILD" = true ] || return 0

    if [[ ! -f ./ssh/dropbear_ecdsa_host_key || ! -f ./ssh/dropbear_rsa_host_key ]]; then
        dropbearkey -t ecdsa -s 521 -f ./ssh/dropbear_ecdsa_host_key
        dropbearkey -t rsa   -s 4096 -f ./ssh/dropbear_rsa_host_key
    fi
    mkdir -p "${BUILD_TMPDIR}/core/usr/local/etc/dropbear/"
    cp ./ssh/dropbear* "${BUILD_TMPDIR}/core/usr/local/etc/dropbear/"
    cp ./ssh/banner    "${BUILD_TMPDIR}/core/usr/local/etc/dropbear/"
    mkdir -p "${BUILD_TMPDIR}/core/home/tc/.ssh"
    cp ./ssh/authorized_keys     "${BUILD_TMPDIR}/core/home/tc/.ssh/"
    cp ./ssh/ssh_sed_unlock.sh   "${BUILD_TMPDIR}/core/usr/local/sbin/"
    chmod +x "${BUILD_TMPDIR}/core/usr/local/sbin/ssh_sed_unlock.sh"
    chown -R 1001 "${BUILD_TMPDIR}/core/home/tc/"
    chmod 700 "${BUILD_TMPDIR}/core/home/tc/.ssh"
    chmod 600 "${BUILD_TMPDIR}/core/home/tc/.ssh/authorized_keys"
}

slim_initrd_filesystem() {
    find "${BUILD_TMPDIR}/core/usr/share/man"    -type f -delete 2>/dev/null || true
    find "${BUILD_TMPDIR}/core/usr/share/doc"    -type f -delete 2>/dev/null || true
    find "${BUILD_TMPDIR}/core/usr/share/locale" -type f -delete 2>/dev/null || true
    rm -rf "${BUILD_TMPDIR}/core/usr/include" "${BUILD_TMPDIR}/core/usr/lib/pkgconfig"
}

repack_initrd_to_boot() {
    chroot "${BUILD_TMPDIR}/core" /sbin/depmod "${TC_KERNEL_VERSION}"
    (
        cd "${BUILD_TMPDIR}/core"
        find . | cpio -o -H newc | xz -9 --check=crc32 >"${BUILD_TMPDIR}/fs/boot/corepure64.gz"
    )
}

# -----------------------------------------------------------------------------
# Raw disk image
# -----------------------------------------------------------------------------
build_partitioned_disk_image() {
    local fssize projected_size_mb maj min part line counter
    fssize=$(du -m --summarize --total "${BUILD_TMPDIR}/fs" | awk '$2 == "total" { printf("%.0f\n", $1); }')
    projected_size_mb=$((fssize + GRUBSIZE + 2))

    if [ "${projected_size_mb}" -gt 128 ]; then
        echo "⚠ WARNING: projected PBA image size is ${projected_size_mb}MB (>128MB OPAL2 guideline)." >&2
        echo "  Consider reducing initrd size or lowering GRUBSIZE to stay under 128MB." >&2
    fi

    dd if=/dev/zero of="${OUTPUTIMG}" bs=1M count="${projected_size_mb}"

    LOOP_DEVICE_HDD=$(losetup --find --show --partscan "${OUTPUTIMG}")

    sfdisk "${LOOP_DEVICE_HDD}" <<'EOF'
label: dos
,+,0xEF,*
EOF

    # Create explicit partition device nodes when udev is absent (e.g. Docker).
    # The node is only created when it does not already exist. (Fix #10 — mknod
    # now also checks the return code and warns on failure rather than silently
    # continuing.) (Fix for fragile mknod in Docker context)
    counter=1
    while read -r line; do
        [ -n "${line}" ] || continue
        maj=$(echo "${line}" | cut -d: -f1)
        min=$(echo "${line}" | cut -d: -f2)
        if [ ! -e "${LOOP_DEVICE_HDD}p${counter}" ]; then
            mknod "${LOOP_DEVICE_HDD}p${counter}" b "${maj}" "${min}" || \
                echo "⚠ mknod ${LOOP_DEVICE_HDD}p${counter} failed (may already exist)" >&2
        fi
        counter=$((counter + 1))
    done < <(lsblk --raw --output MAJ:MIN --noheadings "${LOOP_DEVICE_HDD}" | tail -n +2)

    mkfs.fat -F32 "${LOOP_DEVICE_HDD}p1"
    mount "${LOOP_DEVICE_HDD}p1" "${BUILD_TMPDIR}/img"

    grub-install --no-floppy --boot-directory="${BUILD_TMPDIR}/img/boot" --target=i386-pc "${LOOP_DEVICE_HDD}"
    grub-install --removable --boot-directory="${BUILD_TMPDIR}/img/boot" --target=x86_64-efi \
        --efi-directory="${BUILD_TMPDIR}/img/" "${LOOP_DEVICE_HDD}"
    grub-install --removable --boot-directory="${BUILD_TMPDIR}/img/boot" --target=i386-efi \
        --efi-directory="${BUILD_TMPDIR}/img/" "${LOOP_DEVICE_HDD}"

    cat >"${BUILD_TMPDIR}/img/boot/grub/grub.cfg" <<EOF
set timeout_style=hidden
set timeout=0
set default=0

menuentry "tc" {
  linux /boot/vmlinuz64 ${BOOTARGS}
  initrd /boot/corepure64.gz
}
EOF

    cp -r "${BUILD_TMPDIR}/fs/boot" "${BUILD_TMPDIR}/img/"
    sync
    umount "${BUILD_TMPDIR}/img"
    sync
    sleep 1
    losetup -d "${LOOP_DEVICE_HDD}"
    LOOP_DEVICE_HDD=""
}

finalize_output_artifact() {
    chmod ugo+r "${OUTPUTIMG}"
    ln -sf "${OUTPUTIMG}" "${LATEST_LINK}"
    echo "✅ Build complete: ${OUTPUTIMG}"
}

# -----------------------------------------------------------------------------
# Entry
# -----------------------------------------------------------------------------
main() {
    extract_config_path_from_args "$@"
    load_config_file
    require_root
    parse_args "${REMAINING_ARGS[@]}"
    configure_sedutil_source
    validate_network_settings
    apply_extension_flags
    maybe_clean_workspace

    trap cleanup EXIT

    check_host_dependencies
    setup_workdirs

    build_sedunlocksrv_go
    prepare_expert_password_hash
    ensure_tls_certs

    prepare_tinycore_iso
    fetch_sedutil_cli
    stage_kernel_and_initrd
    populate_initrd_application_tree
    build_kexec_tools
    install_tcz_extensions
    install_bonding_module_if_needed
    apply_keymap_file
    apply_ssh_bundle

    slim_initrd_filesystem
    repack_initrd_to_boot

    build_partitioned_disk_image
    finalize_output_artifact
}

main "$@"
