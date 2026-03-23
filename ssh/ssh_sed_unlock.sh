#!/bin/sh

# Ash uses standard POSIX pathing
export PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:$PATH

trap '' 2

reboot_function () {
    echo -e "\n\nRebooting..."
    reboot -nf
}

shutdown_function () {
    echo -e "\n\nShutting down..."
    poweroff -nf
}

echo "Press ESC anytime to reboot."
echo "Press CTRL-D anytime to shutdown."
echo

if /usr/local/sbin/sedunlocksrv/opal-functions.sh | grep -q "Could not detect TCG Opal 2-compliant disks"; then
    echo "Could not detect TCG Opal 2-compliant disks. Probably nothing to do here."
    echo
fi

# Use ':' for a true loop in ash
while :; do
    echo -n "🔑 Enter SED password: "
    
    password=""
    # Set terminal to raw mode to capture single keystrokes
    stty -echo -icanon min 1 time 0
    
    while :; do
        # Use dd to read exactly 1 byte (ash-friendly way to do read -n1)
        char=$(dd bs=1 count=1 2>/dev/null)
        
        case "$char" in
            $(printf '\004')) # CTRL-D
                stty sane
                shutdown_function
                ;;
            $(printf '\033')) # ESC
                stty sane
                reboot_function
                ;;
            "") # ENTER
                break
                ;;
            $(printf '\177')|$(printf '\010')) # BACKSPACE
                if [ "${#password}" -gt 0 ]; then
                    echo -ne "\b \b"
                    # POSIX way to remove last character
                    password=$(echo "$password" | sed 's/.$//')
                fi
                ;;
            *)
                echo -n '*'
                password="${password}${char}"
                ;;
        esac
    done
    
    # Reset terminal to normal mode
    stty sane
    echo

    if [ -n "$password" ]; then
        /usr/local/sbin/sedunlocksrv/opal-functions.sh "$password"
        echo
    fi
done
