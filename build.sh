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

# Directory containing this script (for default build.conf path).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Optional file: copy build.conf.example → build.conf, or set BUILD_CONFIG / --config=
CONFIG_FILE="${BUILD_CONFIG:-${SCRIPT_DIR}/build.conf}"

# -----------------------------------------------------------------------------
# Defaults (override via build.conf, then via CLI — CLI wins)
# -----------------------------------------------------------------------------
CACHEDIR="cache"
TMPDIR="img.tmp"

BUILD_DATE=$(date +%Y%m%d-%H%M)
OUTPUTIMG="sedunlocksrv-pba-${BUILD_DATE}.img"
LATEST_LINK="sedunlocksrv-pba-latest.img"

GRUBSIZE=25
KEXEC_VER="2.0.28"

TCURL="http://distro.ibiblio.org/tinycorelinux/15.x/x86_64"
INPUTISO="TinyCorePure64-current.iso"
BOOTARGS="libata.allow_tpm=1 net.ifnames=0 biosdevname=0"

SEDUTILBINFILENAME="sedutil-cli"
EXTENSIONS="jq.tcz"

CLEAN_MODE=false
SSHBUILD=false
KEYMAP=""
EXCLUDE_NETDEV=""

# Populated by configure_sedutil_source()
SEDUTIL_FORK=""
SEDUTILURL=""
SEDUTILPATHINTAR=""

# Kernel version string under ${TMPDIR}/core/lib/modules (set after initrd unpack)
TC_KERNEL_VERSION=""
LOOP_DEVICE_HDD=""

# Args after stripping --config= (filled by extract_config_path_from_args)
REMAINING_ARGS=()

# -----------------------------------------------------------------------------
# sedutil upstream (env SEDUTIL_FORK=ChubbyAnt or chubbyant)
# -----------------------------------------------------------------------------
configure_sedutil_source() {
    case "$(echo "${SEDUTIL_FORK-}" | tr '[:upper:]' '[:lower:]')" in
        chubbyant)
            SEDUTIL_FORK="ChubbyAnt"
            SEDUTILURL="https://github.com/ChubbyAnt/sedutil/releases/download/1.15-5ad84d8/sedutil-cli-1.15-5ad84d8.zip"
            ;;
        *)
            SEDUTIL_FORK="Drive-Trust-Alliance"
            SEDUTILURL="https://raw.githubusercontent.com/Drive-Trust-Alliance/exec/master/sedutil_LINUX.tgz"
            SEDUTILPATHINTAR="sedutil/Release_x86_64/${SEDUTILBINFILENAME}"
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
            --config=*)
                CONFIG_FILE="${arg#*=}"
                ;;
            *)
                REMAINING_ARGS+=("$arg")
                ;;
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

# CLI overrides values from defaults / build.conf (legacy: plain SSH)
print_usage() {
    echo "Usage: $0 [--config=FILE] [--clean] [--ssh] [--keymap=NAME]" >&2
    echo "          [--bootargs=KERNEL_CMDLINE] [--exclude-netdev=DEVS] [--sedutil-fork=ChubbyAnt]" >&2
    echo >&2
    echo "Config: defaults in script, then optional ${SCRIPT_DIR}/build.conf (gitignored)," >&2
    echo "        or BUILD_CONFIG env, or --config=FILE. CLI overrides file." >&2
}

parse_args() {
    for arg in "$@"; do
        case "$arg" in
            --help|-h)
                print_usage
                exit 0
                ;;
            --clean)
                CLEAN_MODE=true
                ;;
            --ssh)
                SSHBUILD=true
                ;;
            --keymap=*)
                KEYMAP="${arg#*=}"
                ;;
            --bootargs=*)
                BOOTARGS="${arg#*=}"
                ;;
            --exclude-netdev=*)
                EXCLUDE_NETDEV="${arg#*=}"
                ;;
            --sedutil-fork=*)
                SEDUTIL_FORK="${arg#*=}"
                ;;
            SSH)
                echo "WARNING: 'SSH' is deprecated; use --ssh instead." >&2
                SSHBUILD=true
                ;;
            *)
                echo "Unknown option: $arg" >&2
                print_usage
                exit 1
                ;;
        esac
    done
}

# Tiny Core extensions pulled in from flags / env after parse_args.
apply_extension_flags() {
    if [ "$SSHBUILD" = true ]; then
        EXTENSIONS="${EXTENSIONS} dropbear.tcz"
    fi
    if [ -n "${KEYMAP:-}" ]; then
        EXTENSIONS="${EXTENSIONS} kmaps.tcz"
    fi
}

