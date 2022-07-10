#!/usr/bin/env bash

set -ex

function cleanup() {
    umount "${TMPDIR}/img" || true
    losetup -d "${LOOP_DEVICE_HDD}" || true
    rm -rf "${TMPDIR}"
}
trap cleanup EXIT

# Default config for 64-bit Linux and Sedutil
GRUBSIZE=15 # Reserve this amount of MiB on the image for GRUB (increase this number if needed)
CACHEDIR="cache"
TCURL="http://distro.ibiblio.org/tinycorelinux/13.x/x86_64"
EXTENSIONS="bash.tcz"
INPUTISO="TinyCorePure64-current.iso"
OUTPUTIMG="sedunlocksrv-pba.img"
BOOTARGS="quiet libata.allow_tpm=1"
SEDUTILURL="https://raw.githubusercontent.com/Drive-Trust-Alliance/exec/master/sedutil_LINUX.tgz"
SEDUTILBINFILENAME="sedutil-cli"
SEDUTILPATHINTAR="sedutil/Release_x86_64/${SEDUTILBINFILENAME}"

# Build sedunlocksrv binary with Go
(cd ./sedunlocksrv && env GOOS=linux GOARCH=amd64 go build -trimpath && chmod +x sedunlocksrv)

# Generate cert if not existing
if [[ ! -f sedunlocksrv/server.crt || ! -f sedunlocksrv/server.key ]]; then
    ./make-cert.sh
fi

# Create our working folders
TMPDIR="$(mktemp -d --tmpdir="$(pwd)" 'img.XXXXXX')"
mkdir -p "${TMPDIR}"/{fs/boot,core,img}
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

if [ ! -f "${CACHEDIR}/${SEDUTILBINFILENAME}" ]; then
    SLASHESONLY="${SEDUTILPATHINTAR//[^\/]/}"
    LEVELSDEEP="${#SLASHESONLY}"
    # Download and Unpack Sedutil
    # Use bsdtar to auto-detect de-compression algorithm
    curl -s ${SEDUTILURL} | bsdtar -xf- -C "${CACHEDIR}" --strip-components="${LEVELSDEEP}" ${SEDUTILPATHINTAR}
fi

# Copy the kernel
cp "${CACHEDIR}/iso-extracted/boot/vmlinuz64" "${TMPDIR}/fs/boot/vmlinuz64"

# Remaster the initrd
(cd "${TMPDIR}/core" && zcat "../../${CACHEDIR}/iso-extracted/boot/corepure64.gz" | cpio -i -H newc -d)

mkdir -p "${TMPDIR}/core/usr/local/sbin/"

cp "${CACHEDIR}/${SEDUTILBINFILENAME}" "${TMPDIR}/core/usr/local/sbin/"
rsync -avr --exclude='sedunlocksrv/main.go' --exclude='sedunlocksrv/go.mod' 'sedunlocksrv' "${TMPDIR}/core/usr/local/sbin/"
cp ./tc/tc-config "${TMPDIR}/core/etc/init.d/tc-config"

# Install extensions and dependencies
while [ -n "${EXTENSIONS}" ]; do
    DEPS=""
    for EXTENSION in ${EXTENSIONS}; do
        cachetcfile "${EXTENSION}" tcz tcz
        cachetcfile "${EXTENSION}.dep" dep tcz
        unsquashfs -f -d "${TMPDIR}/core" "${CACHEDIR}/tcz/${EXTENSION}"
        DEPS=$(echo "${DEPS}" | cat - "${CACHEDIR}/dep/${EXTENSION}.dep" | sort -u)
    done
    EXTENSIONS="${DEPS}"
done

# Repackage the initrd
(cd "${TMPDIR}/core" && find | cpio -o -H newc | gzip -9 >"${TMPDIR}/fs/boot/corepure64.gz")

FSSIZE="$(du -m --summarize --total "${TMPDIR}/fs" | awk '$2 == "total" { printf("%.0f\n", $1); }')"

# Make the image
dd if=/dev/zero of="${OUTPUTIMG}" bs=1M count=$((FSSIZE + GRUBSIZE))

# Attaching hard disk image file to loop device.
LOOP_DEVICE_HDD=$(losetup -f)

losetup "${LOOP_DEVICE_HDD}" "${OUTPUTIMG}"

(
    echo o   # clear the in memory partition table
    echo n   # new partition
    echo p   # primary partition
    echo 1   # partition number 1
    echo     # default - start at beginning of disk
    echo     # default, extend partition to end of disk
    echo t   # change partition type
    echo e f # set partition type to EFI (FAT-12/16/32)
    echo a   # make a partition bootable
    echo w   # write the partition table
    echo q   # and we're done
) | fdisk "${LOOP_DEVICE_HDD}" || partprobe "${LOOP_DEVICE_HDD}"

mkfs.fat -F32 "${LOOP_DEVICE_HDD}p1"

mount "${LOOP_DEVICE_HDD}p1" "${TMPDIR}/img"

# Install GRUB

# Allow installing GRUB for AMD architecture from ARM host (these packages aren't available to install)
wget http://security.ubuntu.com/ubuntu/pool/main/g/grub2/grub-pc-bin_2.04-1ubuntu26.12_amd64.deb -O /tmp/grub-pc-bin.deb
dpkg -x /tmp/grub-pc-bin.deb /tmp/grub-pc-bin
wget http://security.ubuntu.com/ubuntu/pool/main/g/grub2/grub-efi-ia32-bin_2.04-1ubuntu26.12_amd64.deb -O /tmp/grub-efi-ia32-bin.deb
dpkg -x /tmp/grub-efi-ia32-bin.deb /tmp/grub-efi-ia32-bin
wget http://security.ubuntu.com/ubuntu/pool/main/g/grub2-unsigned/grub-efi-amd64-bin_2.04-1ubuntu44.2_amd64.deb -O /tmp/grub-efi-amd64-bin.deb
dpkg -x /tmp/grub-efi-amd64-bin.deb /tmp/grub-efi-amd64-bin

grub-install --no-floppy --boot-directory="${TMPDIR}/img/boot" --directory=/tmp/grub-pc-bin/usr/lib/grub/i386-pc "${LOOP_DEVICE_HDD}"
grub-install --removable --boot-directory="${TMPDIR}/img/boot" --directory=/tmp/grub-efi-amd64-bin/usr/lib/grub/x86_64-efi --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"
grub-install --removable --boot-directory="${TMPDIR}/img/boot" --directory=/tmp/grub-efi-ia32-bin/usr/lib/grub/i386-efi --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"

cat >"${TMPDIR}/img/boot/grub/grub.cfg" <<EOF
set timeout_style=hidden
set timeout=0
set default=0

menuentry "tc" {
  linux /boot/vmlinuz64 ${BOOTARGS}
  initrd /boot/corepure64.gz
}
EOF

# Copy the boot directory (initrd and kernel)
cp -r "${TMPDIR}/fs/boot" "${TMPDIR}/img/"

# Unmounting image file
sync
umount "${TMPDIR}/img"
sync
sleep 1

# Free loop device
losetup -d "${LOOP_DEVICE_HDD}"

# Make sure the image is readable
chmod ugo+r "${OUTPUTIMG}"

echo "DONE"
