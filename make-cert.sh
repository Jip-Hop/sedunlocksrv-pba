#!/usr/bin/env bash
# make-cert.sh — generate a self-signed TLS certificate for sedunlocksrv.
#
# Uses a Subject Alternative Name (SAN) so modern browsers accept it
# without rejecting it silently. The CN is kept for legacy compatibility.
# The cert is valid for 100 years — appropriate for an embedded PBA image
# that may not be rebuilt frequently.
#
# To use a real trusted certificate instead, set TLS_CERT_PATH and
# TLS_KEY_PATH in build.conf (or pass --tls-cert / --tls-key to build.sh).

set -e

openssl req -newkey rsa:4096 \
    -x509 \
    -sha256 \
    -days 36500 \
    -nodes \
    -out sedunlocksrv/server.crt \
    -keyout sedunlocksrv/server.key \
    -subj "/O=sedunlocksrv/CN=sedunlocksrv" \
    -addext "subjectAltName=DNS:sedunlocksrv,DNS:localhost,IP:127.0.0.1"

chmod 600 sedunlocksrv/server.key
echo "✅ TLS certificate generated: sedunlocksrv/server.crt"
echo "   SAN covers: DNS:sedunlocksrv, DNS:localhost, IP:127.0.0.1"
echo "   To suppress browser warnings, import server.crt into your browser/OS trust store."
echo "   Or supply a real cert with --tls-cert / --tls-key at build time."
