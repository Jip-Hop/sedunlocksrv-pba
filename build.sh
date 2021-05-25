#!/usr/bin/env bash

set -ex

function cleanup() {
    # clean up our temp folder
    rm -rf "${TMPDIR}"
}
trap cleanup EXIT

# Default config for 64-bit Linux and Sedutil
CACHEDIR="cache"
TCURL="http://distro.ibiblio.org/tinycorelinux/12.x/x86_64"
EXTENSIONS="bash.tcz"
INPUTISO="TinyCorePure64-current.iso"
OUTPUTISO="sedunlocksrv-pba.iso"
BOOTARGS="quiet libata.allow_tpm=1"
SYSLINUXURL="https://mirrors.edge.kernel.org/pub/linux/utils/boot/syslinux/syslinux-6.03.tar.xz"
SEDUTILURL="https://raw.githubusercontent.com/Drive-Trust-Alliance/exec/master/sedutil_LINUX.tgz"
SEDUTILBINFILENAME="sedutil-cli"
SEDUTILPATHINTAR="sedutil/Release_x86_64/GNU-Linux/${SEDUTILBINFILENAME}"

# Build sedunlocksrv binary with Go
cd ./sedunlocksrv
env GOOS=linux GOARCH=amd64 go build -trimpath
chmod +x sedunlocksrv
cd ../

# Generate cert if not existing
if [[ ! -f sedunlocksrv/server.crt || ! -f sedunlocksrv/server.key ]]; then
    ./make-cert.sh
fi

# Create our working folders
TMPDIR="$(mktemp -d --tmpdir=$(pwd) 'iso.XXXXXX')"
chmod 755 "${TMPDIR}"
mkdir -p "${CACHEDIR}"/{iso,tcz,dep}

# Downloads a Tiny Core Linux asset, only if not already cached
function cachetcfile() {
    [ -f "${CACHEDIR}/${2}/${1}" ] || curl -f "${TCURL}/${3}/${1}" -o "${CACHEDIR}/${2}/${1}" ||
        [[ ${2} == dep ]] && touch "${CACHEDIR}/${2}/${1}"
}

if [ ! -d "${CACHEDIR}/iso-extracted" ]; then
    # Download the ISO
    cachetcfile "${INPUTISO}" iso release
    rm -rf "${CACHEDIR}/iso-extracting" && mkdir -p "${CACHEDIR}/iso-extracting"
    # Extract the contents of the ISO
    xorriso -osirrox on -indev "${CACHEDIR}/iso/${INPUTISO}" -extract / "${CACHEDIR}/iso-extracting"
    mv "${CACHEDIR}/iso-extracting" "${CACHEDIR}/iso-extracted"
fi

if [ ! -d "${CACHEDIR}/syslinux" ]; then
    rm -rf "${CACHEDIR}/syslinux-extracting" && mkdir -p "${CACHEDIR}/syslinux-extracting"
    # Download and Unpack Syslinux
    # Use bsdtar to auto-detect de-compression algorithm
    curl -s ${SYSLINUXURL} | bsdtar -xf- -C "${CACHEDIR}/syslinux-extracting"
    mv "${CACHEDIR}/syslinux-extracting" "${CACHEDIR}/syslinux"
fi

if [ ! -f "${CACHEDIR}/${SEDUTILBINFILENAME}" ]; then
    SLASHESONLY="${SEDUTILPATHINTAR//[^\/]/}"
    LEVELSDEEP="${#SLASHESONLY}"
    # Download and Unpack Sedutil
    # Use bsdtar to auto-detect de-compression algorithm
    curl -s ${SEDUTILURL} | bsdtar -xf- -C "${CACHEDIR}" --strip-components="${LEVELSDEEP}" ${SEDUTILPATHINTAR}
fi

mkdir -p "${TMPDIR}/boot"

# Get the initrd and kernel from the ISO
cp "${CACHEDIR}/iso-extracted/boot/vmlinuz64" "${TMPDIR}/boot"
# Copy files for UEFI booting
cp -r "${CACHEDIR}/iso-extracted/EFI" "${TMPDIR}/EFI"