maybe_clean_cache() {
    if [ "$CLEAN_MODE" != true ]; then
        return 0
    fi
    echo "🧹 Cleaning up build environment and cache..."
    rm -rf "${CACHEDIR}" "${TMPDIR}"
}

# -----------------------------------------------------------------------------
# Cleanup on exit (trap). Uses sudo for umount/losetup if build was interrupted.
# -----------------------------------------------------------------------------
cleanup() {
    echo "Cleaning up..."
    sudo umount "${TMPDIR-}/img" 2>/dev/null || true
    if [ -n "${LOOP_DEVICE_HDD-}" ]; then
        sudo losetup -d "${LOOP_DEVICE_HDD}" 2>/dev/null || true
    fi
    rm -rf "${TMPDIR-}"
}

# -----------------------------------------------------------------------------
# Host dependency check (every external binary invoked below)
# -----------------------------------------------------------------------------
check_host_dependencies() {
    echo "--- Checking build dependencies ---"

    local REQUIRED_TOOLS="
        gcc make curl tar xorriso bsdtar go cpio xz sfdisk
        sed awk cut tr date
        chown chmod
        basename mktemp find realpath zcat rsync nproc sort cat head tail
        touch mkdir cp rm mv
        mount umount chroot
        du dd losetup lsblk mknod mkfs.fat
        grub-install sync sleep ln env openssl
    "
    local missing="" tool
    for tool in ${REQUIRED_TOOLS}; do
        if ! command -v "$tool" >/dev/null 2>&1; then
            missing="${missing} ${tool}"
        fi
    done

    if [ -n "${missing}" ]; then
        echo "❌ ERROR: Missing required build tools:${missing}"
        echo "On Debian/Ubuntu, typical packages:"
        echo "  sudo apt update && sudo apt install -y \\"
        echo "    build-essential coreutils findutils curl xorriso bsdtar cpio xz-utils fdisk \\"
        echo "    golang-go gzip rsync util-linux dosfstools openssl \\"
        echo "    grub-common grub-pc-bin grub-efi-amd64-bin grub-efi-ia32-bin"
        exit 1
    fi

    if [ "$SSHBUILD" = true ] && ! command -v dropbearkey >/dev/null 2>&1; then
        echo "❌ ERROR: dropbearkey required for --ssh (e.g. apt install dropbear-bin)"
        exit 1
    fi

    echo "✅ All required tools present."
}

# -----------------------------------------------------------------------------
# Work directories under repo root
# -----------------------------------------------------------------------------
setup_workdirs() {
    mkdir -p "${TMPDIR}/fs/boot" "${TMPDIR}/core" "${TMPDIR}/img"
    mkdir -p "${CACHEDIR}/iso" "${CACHEDIR}/tcz" "${CACHEDIR}/dep" "${CACHEDIR}/iso-extracted"
    mkdir -p "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
}

# -----------------------------------------------------------------------------
# Go: vet + linux/amd64 release binary into sedunlocksrv/
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
            echo "❌ go vet failed"
            exit 1
        fi
        if ! go build -o /dev/null .; then
            echo "❌ go build (test compile) failed"
            exit 1
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
    if [[ -f sedunlocksrv/server.crt && -f sedunlocksrv/server.key ]]; then
        return 0
    fi
    ./make-cert.sh
}

# -----------------------------------------------------------------------------
# Tiny Core cache helper (ISO, .tcz, .dep)
# -----------------------------------------------------------------------------
cachetcfile() {
    local filename="$1"
    local local_dir="$2"
    local remote_type="$3"
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
            if [ ! -s "${zip}" ]; then
                echo "Fetching ChubbyAnt sedutil zip..."
                curl -fL "${SEDUTILURL}" -o "${zip}"
            else
                echo "📦 Using cached: $(basename "${zip}")"
            fi
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
            local slashes depth
            slashes="${SEDUTILPATHINTAR//[^\/]/}"
            depth="${#slashes}"
            curl -sL -H "Cache-Control: no-cache" "${SEDUTILURL}" \
                | bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}" \
                    --strip-components="${depth}" "${SEDUTILPATHINTAR}"
            ;;
    esac
}

