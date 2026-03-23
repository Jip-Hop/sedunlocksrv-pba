#!/usr/bin/env bash

set -euox pipefail

# Ensure /usr/local/go/bin is in the PATH for the script
export PATH=$PATH:/usr/local/go/bin

function cleanup() {
    echo "Cleaning up..."
    # Use ${VAR-} syntax to tell bash: "If this is empty, just use nothing"
    sudo umount "${TMPDIR-}/img" 2>/dev/null || true
    [ -n "${LOOP_DEVICE_HDD-}" ] && sudo losetup -d "${LOOP_DEVICE_HDD}" 2>/dev/null || true
    rm -rf "${TMPDIR-}"
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

# Default config for 64-bit Linux and Sedutil
GRUBSIZE=15 # Reserve this amount of MiB on the image for GRUB (increase this number if needed)
CACHEDIR="cache"
TMPDIR="$(mktemp -d --tmpdir="$(pwd)" 'img.XXXXXX')"
KEXEC_VER="2.0.28"
TCURL="http://distro.ibiblio.org/tinycorelinux/15.x/x86_64"
INPUTISO="TinyCorePure64-current.iso"
BUILD_DATE=$(date +%Y%m%d-%H%M)
OUTPUTIMG="sedunlocksrv-pba-${BUILD_DATE}.img"
LATEST_LINK="sedunlocksrv-pba-latest.img"
BOOTARGS="quiet libata.allow_tpm=1 net.ifnames=0 biosdevname=0"
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

trap cleanup EXIT

# --- 2. BUILD HOST DEPENDENCY CHECK ---
echo "--- Checking Build Dependencies ---"
REQUIRED_TOOLS="gcc make curl tar xorriso bsdtar go"
MISSING_TOOLS=""

for tool in ${REQUIRED_TOOLS}; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        MISSING_TOOLS="${MISSING_TOOLS} $tool"
    fi
done

if [ -n "$MISSING_TOOLS" ]; then
    echo "❌ ERROR: Missing required build tools:$MISSING_TOOLS"
    echo "Please install them: sudo apt update && sudo apt install -y build-essential curl xorriso bsdtar golang-go"
    exit 1
fi
echo "✅ All build tools present."

# --- FORCE RE-DOWNLOAD LOGIC ---
# Instead of checking if directories exist, we destroy them first.
echo "Cleaning up previous build artifacts and caches..."
rm -rf "${CACHEDIR}"
rm -rf "${TMPDIR}"
rm -rf "mnt.*" "img.*" 

# Re-initialize fresh folders
mkdir -p "${TMPDIR}"/{fs/boot,core,img}
mkdir -p "${CACHEDIR}"/{iso,tcz,dep,iso-extracted}
mkdir -p "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"

# Build sedunlocksrv binary with Go
(
    cd ./sedunlocksrv
    echo "--- Verifying Go Code ---"

   # Verify Go version is at least 1.21
    GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
    MAJOR=$(echo $GO_VERSION | cut -d. -f1)
    MINOR=$(echo $GO_VERSION | cut -d. -f2)
    
    if [ "$MAJOR" -lt 1 ] || [ "$MINOR" -lt 21 ]; then
        echo "❌ Error: Go 1.21 or higher is required (Found: $GO_VERSION)"
        exit 1
    fi
    
    # 1. Initialize and fetch dependencies
    [ -f go.mod ] || go mod init sedunlocksrv
    go get golang.org/x/term
    go mod tidy

    # 2. Run Go Vet to check for common mistakes (shadowed variables, etc.)
    if ! go vet ./...; then
        echo "❌ Go Vet failed! Fix the code before building."
        exit 1
    fi

    # 3. Test build to check for syntax/compilation errors without saving a file
    if ! go build -o /dev/null .; then
        echo "❌ Compilation failed! Check the errors above."
        exit 1
    fi

    # 4. Final optimized build
    echo "--- Compiling Optimized Binary ---"
    if env GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o sedunlocksrv; then
        echo "✅ Go build successful: sedunlocksrv created."
        chmod +x sedunlocksrv
    else
        echo "❌ Final build failed."
        exit 1
    fi
)


# Generate cert if not existing
if [[ ! -f sedunlocksrv/server.crt || ! -f sedunlocksrv/server.key ]]; then
    ./make-cert.sh
fi

# Downloads a Tiny Core Linux asset, only if not already cached
# Redefine the download function to bypass local file checks
function cachetcfile() {
    # Argument 1: Filename (e.g., bash.tcz)
    # Argument 2: Local Subdir (e.g., tcz)
    # Argument 3: Remote Path Type (e.g., tcz)
    
    local filename="$1"
    local local_dir="$2"
    local remote_type="$3"

    echo "Fetching fresh: ${filename} from ${TCURL}/${remote_type}/"
    
    # We remove the [ -f ] check entirely. 
    # Added -L to follow redirects and -H to bypass CDN caching.
    curl -fL -H "Cache-Control: no-cache" \
         "${TCURL}/${remote_type}/${filename}" \
         -o "${CACHEDIR}/${local_dir}/${filename}" || \
         { [[ "${local_dir}" == "dep" ]] && touch "${CACHEDIR}/${local_dir}/${filename}"; }
}

    # Download the ISO
    cachetcfile "${INPUTISO}" iso release
    rm -rf "${CACHEDIR}/iso-extracting" && mkdir -p "${CACHEDIR}/iso-extracting"
    # Extract the contents of the ISO
    xorriso -osirrox on -indev "${CACHEDIR}/iso/${INPUTISO}" -extract / "${CACHEDIR}/iso-extracting"
    mv "${CACHEDIR}/iso-extracting" "${CACHEDIR}/iso-extracted"

    case "${SEDUTIL_FORK}" in
        "ChubbyAnt")
            # Download and Unpack Sedutil
            # Use bsdtar to auto-detect de-compression algorithm
            curl -sL -H "Cache-Control: no-cache" ${SEDUTILURL} | bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}"
            chmod +x "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}"
        ;;
        *)
            SLASHESONLY="${SEDUTILPATHINTAR//[^\/]/}"
            LEVELSDEEP="${#SLASHESONLY}"
            # Download and Unpack Sedutil
            # Use bsdtar to auto-detect de-compression algorithm
            curl -sL -H "Cache-Control: no-cache" ${SEDUTILURL} | bsdtar -xf- -C "${CACHEDIR}/sedutil/${SEDUTIL_FORK}" --strip-components="${LEVELSDEEP}" ${SEDUTILPATHINTAR}
        ;;
    esac
    
