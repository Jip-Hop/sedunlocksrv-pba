#!/bin/sh

# Unused and not copied to the ISO at the moment.
# Could be used when not modifying tc-config.

echo "Wait for network connection..."
until ifconfig | grep -q Bcast
do
    sleep 1
done

cd /usr/local/sbin/sedunlocksrv/
./sedunlocksrv