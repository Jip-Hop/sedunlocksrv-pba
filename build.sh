#!/usr/bin/env bash
set -euox pipefail

# =========================================================
# ROOT CHECK
# =========================================================
[ "$EUID" -ne 0 ] && echo "Run as root" && exit 1

# =========================================================
# PATH SETUP
# =========================================================
export PATH="$PATH:/usr/local/go/bin"

# =========================================================
# GLOBAL CONFIGURATION
# =========================================================
CACHEDIR="cache"
TMPDIR=$(mktemp -d)

BUILD_DATE=$(date +%Y%m%d-%H%M)
OUTPUTIMG="sedunlocksrv-pba-${BUILD_DATE}.img"
LATEST_LINK="sedunlocksrv-pba-latest.img"

GRUBSIZE=25
KEXEC_VER="2.0.28"

TCURL="http://distro.ibiblio.org/tinycorelinux/15.x/x86_64"
INPUTISO="TinyCorePure64-current.iso"

BOOTARGS="libata.allow_tpm=1 net.ifnames=0 biosdevname=0"

SEDUTILBINFILENAME="sedutil-cli"
EXTENSIONS="bash.tcz jq.tcz" # jq for JSON parsing in SSH/UI scripts

CLEAN_MODE=false
SSHBUILD=false

# =========================================================
# ARGUMENT PARSING
# =========================================================
parse_args() {
    for arg in "$@"; do
        case "$arg" in
            --clean) CLEAN_MODE=true ;;
            SSH)     SSHBUILD=true ;;
        esac
    done

    [ "$SSHBUILD" = true ] && EXTENSIONS+=" dropbear.tcz"
    [ -n "${KEYMAP+x}" ] && EXTENSIONS+=" kmaps.tcz"
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
    umount "${TMPDIR}/img" 2>/dev/null || true
    [ -n "${LOOP_DEVICE_HDD-}" ] && losetup -d "${LOOP_DEVICE_HDD}" 2>/dev/null || true
    rm -rf "${TMPDIR}"
}

run_cleanup_if_requested() {
    [ "$CLEAN_MODE" = true ] && rm -rf "${CACHEDIR}"
}

# =========================================================
# DEPENDENCIES
# =========================================================
check_dependencies() {
    local REQUIRED_TOOLS="gcc make curl tar xorriso bsdtar go losetup fdisk mkfs.fat grub-install lsblk cpio xz zcat mount umount"
    local MISSING=""

    for tool in $REQUIRED_TOOLS; do
        command -v "$tool" >/dev/null 2>&1 || MISSING+=" $tool"
    done

    [ -n "$MISSING" ] && { echo "Missing tools:$MISSING"; exit 1; }
}

# =========================================================
# SETUP
# =========================================================
setup_directories() {
    mkdir -p "${TMPDIR}"/{fs/boot,core,img}
    mkdir -p "${CACHEDIR}"/{iso,tcz,dep,iso-extracted,src}
    mkdir -p "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
}

# =========================================================
# GO BUILD
# =========================================================
build_go_binary() {
    (
        cd ./sedunlocksrv

        # Robust Go version check (>= 1.21)
        go version | grep -qE 'go1\.(2[1-9]|[3-9][0-9])' || {
            echo "Go 1.21+ required"
            exit 1
        }

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

    if [[ ! -f sedunlocksrv/server.crt || ! -f sedunlocksrv/server.key ]]; then
        ./make-cert.sh
    fi
}

# =========================================================
# CACHE
# =========================================================
cachetcfile() {
    local f="$1"
    local dir="$2"
    local type="$3"
    local path="${CACHEDIR}/${dir}/${f}"

    if [ ! -s "$path" ]; then
        curl -fsSL "${TCURL}/${type}/${f}" -o "$path" || {
            [[ "$dir" == "dep" ]] && touch "$path"
        }
    fi
}

# =========================================================
# ISO
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
# SEDUTIL
# =========================================================
prepare_sedutil() {
    case "${SEDUTIL_FORK}" in
        "ChubbyAnt")
            curl -fsSL "${SEDUTILURL}" | bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
            chmod +x "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}"
            ;;
        *)
            SLASHESONLY="${SEDUTILPATHINTAR//[^\/]/}"
            LEVELSDEEP="${#SLASHESONLY}"

            curl -fsSL "${SEDUTILURL}" | \
                bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}" \
                --strip-components="${LEVELSDEEP}" ${SEDUTILPATHINTAR}
            ;;
    esac
}

