#!/usr/bin/env bash
set -euox pipefail

export PATH="$PATH:/usr/local/go/bin"

# =========================================================
# GLOBAL CONFIGURATION (DEFAULTS)
# =========================================================
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
EXTENSIONS="bash.tcz"

# ---------------- NETWORK DEFAULTS ----------------
NET_MODE="auto"              # auto | dhcp | static
NET_INTERFACES=""            # eth0,eth1
NET_EXCLUDE=""               # eth2,eth3
NET_BOND=""                  # eth0,eth1

NET_IP=""
NET_MASK=""
NET_GW=""
NET_DNS=""

CLEAN_MODE=false
SSHBUILD=false

# =========================================================
# ARGUMENT PARSING (EXTENDED)
# =========================================================
parse_args() {
    for arg in "$@"; do
        case "$arg" in
            --clean) CLEAN_MODE=true ;;
            SSH)     SSHBUILD=true ;;

            # -------- NETWORK OPTIONS --------
            --net=*) NET_MODE="${arg#*=}" ;;
            --ifaces=*) NET_INTERFACES="${arg#*=}" ;;
            --exclude=*) NET_EXCLUDE="${arg#*=}" ;;
            --bond=*) NET_BOND="${arg#*=}" ;;

            --ip=*) NET_IP="${arg#*=}" ;;
            --mask=*) NET_MASK="${arg#*=}" ;;
            --gw=*) NET_GW="${arg#*=}" ;;
            --dns=*) NET_DNS="${arg#*=}" ;;
        esac
    done

    if [ "$SSHBUILD" = true ]; then
        EXTENSIONS+=" dropbear.tcz"
    fi

    if [ -n "${KEYMAP+x}" ]; then
        EXTENSIONS+=" kmaps.tcz"
    fi
}

# =========================================================
# BUILD BOOTARGS (NETWORK INJECTION)
# =========================================================
build_bootargs() {
    echo "----------------------------------------------------"
    echo "🌐 Building kernel boot arguments"
    echo "----------------------------------------------------"

    # Mode
    BOOTARGS+=" sed.net=${NET_MODE}"

    # Interfaces
    [ -n "$NET_INTERFACES" ] && BOOTARGS+=" sed.ifaces=${NET_INTERFACES}"
    [ -n "$NET_EXCLUDE" ] && BOOTARGS+=" sed.exclude=${NET_EXCLUDE}"
    [ -n "$NET_BOND" ] && BOOTARGS+=" sed.bond=${NET_BOND}"

    # Static config
    [ -n "$NET_IP" ]   && BOOTARGS+=" sed.ip=${NET_IP}"
    [ -n "$NET_MASK" ] && BOOTARGS+=" sed.mask=${NET_MASK}"
    [ -n "$NET_GW" ]   && BOOTARGS+=" sed.gw=${NET_GW}"
    [ -n "$NET_DNS" ]  && BOOTARGS+=" sed.dns=${NET_DNS}"

    echo "BOOTARGS:"
    echo "$BOOTARGS"
}

# =========================================================
# SEDUTIL CONFIG
# =========================================================
configure_sedutil() {
    case "$(echo "${SEDUTIL_FORK-}" | tr '[:upper:]' '[:lower:]')" in
        "chubbyant")
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

# =========================================================
# CLEANUP
# =========================================================
cleanup() {
    echo "Cleaning up..."
    umount "${TMPDIR-}/img" 2>/dev/null || true
    [ -n "${LOOP_DEVICE_HDD-}" ] && losetup -d "${LOOP_DEVICE_HDD}" 2>/dev/null || true
    rm -rf "${TMPDIR-}"
}

run_cleanup_if_requested() {
    if [ "$CLEAN_MODE" = true ]; then
        echo "Cleaning build environment..."
        rm -rf "${CACHEDIR}" "${TMPDIR}"
    fi
}

# =========================================================
# DEPENDENCIES
# =========================================================
check_dependencies() {
    local REQUIRED_TOOLS="gcc make curl tar xorriso bsdtar go"
    local MISSING=""

    for tool in $REQUIRED_TOOLS; do
        command -v "$tool" >/dev/null 2>&1 || MISSING+=" $tool"
    done

    if [ -n "$MISSING" ]; then
        echo "ERROR: Missing tools:$MISSING"
        exit 1
    fi
}

# =========================================================
# SETUP
# =========================================================
setup_directories() {
    mkdir -p "${TMPDIR}"/{fs/boot,core,img}
    mkdir -p "${CACHEDIR}"/{iso,tcz,dep,iso-extracted}
    mkdir -p "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
}

# =========================================================
# GO BUILD (UNCHANGED)
# =========================================================
build_go_binary() {
    (
        cd ./sedunlocksrv

        GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
        MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
        MINOR=$(echo "$GO_VERSION" | cut -d. -f2)

        if [ "$MAJOR" -lt 1 ] || [ "$MINOR" -lt 21 ]; then
            echo "Go 1.21+ required"
            exit 1
        fi

        [ -f go.mod ] || go mod init sedunlocksrv

        go get golang.org/x/term
        go mod tidy

        go vet ./...
        go build -o /dev/null .

        env GOOS=linux GOARCH=amd64 \
            go build -ldflags="-s -w" -trimpath -o sedunlocksrv

        chown 1001:50 sedunlocksrv
        chmod +x sedunlocksrv
    )
}