# -----------------------------------------------------------------------------
# Kernel + initrd from ISO → ${TMPDIR}/fs/boot and unpacked core
# -----------------------------------------------------------------------------
stage_kernel_and_initrd() {
    local kernel_path core_path
    kernel_path=$(find "${CACHEDIR}/iso-extracted" -name "vmlinuz64" | head -n1)
    if [ -z "${kernel_path}" ]; then
        echo "❌ vmlinuz64 not found in ISO extract"
        exit 1
    fi
    echo "✅ Kernel: $(realpath "${kernel_path}")"
    cp "$(realpath "${kernel_path}")" "${TMPDIR}/fs/boot/vmlinuz64"

    core_path=$(find "${CACHEDIR}/iso-extracted" -name "corepure64.gz" | head -n1)
    if [ -z "${core_path}" ]; then
        echo "❌ corepure64.gz not found in ISO extract"
        exit 1
    fi
    echo "✅ Initrd: $(realpath "${core_path}")"
    (cd "${TMPDIR}/core" && zcat "$(realpath "${core_path}")" | cpio -id -H newc)

    TC_KERNEL_VERSION=$(ls "${TMPDIR}/core/lib/modules")
    EXTENSIONS="${EXTENSIONS} scsi-${TC_KERNEL_VERSION}.tcz"
    EXTENSIONS="${EXTENSIONS} ipv6-netfilter-${TC_KERNEL_VERSION}.tcz"
}

# -----------------------------------------------------------------------------
# App payload: sedutil, Go static dir, init script, optional EXCLUDE_NETDEV placeholder
# -----------------------------------------------------------------------------
populate_initrd_application_tree() {
    mkdir -p "${TMPDIR}/core/usr/local/sbin/"
    cp "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}" "${TMPDIR}/core/usr/local/sbin/"
    rsync -avr \
        --exclude='sedunlocksrv/main.go' \
        --exclude='sedunlocksrv/go.mod' \
        'sedunlocksrv' "${TMPDIR}/core/usr/local/sbin/"
    cp ./tc/tc-config "${TMPDIR}/core/etc/init.d/tc-config"
    sed -i "s/::exclude_devices::/${EXCLUDE_NETDEV-}/" "${TMPDIR}/core/etc/init.d/tc-config"
}

# -----------------------------------------------------------------------------
# kexec static binary (build once in cache/src, install into initrd)
# -----------------------------------------------------------------------------
build_kexec_tools() {
    echo "--- kexec-tools ${KEXEC_VER} ---"
    (
        mkdir -p "${CACHEDIR}/src"
        cd "${CACHEDIR}/src"
        if [ ! -f "kexec-tools-${KEXEC_VER}.tar.xz" ]; then
            curl -OL "https://www.kernel.org/pub/linux/utils/kernel/kexec/kexec-tools-${KEXEC_VER}.tar.xz"
        fi
        tar -xf "kexec-tools-${KEXEC_VER}.tar.xz"
        cd "kexec-tools-${KEXEC_VER}"
        ./configure --prefix=/usr/local
        make -j"$(nproc)"
        cp build/sbin/kexec "${TMPDIR}/core/usr/local/sbin/kexec"
        chmod +x "${TMPDIR}/core/usr/local/sbin/kexec"
    )
}

# -----------------------------------------------------------------------------
# Merge .tcz trees + recursive .dep (same pattern as tce-load expansion)
# -----------------------------------------------------------------------------
install_tcz_extensions() {
    local pending processed ext mount_dir deps

    while [ -n "${EXTENSIONS}" ]; do
        deps=""
        for ext in ${EXTENSIONS}; do
            mount_dir="$(mktemp -d --tmpdir="$(pwd)" 'mnt.XXXXXX')"
            cachetcfile "${ext}" tcz tcz
            cachetcfile "${ext}.dep" dep tcz
            mount -o loop "${CACHEDIR}/tcz/${ext}" "${mount_dir}"
            cp -r "${mount_dir}/"* "${TMPDIR}/core/"
            umount "${mount_dir}"
            rm -rf "${mount_dir}"
            deps=$(echo "${deps}" | cat - "${CACHEDIR}/dep/${ext}.dep" | sort -u)
        done
        EXTENSIONS="${deps}"
    done
}

apply_keymap_file() {
    [ -n "${KEYMAP:-}" ] || return 0
    mkdir -p "${TMPDIR}/core/home/tc"
    printf '%s\n' "${KEYMAP}" > "${TMPDIR}/core/home/tc/keymap"
}

