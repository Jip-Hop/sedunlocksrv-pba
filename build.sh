#!/usr/bin/env bash

set -euox pipefail

function cleanup() {
    umount "${TMPDIR}/img" || true
    losetup -d "${LOOP_DEVICE_HDD}" || true
    rm -rf "${TMPDIR}"
}

SSHBUILD=FALSE
if [ "${1-default}" == "SSH" ]; then
    SSHBUILD=TRUE
    # check SSH build dependencies
    if [ ! -f ./ssh/authorized_keys ]; then
        echo "You have to create authorized_keys file in ssh folder"
        exit
    fi
    if ! which dropbear; then
        echo "Please install dropbear: apt install dropbear"
        exit
    fi
fi

trap cleanup EXIT

# Default config for 64-bit Linux and Sedutil
GRUBSIZE=15 # Reserve this amount of MiB on the image for GRUB (increase this number if needed)
CACHEDIR="cache"
TCURL="http://distro.ibiblio.org/tinycorelinux/15.x/x86_64"
INPUTISO="TinyCorePure64-current.iso"
OUTPUTIMG="sedunlocksrv-pba.img"
BOOTARGS="quiet libata.allow_tpm=1"
SEDUTILBINFILENAME="sedutil-cli"
EXTENSIONS="bash.tcz"
if [ $SSHBUILD == "TRUE" ]; then
    EXTENSIONS="$EXTENSIONS dropbear.tcz"
fi
if [ ! -z "${KEYMAP+x}" ]; then
    EXTENSIONS="$EXTENSIONS kmaps.tcz"
fi
case "$(echo ${SEDUTIL_FORK-} | tr '[:upper:]' '[:lower:]')" in
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
mkdir -p "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"

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

if [ ! -f "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}" ]; then
    case "${SEDUTIL_FORK}" in
        "ChubbyAnt")
            # Download and Unpack Sedutil
            # Use bsdtar to auto-detect de-compression algorithm
            wget -O - ${SEDUTILURL} | bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
            chmod +x "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}"
        ;;
        *)
            SLASHESONLY="${SEDUTILPATHINTAR//[^\/]/}"
            LEVELSDEEP="${#SLASHESONLY}"
            # Download and Unpack Sedutil
            # Use bsdtar to auto-detect de-compression algorithm
            curl -s ${SEDUTILURL} | bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}" --strip-components="${LEVELSDEEP}" ${SEDUTILPATHINTAR}
        ;;
    esac
fi

# Copy the kernel
cp "${CACHEDIR}/iso-extracted/boot/vmlinuz64" "${TMPDIR}/fs/boot/vmlinuz64"

# Remaster the initrd
(cd "${TMPDIR}/core" && zcat "../../${CACHEDIR}/iso-extracted/boot/corepure64.gz" | cpio -i -H newc -d)

# We can only detect the kernel version after the intird is extracted.
# We need the kernel version to install the right scsi driver 
TC_KERNEL_VERSION=$(ls "${TMPDIR}/core/lib/modules")
EXTENSIONS="$EXTENSIONS  scsi-${TC_KERNEL_VERSION}.tcz"

mkdir -p "${TMPDIR}/core/usr/local/sbin/"

cp "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}" "${TMPDIR}/core/usr/local/sbin/"
rsync -avr --exclude='sedunlocksrv/main.go' --exclude='sedunlocksrv/go.mod' 'sedunlocksrv' "${TMPDIR}/core/usr/local/sbin/"
cp ./tc/tc-config "${TMPDIR}/core/etc/init.d/tc-config"
sed -i "s/::exclude_devices::/${EXCLUDE_NETDEV-}/" "${TMPDIR}/core/etc/init.d/tc-config"
# cp ./tc/bootsync.sh "${TMPDIR}/core/opt/bootsync.sh"

# Install extensions and dependencies
while [ -n "${EXTENSIONS}" ]; do
    DEPS=""
    for EXTENSION in ${EXTENSIONS}; do
        MOUNTDIREXT="$(mktemp -d --tmpdir="$(pwd)" 'mnt.XXXXXX')"
        cachetcfile "${EXTENSION}" tcz tcz
        cachetcfile "${EXTENSION}.dep" dep tcz
        mount -o loop "${CACHEDIR}/tcz/${EXTENSION}" "${MOUNTDIREXT}"
        cp -r "${MOUNTDIREXT}/"* "${TMPDIR}/core/"
        umount "${MOUNTDIREXT}"
        rm -rfv "${MOUNTDIREXT}"
        DEPS=$(echo "${DEPS}" | cat - "${CACHEDIR}/dep/${EXTENSION}.dep" | sort -u)
    done
    EXTENSIONS="${DEPS}"