# Copy the precompiled files 'isolinux.bin' and 'ldlinux.c32'. These files
# are used by Syslinux during the legacy BIOS boot process.
EXTRACTED_SYSLINUX_DIR=$(ls -d ${CACHEDIR}/syslinux/syslinux-*)
mkdir -p "${TMPDIR}/boot/syslinux"
cp "${EXTRACTED_SYSLINUX_DIR}/bios/core/isolinux.bin" \
    "${TMPDIR}/boot/syslinux"
cp "${EXTRACTED_SYSLINUX_DIR}/bios/com32/elflink/ldlinux/ldlinux.c32" \
    "${TMPDIR}/boot/syslinux"

cat >"${TMPDIR}/boot/syslinux/isolinux.cfg" <<EOF
default core
label core
	kernel /boot/vmlinuz64
	initrd /boot/corepure64.gz
	append $BOOTARGS
EOF

cat >"${TMPDIR}/EFI/BOOT/grub/grub.cfg" <<EOF
set timeout_style=hidden
set timeout=0
set default=0

if loadfont unicode ; then
    set gfxmode=auto
    set gfxpayload=keep
    set gfxterm_font=unicode
    terminal_output gfxterm
fi

menuentry "tc" {
  linux /boot/vmlinuz64 $BOOTARGS
  initrd /boot/corepure64.gz
}
EOF

# Remaster the initrd
rm -rf "${CACHEDIR}/core" && mkdir -p "${CACHEDIR}/core"
cd "${CACHEDIR}/core"
zcat "../iso-extracted/boot/corepure64.gz" | cpio -i -H newc -d
cd ../../

mkdir -p "${CACHEDIR}/core/usr/local/sbin/"

cp "${CACHEDIR}/${SEDUTILBINFILENAME}" "${CACHEDIR}/core/usr/local/sbin/"
rsync -avr --exclude='sedunlocksrv/main.go' --exclude='sedunlocksrv/go.mod' 'sedunlocksrv' "${CACHEDIR}/core/usr/local/sbin/"
cp ./tc/tc-config "${CACHEDIR}/core/etc/init.d/tc-config"

# Install extensions and dependencies
while [ -n "${EXTENSIONS}" ]; do
    DEPS=""
    for EXTENSION in ${EXTENSIONS}; do
        cachetcfile "${EXTENSION}" tcz tcz
        cachetcfile "${EXTENSION}.dep" dep tcz
        unsquashfs -f -d "${CACHEDIR}/core" "${CACHEDIR}/tcz/${EXTENSION}"
        DEPS=$(echo ${DEPS} | cat - "${CACHEDIR}/dep/${EXTENSION}.dep" | sort -u)
    done
    EXTENSIONS=${DEPS}
done

# Now repackage it
cd "${CACHEDIR}/core"
find | cpio -o -H newc | gzip -9 >"${TMPDIR}/boot/corepure64.gz"
cd ../../

# Generate ISO image for both BIOS and UEFI based systems.
xorriso -as mkisofs \
    -isohybrid-mbr "${EXTRACTED_SYSLINUX_DIR}/bios/mbr/isohdpfx.bin" \
    -c boot/syslinux/boot.cat \
    -b boot/syslinux/isolinux.bin \
    -no-emul-boot \
    -boot-load-size 4 \
    -boot-info-table \
    -eltorito-alt-boot \
    -e EFI/BOOT/efiboot.img \
    -no-emul-boot \
    -isohybrid-gpt-basdat \
    -o "${OUTPUTISO}" \
    "${TMPDIR}/"

# # Generate ISO image for BIOS based systems.
# xorriso -as mkisofs \
#     -isohybrid-mbr "${EXTRACTED_SYSLINUX_DIR}/bios/mbr/isohdpfx.bin" \
#     -c boot/syslinux/boot.cat \
#     -b boot/syslinux/isolinux.bin \
#     -no-emul-boot \
#     -boot-load-size 4 \
#     -boot-info-table \
#     -o "${OUTPUTISO}" \
#     "${TMPDIR}/"

# # Generate ISO image for UEFI based systems.
# xorriso -as mkisofs \
#     -isohybrid-mbr "${EXTRACTED_SYSLINUX_DIR}/bios/mbr/isohdpfx.bin" \
#     -c boot/syslinux/boot.cat \
#     -e EFI/BOOT/efiboot.img \
#     -no-emul-boot \
#     -isohybrid-gpt-basdat \
#     -o "${OUTPUTISO}" \
#     "${TMPDIR}/"
