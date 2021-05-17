#!/usr/bin/env bash

shopt -s nullglob extglob

password=$1
new_password=$2
new_password2=$3

# Unlock code borrowed from rear:
# https://github.com/rear/rear/blob/6a3d0b4d5e73c69a62ce0bd209b2b38ffb462569/usr/share/rear/lib/opal-functions.sh
# https://github.com/rear/rear/blob/6a3d0b4d5e73c69a62ce0bd209b2b38ffb462569/usr/share/rear/skel/default/etc/scripts/unlock-opal-disks

function opal_devices() {
    # prints a list of TCG Opal 2-compliant devices.

    sedutil-cli --scan | awk '$1 ~ /\/dev\// && $2 ~ /2/ { print $1; }'
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

function opal_device_change_password() {
    local device="${1:?}"
    local old_password="${2:?}"
    local new_password="${3:?}"
    # sets a new Admin1 and SID password, returns 0 on success

    sedutil-cli --setSIDPassword "$old_password" "$new_password" "$device" &&
    sedutil-cli --setAdmin1Pwd "$old_password" "$new_password" "$device"
}

function opal_device_attributes() {
    local device="${1:?}"
    local result_variable_name="${2:?}"
    # returns a script assigning the Opal device's attributes to a local associative array variable:
    #   model=..., firmware=..., serial=..., interface=...
    #   support=[yn], setup=[yn], locked=[yn], encrypted=[yn], mbr={visible,hidden,disabled},
    #
    # Usage example:
    #   source "$(opal_device_attributes "$device" attributes)"
    #   if [[ "${attributes[setup]}" == "y" ]]; then ...

    local result_script="$(mktemp)"

    {
        echo -n "local -A $result_variable_name=( "
        sedutil-cli --query "$device" | awk '
            /^\/dev\// {
                gsub(/[$"]/, "*");  # strip characters interpreted by bash if part of a double-quoted string
                sub(/^\/dev\/[^ ]+ +/, "");  # strip device field from $0
                printf("[serial]=\"%s\" ", $(NF));
                printf("[firmware]=\"%s\" ", $(NF-1));
                sub(/ +[^ ]+ +[^ ]+ *$/, "");  # strip serial and firmware fields from $0
                printf("[interface]=\"%s\" ", $1);
                sub(/^[^ ]+ +/, "");  # strip type field from $0
                printf("[model]=\"%s\" ", $0);
            }
            /^Locking function \(0x0002\)/ {
                getline;
                gsub(/ /, "");
                split($0, field_assignments, ",");
                for (field_assignment_index in field_assignments) {
                    split(field_assignments[field_assignment_index], assignment_parts, "=");
                    raw_fields[assignment_parts[1]] = assignment_parts[2];
                }
                printf("[support]=\"%s\" ", tolower(raw_fields["LockingSupported"]));
                printf("[setup]=\"%s\" ", tolower(raw_fields["LockingEnabled"]));
                printf("[locked]=\"%s\" ", tolower(raw_fields["Locked"]));
                printf("[encrypted]=\"%s\" ", tolower(raw_fields["MediaEncrypt"]));
                printf("[mbr]=\"%s\" ", (raw_fields["MBREnabled"] == "Y" ? (raw_fields["MBRDone"] == "Y" ? "hidden" : "visible") : "disabled"));
            }
        '
        echo -e ")\nrm \"$result_script\""
    } > "$result_script"
    echo "$result_script"
}

function opal_device_attribute() {
    local device="${1:?}"
    local attribute_name="${2:?}"
    # prints the value of an Opal device attribute.

    source "$(opal_device_attributes "$device" attributes)"
    echo "${attributes[$attribute_name]}"
}

function opal_device_identification() {
    local device="${1:?}"
    # prints identification information for an Opal device.

    echo "'$device' ($(opal_device_attribute "$device" "model"))"
}

function opaladmin_changePW_action() {
    # changes the disk password.

    local device

    for device in "${devices[@]}"; do

        if [[ "$(opal_device_attribute "$device" "setup")" == "y" ]]; then
            echo "Changing disk password of device $(opal_device_identification "$device")..."
            
            if opal_device_change_password "$device" "$password" "$new_password"; then
                echo "Password changed on device $(opal_device_identification "$device")."
                
            else
                # Assume that the password for this disk did not fit, retry with a new one
                echo "Could not change password on device $(opal_device_identification "$device")."
            fi

        else
            echo "SKIPPING: Device $(opal_device_identification "$device") has not been setup, cannot change password."
        fi
    done
    echo "Done"
}

# Find TCG Opal 2-compliant disks
devices=( $(opal_devices) )
declare -i device_count=${#devices[@]}

if (( device_count == 0 )); then
    echo "Could not detect TCG Opal 2-compliant disks."
    exit
fi

# Arguments passed for password change
if [ $# -ge 3 ]; then
    
    # If new password and/or new password confirmation are passed,
    # check if they match.
    if [ "$new_password" != "$new_password2" ]; then
        echo "New password doesn't match confirmation password."
        exit    
    fi

    if [ -z "$new_password" ]; then
        echo "Please enter a non-empty password."
        exit    
    fi

    opaladmin_changePW_action
    exit
fi

# Unlock attempt
declare -i unlocked_device_count=0
for device in "${devices[@]}"; do
    "opal_device_unlock" "$device" "$password" >/dev/null && unlocked_device_count+=1
done

if (( unlocked_device_count > 0 )); then
    if (( device_count == 1 && unlocked_device_count == 1 )); then
        echo "Disk unlocked. Please reboot manually."
    else
        echo "$unlocked_device_count of $device_count disks unlocked. Please reboot manually."
    fi
else
    if (( device_count == 1 )); then
        echo "Could not unlock the disk."
    else
        echo "Could not unlock any of $device_count disks."
    fi
fi