# Find and copy the kernel (CorePure64 usually uses /boot/vmlinuz64)
KERNEL_PATH=$(find "${CACHEDIR}/iso-extracted" -name "vmlinuz64" | head -n 1)
if [ -z "${KERNEL_PATH-}" ]; then
    echo "❌ ERROR: Kernel (vmlinuz64) not found!"
    exit 1
fi
ABS_KERNEL_PATH=$(realpath "$KERNEL_PATH")
echo "✅ Copying Kernel from: ${ABS_KERNEL_PATH}"
# Copy the kernel
cp "$ABS_KERNEL_PATH" "${TMPDIR}/fs/boot/vmlinuz64"

# Find and Extract the Initrd (core)
CORE_PATH=$(find "${CACHEDIR}/iso-extracted" -name "corepure64.gz" | head -n 1)
if [ -z "${CORE_PATH-}" ]; then
    echo "❌ ERROR: Initrd (corepure64.gz) not found!"
    exit 1
fi
ABS_CORE_PATH=$(realpath "$CORE_PATH")
echo "✅ Extracting Initrd from: ${ABS_CORE_PATH}"

# Remaster the initrd
(cd "${TMPDIR}/core" && zcat "${ABS_CORE_PATH}" | cpio -i -H newc -d)

# We can only detect the kernel version after the intird is extracted.
# We need the kernel version to install the right scsi driver 
TC_KERNEL_VERSION=$(ls "${TMPDIR}/core/lib/modules")
EXTENSIONS="$EXTENSIONS scsi-${TC_KERNEL_VERSION}.tcz"
EXTENSIONS="$EXTENSIONS ipv6-netfilter-${TC_KERNEL_VERSION}.tcz"

