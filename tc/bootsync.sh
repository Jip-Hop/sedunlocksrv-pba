#!/bin/sh

# Unused and not copied to the ISO at the moment.
# Could be used when not modifying tc-config.

echo "Wait for network connection..."
until ifconfig | grep -q Bcast
do
    sleep 1
done

# Fix to get the 64-bit binaries working
if [ ! -d /lib64 ]; then
    ln -s /lib /lib64
fi

cd /usr/local/sbin/sedunlocksrv/
./sedunlocksrv

# Reboot when sedunlocksrv exits
echo "Rebooting..."
sleep 3
reboot -nf