#!/usr/bin/env bash

openssl req -newkey rsa:4096 \
    -x509 \
    -sha256 \
    -days 36500 \
    -nodes \
    -out sedunlocksrv/server.crt \
    -keyout sedunlocksrv/server.key \
    -subj "/C=/ST=/L=/O=sedunlocksrv/OU=/CN=*"
