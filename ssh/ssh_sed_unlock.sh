#!/usr/bin/env bash

export PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin"$PATH"

trap '' 2

reboot_function () {
    # reboot
    echo; echo
    echo "Rebooting..."
    reboot -nf
}

shutdown_function () {
    # shutdwn
    echo; echo
    echo "Shutting down..."
    poweroff -nf
}

efiupdate_function () {
    # efiupdate
    echo
    /usr/local/sbin/sedunlocksrv/efiupdate.sh
    echo
}


echo "Press ESC anytime to reboot."
echo "Press CTRL-D anytime to shutdown."

echo

if /usr/local/sbin/sedunlocksrv/opal-functions.sh | grep -q "Could not detect TCG Opal 2-compliant disks"
    then
        echo "Could not detect TCG Opal 2-compliant disks. Probably nothing to do here."
        echo
fi

while [ true ] ; do

    echo -n "🔑🔑🔑 Enter SED password: "

    unset password;
    while IFS= read -r -n1 -s char; do
        case "$char" in
        $'\004') # if input == CTRL-D key
            shutdown_function
            ;;
        $'\e') # if input == ESC key
            if [ -e /home/tc/partid-efi ]; then
                efiupdate_function
            fi
            reboot_function
            ;;
        $'\0') # if input == ENTER key
            break
            ;;
        $'\177') # if input == BACKSPACE key
            if [ ${#password} -gt 0 ]; then
                echo -ne "\b \b"
                password=${password::-1}
            fi
            ;;
        *)
            echo -n '*'
            password+="$char"
            ;;
        esac
    done
    echo

    # do not accept empty passwords
    if [ -z "$password" ]
    then
        :
    else
        # attempt to unlock SED drive(s)
        /usr/local/sbin/sedunlocksrv/opal-functions.sh "$password"
        echo
    fi
done