# =========================================================
# KEXEC
# =========================================================
build_kexec() {
    (
        cd "${CACHEDIR}/src"

        [ -f "kexec-tools-${KEXEC_VER}.tar.xz" ] || \
            curl -fsSLO "https://www.kernel.org/pub/linux/utils/kernel/kexec/kexec-tools-${KEXEC_VER}.tar.xz"

        tar -xf "kexec-tools-${KEXEC_VER}.tar.xz"
        cd "kexec-tools-${KEXEC_VER}"

        ./configure --prefix=/usr/local
        make -j$(nproc)

        mkdir -p "${TMPDIR}/core-root/usr/local/sbin"
        cp build/sbin/kexec "${TMPDIR}/core-root/usr/local/sbin/kexec"
        chmod +x "${TMPDIR}/core-root/usr/local/sbin/kexec"
    )
}

# =========================================================
# EXTENSIONS
# =========================================================
install_extensions() {
    TC_KERNEL_VERSION=$(find "${TMPDIR}/core/lib/modules" -mindepth 1 -maxdepth 1 -type d -printf '%f\n')

    local pending processed
    pending="$EXTENSIONS scsi-${TC_KERNEL_VERSION}.tcz ipv6-netfilter-${TC_KERNEL_VERSION}.tcz"
    processed=""

    while [ -n "$pending" ]; do
        current="$pending"
        pending=""

        for ext in $current; do
            echo "$processed" | grep -qw "$ext" && continue

            processed="$processed $ext"

            cachetcfile "$ext" tcz tcz
            cachetcfile "$ext.dep" dep tcz

            MOUNTDIREXT=$(mktemp -d)

            mount -o loop "${CACHEDIR}/tcz/${ext}" "$MOUNTDIREXT" || {
                echo "Mount failed: $ext"
                exit 1
            }

            cp -r "${MOUNTDIREXT}/"* "${TMPDIR}/core/"
            umount "$MOUNTDIREXT"
            rm -rf "$MOUNTDIREXT"

            [ -s "${CACHEDIR}/dep/${ext}.dep" ] && \
                pending="$pending $(cat "${CACHEDIR}/dep/${ext}.dep")"
        done
    done
}

# =========================================================
# SSH
# =========================================================
setup_ssh() {
    if [ "$SSHBUILD" = true ]; then
        if [[ ! -f ./ssh/dropbear_ecdsa_host_key ]]; then
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
    fi
}

# =========================================================
# INITRD
# =========================================================
finalize_initrd() {
    find "${TMPDIR}/core/usr/share/"{man,doc,locale} -type f -delete 2>/dev/null || true
    rm -rf "${TMPDIR}/core/usr/include" "${TMPDIR}/core/usr/lib/pkgconfig"

    chroot "${TMPDIR}/core" /sbin/depmod "$(ls ${TMPDIR}/core/lib/modules)"

    (cd "${TMPDIR}/core" && find | cpio -o -H newc | xz -9 > "${TMPDIR}/fs/boot/corepure64.gz")
}

# =========================================================
# IMAGE
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
    grub-install --removable --boot-directory="${TMPDIR}/img/boot" --target=i386-efi --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"

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
    sync
    sleep 1

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

    run_cleanup_if_requested
    trap cleanup EXIT INT TERM

    check_dependencies
    setup_directories

    build_go_binary

    prepare_iso
    extract_kernel_initrd
    prepare_sedutil

    install_extensions
    build_kexec
    setup_ssh
    finalize_initrd

    build_image

    echo "Build Complete: ${OUTPUTIMG}"
}

main "$@"
