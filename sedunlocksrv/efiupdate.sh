#!/usr/bin/env bash
shopt -s nullglob extglob
PARTID=$(cat /home/tc/partid-efi 2> /dev/null)

if [ -z ${PARTID} ] ; then
   echo "Need PartionID defined on ~tc/partid-efi for add EFI entry"
   # Sleep for see message before reboot
   sleep 5
   exit 1
fi

# Search partion EFI
efi_partition=$(sudo blkid  |  grep -i "${PARTID}" | awk '{print $1}' | sed 's/://g')

# Check if partion is detected
if [ -z "$efi_partition" ]; then
    echo "Partition EFI with ID ${PARTID} not found."
    # Sleep for see message before reboot
    sleep 5
    exit 1
fi

# Extract DEV DISK
efi_disk="$(echo $efi_partition | sed 's/p[0-9]*$//')"


# Extract PART NUMBER
efi_part_num=$(echo $efi_partition | grep -o '[0-9]*$')


# Get information entry EFI
sudo mount ${efi_partition} /mnt
loaderefi=$(sudo find /mnt/EFI -type f -name '*.efi' -print | sed 's!/mnt!!g')
labelefi=$(dirname ${loaderefi} | sed 's!^/EFI/!!g')
loaderefi=$(echo   ${loaderefi} | sed 's!/!\\!g')
echo "Disk:${efi_disk} Part:${efi_part_num} Label:${labelefi} Loader:${loaderefi}"
sudo umount /mnt

# Mount variable EFI
sudo mount -t efivarfs efivarfs /sys/firmware/efi/efivars

# Add Entry EFI
sudo efibootmgr --create --disk "$efi_disk" --part "$efi_part_num" --label "$labelefi" --loader "${loaderefi}"
# Prevent reboot without sync
sync