done

if [ ! -z "${KEYMAP+x}" ]; then
  mkdir -p "${TMPDIR}/core/home/tc"
  echo -en "${KEYMAP}" > "${TMPDIR}/core/home/tc/keymap"
fi

if [ $SSHBUILD == "TRUE" ]; then
    # Generate dropbear hostkeys if not existing
    if [[ ! -f ./ssh/dropbear_ecdsa_host_key || ! -f ./ssh/dropbear_rsa_host_key ]]; then
        dropbearkey -t ecdsa -s 521 -f ./ssh/dropbear_ecdsa_host_key
        dropbearkey -t rsa -s 4096 -f ./ssh/dropbear_rsa_host_key
    fi

    # Copy dropbear files to the image
    mkdir -p "${TMPDIR}/core/usr/local/etc/dropbear/"
    cp ./ssh/dropbear* "${TMPDIR}/core/usr/local/etc/dropbear/"
    cp ./ssh/banner "${TMPDIR}/core/usr/local/etc/dropbear/"

    # Copy tc user files to the image
    mkdir -p "${TMPDIR}/core/home/tc/.ssh"
    cp ./ssh/authorized_keys "${TMPDIR}/core/home/tc/.ssh/"
    cp ./ssh/ssh_sed_unlock.sh "${TMPDIR}/core/home/tc/"
    chown -R 1001 "${TMPDIR}/core/home/tc/"
    chmod 700 "${TMPDIR}/core/home/tc/.ssh"
    chmod 600 "${TMPDIR}/core/home/tc/.ssh/authorized_keys"
fi

# Since we installed the scsi extension by extracting it rather than using tce-load, we need to fix modules.dep
chroot "${TMPDIR}/core" /sbin/depmod "$TC_KERNEL_VERSION"

# Repackage the initrd
(cd "${TMPDIR}/core" && find | cpio -o -H newc | gzip -9 >"${TMPDIR}/fs/boot/corepure64.gz")

FSSIZE="$(du -m --summarize --total "${TMPDIR}/fs" | awk '$2 == "total" { printf("%.0f\n", $1); }')"

# Make the image
# Add 1 MiB extra to account for FSSIZE rounding errors
dd if=/dev/zero of="${OUTPUTIMG}" bs=1M count=$((FSSIZE + GRUBSIZE +1))

# Attaching hard disk image file to loop device.
LOOP_DEVICE_HDD=$(losetup --find --show --partscan ${OUTPUTIMG})

(
    echo o   # clear the in memory partition table
    echo n   # new partition
    echo p   # primary partition
    echo 1   # partition number 1
    echo     # default - start at beginning of disk
    echo     # default, extend partition to end of disk
    echo t   # change partition type
    echo ef  # set partition type to EFI (FAT-12/16/32)
    echo a   # make a partition bootable
    echo w   # write the partition table
    echo q   # and we're done
) | fdisk "${LOOP_DEVICE_HDD}" || true

# Using mknod to create partition node files
# https://github.com/moby/moby/issues/27886#issuecomment-417074845
# drop the first line, as this is our LOOP_DEVICE_HDD itself, but we only want the child partitions
PARTITIONS=$(lsblk --raw --output "MAJ:MIN" --noheadings ${LOOP_DEVICE_HDD} | tail -n +2)
COUNTER=1
for i in $PARTITIONS; do
    MAJ=$(echo $i | cut -d: -f1)
    MIN=$(echo $i | cut -d: -f2)
    if [ ! -e "${LOOP_DEVICE_HDD}p${COUNTER}" ]; then mknod ${LOOP_DEVICE_HDD}p${COUNTER} b $MAJ $MIN; fi
    COUNTER=$((COUNTER + 1))
done

mkfs.fat -F32 "${LOOP_DEVICE_HDD}p1"

mount "${LOOP_DEVICE_HDD}p1" "${TMPDIR}/img"

# Install GRUB

grub-install --no-floppy --boot-directory="${TMPDIR}/img/boot" --target=i386-pc "${LOOP_DEVICE_HDD}"
grub-install --removable --boot-directory="${TMPDIR}/img/boot" --target=x86_64-efi --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"
grub-install --removable --boot-directory="${TMPDIR}/img/boot" --target=i386-efi --efi-directory="${TMPDIR}/img/" "${LOOP_DEVICE_HDD}"

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