apply_ssh_bundle() {
    [ "$SSHBUILD" = true ] || return 0

    if [[ ! -f ./ssh/dropbear_ecdsa_host_key || ! -f ./ssh/dropbear_rsa_host_key ]]; then
        dropbearkey -t ecdsa -s 521 -f ./ssh/dropbear_ecdsa_host_key
        dropbearkey -t rsa -s 4096 -f ./ssh/dropbear_rsa_host_key
    fi
    mkdir -p "${TMPDIR}/core/usr/local/etc/dropbear/"
    cp ./ssh/dropbear* "${TMPDIR}/core/usr/local/etc/dropbear/"
    cp ./ssh/banner "${TMPDIR}/core/usr/local/etc/dropbear/"
    mkdir -p "${TMPDIR}/core/home/tc/.ssh"
    cp ./ssh/authorized_keys "${TMPDIR}/core/home/tc/.ssh/"
    cp ./ssh/ssh_sed_unlock.sh "${TMPDIR}/core/usr/local/sbin/"
    chmod +x "${TMPDIR}/core/usr/local/sbin/ssh_sed_unlock.sh"
    chown -R 1001 "${TMPDIR}/core/home/tc/"
    chmod 700 "${TMPDIR}/core/home/tc/.ssh"
    chmod 600 "${TMPDIR}/core/home/tc/.ssh/authorized_keys"
}

slim_initrd_filesystem() {
    find "${TMPDIR}/core/usr/share/man" -type f -delete 2>/dev/null || true
    find "${TMPDIR}/core/usr/share/doc" -type f -delete 2>/dev/null || true
    find "${TMPDIR}/core/usr/share/locale" -type f -delete 2>/dev/null || true
    rm -rf "${TMPDIR}/core/usr/include" "${TMPDIR}/core/usr/lib/pkgconfig"
}

repack_initrd_to_boot() {
    chroot "${TMPDIR}/core" /sbin/depmod "${TC_KERNEL_VERSION}"
    (
        cd "${TMPDIR}/core"
        find . | cpio -o -H newc | xz -9 --check=crc32 >"${TMPDIR}/fs/boot/corepure64.gz"
    )
}

# -----------------------------------------------------------------------------
# Raw disk image: one EFI FAT partition, GRUB BIOS + EFI, kernel+initrd on FAT
# -----------------------------------------------------------------------------
build_partitioned_disk_image() {
    local fssize maj min part line counter
    fssize=$(du -m --summarize --total "${TMPDIR}/fs" | awk '$2 == "total" { printf("%.0f\n", $1); }')

    dd if=/dev/zero of="${OUTPUTIMG}" bs=1M count=$((fssize + GRUBSIZE + 2))

    LOOP_DEVICE_HDD=$(losetup --find --show --partscan "${OUTPUTIMG}")

    sfdisk "${LOOP_DEVICE_HDD}" <<'EOF'
label: dos
,+,0xEF,*
EOF

    # Some environments need explicit /dev/loopNp nodes (e.g. Docker).
    counter=1
    while read -r line; do
        [ -n "${line}" ] || continue
        maj=$(echo "${line}" | cut -d: -f1)
        min=$(echo "${line}" | cut -d: -f2)
        if [ ! -e "${LOOP_DEVICE_HDD}p${counter}" ]; then
            mknod "${LOOP_DEVICE_HDD}p${counter}" b "${maj}" "${min}"
        fi
        counter=$((counter + 1))
    done < <(lsblk --raw --output MAJ:MIN --noheadings "${LOOP_DEVICE_HDD}" | tail -n +2)

    mkfs.fat -F32 "${LOOP_DEVICE_HDD}p1"
    mount "${LOOP_DEVICE_HDD}p1" "${TMPDIR}/img"

    grub-install --no-floppy --boot-directory="${TMPDIR}/img/boot" --target=i386-pc "${LOOP_DEVICE_HDD}"
    grub-install --removable --boot-directory="${TMPDIR}/img/boot" --target=x86_64-efi \
        --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"
    grub-install --removable --boot-directory="${TMPDIR}/img/boot" --target=i386-efi \
        --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"

    cat >"${TMPDIR}/img/boot/grub/grub.cfg" <<EOF
set timeout_style=hidden
set timeout=0
set default=0

menuentry "tc" {
  linux /boot/vmlinuz64 ${BOOTARGS}
  initrd /boot/corepure64.gz
}
EOF

    cp -r "${TMPDIR}/fs/boot" "${TMPDIR}/img/"
    sync
    umount "${TMPDIR}/img"
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
    parse_args "${REMAINING_ARGS[@]}"
    configure_sedutil_source
    apply_extension_flags
    maybe_clean_cache

    trap cleanup EXIT

    check_host_dependencies
    setup_workdirs

    build_sedunlocksrv_go
    ensure_tls_certs

    prepare_tinycore_iso
    fetch_sedutil_cli
    stage_kernel_and_initrd
    populate_initrd_application_tree
    build_kexec_tools
    install_tcz_extensions
    apply_keymap_file
    apply_ssh_bundle

    slim_initrd_filesystem
    repack_initrd_to_boot

    build_partitioned_disk_image
    finalize_output_artifact
}

main "$@"