# =========================================================
# CACHE
# =========================================================
cachetcfile() {
    local filename="$1"
    local local_dir="$2"
    local remote_type="$3"

    local path="${CACHEDIR}/${local_dir}/${filename}"

    if [ -s "$path" ]; then
        return
    fi

    curl -fL "${TCURL}/${remote_type}/${filename}" -o "$path" || {
        [[ "$local_dir" == "dep" ]] && touch "$path"
    }
}

# =========================================================
# ISO + INITRD
# =========================================================
prepare_iso() {
    cachetcfile "$INPUTISO" iso release

    rm -rf "${CACHEDIR}/iso-extracting"
    mkdir -p "${CACHEDIR}/iso-extracting"

    xorriso -osirrox on \
        -indev "${CACHEDIR}/iso/${INPUTISO}" \
        -extract / "${CACHEDIR}/iso-extracting"

    mv "${CACHEDIR}/iso-extracting" "${CACHEDIR}/iso-extracted"
}

extract_kernel_initrd() {
    kernel=$(find "${CACHEDIR}/iso-extracted" -name vmlinuz64 | head -n1)
    core=$(find "${CACHEDIR}/iso-extracted" -name corepure64.gz | head -n1)

    cp "$(realpath "$kernel")" "${TMPDIR}/fs/boot/vmlinuz64"
    (cd "${TMPDIR}/core" && zcat "$(realpath "$core")" | cpio -id -H newc)
}

# =========================================================
# EXTENSIONS + BONDING GUARANTEE
# =========================================================
install_extensions() {
    TC_KERNEL_VERSION=$(ls "${TMPDIR}/core/lib/modules")

    REQUIRED="scsi-${TC_KERNEL_VERSION}.tcz ipv6-netfilter-${TC_KERNEL_VERSION}.tcz"

    local pending processed
    pending="$EXTENSIONS $REQUIRED"
    processed=""

    while [ -n "$pending" ]; do
        current="$pending"
        pending=""

        for ext in $current; do
            if echo "$processed" | grep -qw "$ext"; then
                continue
            fi

            processed="$processed $ext"

            cachetcfile "$ext" tcz tcz
            cachetcfile "$ext.dep" dep tcz

            MOUNTDIREXT=$(mktemp -d)
            mount -o loop "${CACHEDIR}/tcz/${ext}" "$MOUNTDIREXT"
            cp -r "${MOUNTDIREXT}/"* "${TMPDIR}/core/"
            umount "$MOUNTDIREXT"
            rm -rf "$MOUNTDIREXT"

            [ -s "${CACHEDIR}/dep/${ext}.dep" ] && \
                pending="$pending $(cat "${CACHEDIR}/dep/${ext}.dep")"
        done
    done

    # VALIDATE bonding
    if ! find "${TMPDIR}/core/lib/modules" -name bonding.ko | grep -q .; then
        echo "ERROR: bonding.ko missing"
        exit 1
    fi
}

# =========================================================
# IMAGE BUILD
# =========================================================
build_image() {
    FSSIZE=$(du -m --summarize --total "${TMPDIR}/fs" | awk '$2 == "total" { printf("%.0f\n", $1); }')

    dd if=/dev/zero of="${OUTPUTIMG}" bs=1M count=$((FSSIZE + GRUBSIZE +2))

    LOOP_DEVICE_HDD=$(losetup --find --show --partscan "${OUTPUTIMG}")

    (
        echo o; echo n; echo p; echo 1; echo; echo
        echo t; echo ef; echo a; echo w; echo q
    ) | fdisk "${LOOP_DEVICE_HDD}" || true

    mkfs.fat -F32 "${LOOP_DEVICE_HDD}p1"
    mount "${LOOP_DEVICE_HDD}p1" "${TMPDIR}/img"

    grub-install --no-floppy --boot-directory="${TMPDIR}/img/boot" --target=i386-pc "${LOOP_DEVICE_HDD}"
    grub-install --removable --boot-directory="${TMPDIR}/img/boot" --target=x86_64-efi --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"

    cat > "${TMPDIR}/img/boot/grub/grub.cfg" <<EOF
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
    losetup -d "${LOOP_DEVICE_HDD}"

    chmod ugo+r "${OUTPUTIMG}"
    ln -sf "${OUTPUTIMG}" "${LATEST_LINK}"
}

# =========================================================
# MAIN
# =========================================================
main() {
    parse_args "$@"
    configure_sedutil
    build_bootargs

    run_cleanup_if_requested
    trap cleanup EXIT

    check_dependencies
    setup_directories

    build_go_binary

    prepare_iso
    extract_kernel_initrd
    install_extensions

    build_image

    echo "Build Complete: ${OUTPUTIMG}"
}

main "$@"