mkdir -p "${TMPDIR}/core/usr/local/sbin/"

cp "${CACHEDIR}/sedutil/${SEDUTIL_FORK}/${SEDUTILBINFILENAME}" "${TMPDIR}/core/usr/local/sbin/"
rsync -avr --exclude='sedunlocksrv/main.go' --exclude='sedunlocksrv/go.mod' 'sedunlocksrv' "${TMPDIR}/core/usr/local/sbin/"
cp ./tc/tc-config "${TMPDIR}/core/etc/init.d/tc-config"
sed -i "s/::exclude_devices::/${EXCLUDE_NETDEV-}/" "${TMPDIR}/core/etc/init.d/tc-config"

# --- Build kexec-tools from kernel.org source ---
echo "--- Downloading and Building kexec-tools v${KEXEC_VER} ---"

(
    mkdir -p "${CACHEDIR}/src"
    cd "${CACHEDIR}/src"
    
    # 1. Download the official source
    if [ ! -f "kexec-tools-${KEXEC_VER}.tar.xz" ]; then
        curl -OL "https://www.kernel.org/pub/linux/utils/kernel/kexec/kexec-tools-${KEXEC_VER}.tar.xz"
    fi
    
    # 2. Extract
    tar -xf "kexec-tools-${KEXEC_VER}.tar.xz"
    cd "kexec-tools-${KEXEC_VER}"
    
    # 3. Configure and Compile
    # We use --prefix to define the final path and make to build the binary
    ./configure --prefix=/usr/local
    make -j$(nproc)
    
    # 4. Copy to the PBA filesystem
    # We place it in /usr/local/sbin so the Go backend can find it
    sudo mkdir -p "${TMPDIR}/core-root/usr/local/sbin"
    sudo cp build/sbin/kexec "${TMPDIR}/core-root/usr/local/sbin/kexec"
    sudo chmod +x "${TMPDIR}/core-root/usr/local/sbin/kexec"
)

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
    cp ./ssh/ssh_sed_unlock.sh "${TMPDIR}/core/usr/local/sbin/"
    chmod +x "${TMPDIR}/core/usr/local/sbin/ssh_sed_unlock.sh"
    chown -R 1001 "${TMPDIR}/core/home/tc/"
    chmod 700 "${TMPDIR}/core/home/tc/.ssh"
    chmod 600 "${TMPDIR}/core/home/tc/.ssh/authorized_keys"
fi

# Remove bloat from the core filesystem to reduce PBA size
find "${TMPDIR}/core/usr/share/man" -type f -delete 2>/dev/null || true
find "${TMPDIR}/core/usr/share/doc" -type f -delete 2>/dev/null || true
find "${TMPDIR}/core/usr/share/locale" -type f -delete 2>/dev/null || true
rm -rf "${TMPDIR}/core/usr/include" "${TMPDIR}/core/usr/lib/pkgconfig"

# Since we installed the scsi extension by extracting it rather than using tce-load, we need to fix modules.dep
chroot "${TMPDIR}/core" /sbin/depmod "$TC_KERNEL_VERSION"

# Repackage the initrd - change compression to xz instead of gzip
(cd "${TMPDIR}/core" && find | cpio -o -H newc | xz -9 --check=crc32 >"${TMPDIR}/fs/boot/corepure64.gz")

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

# Update the symlink so external scripts always find the newest build
echo "Updating symlink: ${LATEST_LINK} -> ${OUTPUTIMG}"
ln -sf "${OUTPUTIMG}" "${LATEST_LINK}"

echo "✅ Build Complete: ${OUTPUTIMG}!"
