#!/usr/bin/env bash

shopt -s nullglob extglob

# arg 1 is password
# echo "$1"
# arg 2 should be current attempt number
# echo "$2"

# Unlock code borrowed from rear:
# https://github.com/rear/rear/blob/6a3d0b4d5e73c69a62ce0bd209b2b38ffb462569/usr/share/rear/lib/opal-functions.sh
# https://github.com/rear/rear/blob/6a3d0b4d5e73c69a62ce0bd209b2b38ffb462569/usr/share/rear/skel/default/etc/scripts/unlock-opal-disks

function opal_devices() {
    # prints a list of TCG Opal 2-compliant devices.

    sedutil-cli --scan | awk '$1 ~ /\/dev\// && $2 ~ /2/ { print $1; }'
}

function opal_device_disks() {
    local device="${1:?}"
    # prints all block devices belonging to the given Opal device.
    # Normally, this is just the Opal device itself, however, NVME devices have one or more namespaces per primary
    # device and these namespaces act as disks.

    case "$device" in
        (*/nvme*)
            echo "$device"n+([0-9])  # consider all namespace block devices (NOTE: relies on nullglob extglob)
            ;;
        (*)
            echo "$device"
            ;;
    esac
}

function opal_device_max_authentications() {
    local device="${1:?}"
    # prints the maximum number of authentication attempts for the device.
    # When the maximum number of authentication attempts has been reached, an Opal device needs to be power-cycled
    # before accepting any further authentications.

    sedutil-cli --query "$device" | sed -r -e '/MaxAuthentications/ { s/.*MaxAuthentications *= *([0-9]+).*/\1/; p }' -e 'd'
}

function opal_device_hide_mbr() {
    local device="$1"
    local password="$2"
    # hides the device's shadow MBR if one has been enabled, does nothing otherwise.
    # Returns 0 on success.

    sedutil-cli --setMBRDone on "$password" "$device"
}

function opal_device_unlock() {
    local device="$1"
    local password="$2"
    # attempts to unlock the device (locking range 0 spanning the entire disk) and hide the MBR, if any.
    # Returns 0 on success.

    sedutil-cli --setLockingRange 0 RW "$password" "$device" && opal_device_hide_mbr "$device" "$password"
}

# Find TCG Opal 2-compliant disks
devices=( $(opal_devices) )
declare -i device_count=${#devices[@]}

if (( device_count == 0 )); then
    echo "Could not detect TCG Opal 2-compliant disks."
    exit
elif (( device_count == 1 )); then
    unsuccessful_unlock_response="Could not unlock the disk."
else
    unsuccessful_unlock_response="Could not unlock any of $device_count disks."
fi

# Query TCG Opal 2-compliant disks to determine the maximum number of authentication attempts
declare -i max_authentications=5  # self-imposed limit to begin with
for device in "${devices[@]}"; do
    device_max_authentications="$(opal_device_max_authentications "$device")"
    if (( device_max_authentications > 0 && device_max_authentications < max_authentications )); then
        # Limit authentication attempts to the lowest number supported by any disk
        max_authentications=$device_max_authentications
    fi
done

if [ "$2" -le "$max_authentications" ]
then

    # Success in this case is achieved if at least one device can be unlocked.
    # If other devices require different passwords for unlocking, we assume
    # that this is intentional and will be dealt with by other means.
    declare -i unlocked_device_count=0
    for device in "${devices[@]}"; do
        "opal_device_unlock" "$device" "$1" >/dev/null && unlocked_device_count+=1
    done

    if (( unlocked_device_count > 0 )); then
        if (( device_count == 1 && unlocked_device_count == 1 )); then
            echo "Disk unlocked. Please reboot manually."
        else
            echo "$unlocked_device_count of $device_count disks $result_state_message. Please reboot manually."
        fi
    else
        echo "$unsuccessful_unlock_response"
    fi

else
    echo "Maximum number of authentication attempts reached. Please reboot manually."
fi