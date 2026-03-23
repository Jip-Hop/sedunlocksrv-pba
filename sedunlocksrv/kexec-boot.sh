#!/bin/sh
# kexec-boot.sh: Locate and boot Proxmox after SED unlock

echo "----------------------------------------------------"
echo "🔄 Starting kexec transition to Proxmox"
echo "----------------------------------------------------"

# 1. Identify the unlocked boot drive
# We look for the first Opal-compliant drive that is now UNLOCKED
BOOT_DRIVE=$(sedutil-cli --scan | awk '$1 ~ /\/dev\// && $2 ~ /2/ {print $1; exit}')

if [ -z "$BOOT_DRIVE" ]; then
    echo "❌ ERROR: No compliant boot drive found."
    exit 1
fi

# 2. Search for the boot files across common partitions (p2, p3, p1)
# Proxmox default is usually p2 (ESP) or p3 (Root)
MOUNT_POINT="/mnt/proxmox"
mkdir -p "$MOUNT_POINT"

for part_suffix in p2 p3 p1; do
    PART="${BOOT_DRIVE}${part_suffix}"
    echo "🔍 Checking $PART for Proxmox kernel..."
    
    mount -r "$PART" "$MOUNT_POINT" 2>/dev/null
    if [ -f "$MOUNT_POINT/boot/vmlinuz-*-pve" ]; then
        echo "✅ Found Proxmox boot files on $PART"
        
        # 3. Identify the latest Kernel and Initrd
        # We sort by version (reverse) to get the latest one
        PVE_KERNEL=$(ls -rv "$MOUNT_POINT"/boot/vmlinuz-*-pve | head -n 1)
        PVE_INITRD=$(ls -rv "$MOUNT_POINT"/boot/initrd.img-*-pve | head -n 1)
        
        echo "   -> Kernel: $(basename "$PVE_KERNEL")"
        echo "   -> Initrd: $(basename "$PVE_INITRD")"

        # 4. Load the kernel
        # We reuse current cmdline but ensure the root= is correct for Proxmox
        # Standard PVE uses LVM: root=/dev/mapper/pve-root
        # For ZFS: root=ZFS=rpool/ROOT/pve-1
        kexec -l "$PVE_KERNEL" \
            --initrd="$PVE_INITRD" \
            --append="root=/dev/mapper/pve-root ro quiet"

        echo "🚀 Jumping to OS now..."
        umount "$MOUNT_POINT"
        kexec -e
        exit 0 # Should never reach this
    fi
    umount "$MOUNT_POINT" 2>/dev/null
done

echo "❌ ERROR: Could not find Proxmox boot files."
echo "Falling back to standard reboot in 10 seconds..."
sleep 10
reboot -f
