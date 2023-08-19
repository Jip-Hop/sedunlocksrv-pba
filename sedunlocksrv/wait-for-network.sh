#!/usr/bin/env bash

echo "Wait for network connection..."
until ifconfig | grep -q Bcast
do
    sleep 1
done