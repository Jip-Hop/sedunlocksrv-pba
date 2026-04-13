# Certificate Freshness Validation

## Problem

When using automated certificate management, there's a race condition risk:

1. **Order matters**: Certificate files must be copied to the host **before** `deploy.sh` is called
2. **Race condition**: If the certificate copy completes **after** `deploy.sh` starts but **before** `build.sh` runs, the old certificates get used instead of the new ones
3. **Silent failure**: The build completes successfully but deploys outdated certificates

**Example of the problem:**

```bash
# Time 0: Old certificates (v1) on disk
/path/to/cert.pem (serial: 12345)

# Time 0ms: Cert manager starts copying new certificates (v2)
# Time 5ms: deploy.sh starts
# Time 10ms: deploy.sh calls build.sh
# Time 50ms: Certificate copy finally completes (new v2 cert written)
#
# Result: build.sh used old cert (v1) even though new one (v2) exists!
```

## Solution

`deploy.sh` can wait for certificate updates before calling `build.sh`. Multiple detection strategies are available.

---

## Detection Strategies

### 1. **Hash-Based (Default: `--cert-freshness=hash`)**

Compares the SHA256 hash of the certificate file to detect content changes.

**Best for**: Most scenarios, immune to timing issues

**How it works:**
- On first call, hashes the certificate file
- Repeatedly checks until the hash changes
- Once hash changes, proceeds with build

**Advantages:**
- ✅ Detects actual content changes
- ✅ Immune to system clock drift
- ✅ Works across network filesystems
- ✅ Works if file is replaced (inode change)

**Disadvantages:**
- ❌ Uses CPU for repeated hashing
- ❌ Requires certificate to be copied from fresh source

**Use case:**
- Default strategy for most deployments
- Works with: Certbot, cert-manager, Let's Encrypt, manual copy

**Example:**
```bash
deploy.sh --cert-path=/etc/letsencrypt/live/example.com/fullchain.pem \
          --key-path=/etc/letsencrypt/live/example.com/privkey.pem \
          --use-ssh-key-encrypted \
          --cert-freshness=hash \
          --cert-timeout=300
```

---

### 2. **Modification Time (mtime): `--cert-freshness=mtime`**

Checks when the certificate file was last modified.

**Best for**: Simple scenarios with tight timing windows

**How it works:**
- Checks if file mtime is newer than grace period
- Waits until grace period has elapsed
- Assumes recent modifications = fresh certificates

**Advantages:**
- ✅ Fastest check (stat call only)
- ✅ Works with any filesystem
- ✅ Simple to understand

**Disadvantages:**
- ❌ Vulnerable to clock skew
- ❌ Grace period is a guess (too short = failure, too long = delays)
- ❌ Doesn't detect if old cert was recopied (same mtime)

**Grace Period Guidance:**
- Default: 10 seconds
- For local filesystems: 5-10 seconds
- For network filesystems (NFS, SMB): 15-30 seconds
- For S3/cloud sync: 30-60 seconds

**Use case:**
- Quick deployments where certificates are on local disk
- When certificate copy process finishes quickly

**Example:**
```bash
deploy.sh --cert-path=/opt/certs/cert.pem \
          --key-path=/opt/certs/key.pem \
          --use-ssh-key-encrypted \
          --cert-freshness=mtime \
          --cert-grace=5 \
          --cert-timeout=60
```

---

### 3. **Certificate Serial Number: `--cert-freshness=serial`**

Extracts and compares the certificate's serial number.

**Best for**: Detecting certificate renewals from the same CA

**How it works:**
- Extracts the X.509 serial number from certificate
- Waits for serial number to change
- Only triggers on actual certificate renewal

**Advantages:**
- ✅ Certificate-aware detection
- ✅ Detects actual renewals, not just content changes
- ✅ Immune to test/staging certificate copies

**Disadvantages:**
- ❌ Requires certificate parsing (slower than hash)
- ❌ Doesn't detect if cert is replaced with one having same serial
- ❌ Fails if certificate format is invalid

**Use case:**
- Deployments using Let's Encrypt or RFC 6844 (CAA) records
- When you want to detect "new certificate from CA" specifically

**Example:**
```bash
deploy.sh --cert-path=/etc/letsencrypt/live/example.com/fullchain.pem \
          --key-path=/etc/letsencrypt/live/example.com/privkey.pem \
          --use-ssh-key-encrypted \
          --cert-freshness=serial \
          --cert-timeout=300
```

---

### 4. **Marker File: `--cert-freshness=marker`**

Waits for explicit marker file (created by certificate update process).

**Best for**: Tight control and explicit coordination

**How it works:**
- Waits for file: `/path/to/cert.pem.updated`
- When marker file appears, proceeds with build
- Cleans up marker file after detecting

**Advantages:**
- ✅ Explicit, predictable coordination
- ✅ No guessing about timing
- ✅ Clear success indicator

**Disadvantages:**
- ❌ Requires certificate update process to create marker file
- ❌ Marker file cleanup logic needed
- ❌ Tight coupling between cert update and deploy processes

**Use case:**
- Controlled CI/CD pipelines
- When certificate update and deployment are tightly coordinated
- For auditing and explicit handshake

**Example with Certbot hook:**
```bash
# In Certbot renewal hook (/etc/letsencrypt/renewal-hooks/post/):
#!/bin/bash
cp /etc/letsencrypt/live/example.com/fullchain.pem /opt/pba-certs/
touch /opt/pba-certs/cert.pem.updated  # Create marker file
```

Then in deployment:
```bash
deploy.sh --cert-path=/opt/pba-certs/cert.pem \
          --key-path=/opt/pba-certs/key.pem \
          --use-ssh-key-encrypted \
          --cert-freshness=marker
```

---

### 5. **No Check: `--cert-freshness=none`**

Disables certificate freshness validation entirely.

**Best for**: Manual deployments, testing

**How it works:**
- Skips all freshness checks
- Immediately proceeds to build

**Advantages:**
- ✅ No delays
- ✅ No failed checks
- ✅ Fast for testing

**Disadvantages:**
- ❌ **Risk of deploying stale certificates**
- ❌ Eliminates race condition protection

**Use case:**
- Manual one-off deployments
- Testing/development environments
- When certificates are guaranteed fresh (synchronous copy)

**Example:**
```bash
deploy.sh --cert-path=/tmp/cert.pem \
          --key-path=/tmp/key.pem \
          --use-ssh-key-encrypted \
          --cert-freshness=none
```

---

## Timeout and Grace Period Options

| Option | Default | Purpose |
|--------|---------|---------|
| `--cert-freshness=METHOD` | `hash` | Which strategy to use |
| `--cert-timeout=SECS` | `300` | Max seconds to wait for certificate update (0 = infinite wait) |
| `--cert-grace=SECS` | `10` | Grace period for mtime strategy (seconds) |

### Timeout Examples

```bash
# Wait up to 5 minutes (default)
deploy.sh ... --cert-timeout=300

# Wait up to 2 minutes
deploy.sh ... --cert-timeout=120

# Wait up to 60 seconds
deploy.sh ... --cert-timeout=60

# Wait indefinitely (NOT RECOMMENDED)
deploy.sh ... --cert-timeout=0
```

### Grace Period Examples (mtime strategy only)

```bash
# Local disk copy (fast)
deploy.sh ... --cert-freshness=mtime --cert-grace=5

# Network copy over NFS
deploy.sh ... --cert-freshness=mtime --cert-grace=20

# Cloud sync (S3, etc)
deploy.sh ... --cert-freshness=mtime --cert-grace=60
```

---

## Usage Recommendations

### Scenario 1: Certbot Renewal Hook (Local Filesystem)

**Setup:**
```bash
# /etc/letsencrypt/renewal-hooks/post/notify-pba-deploy.sh
#!/bin/bash
cp /etc/letsencrypt/live/example.com/fullchain.pem /opt/pba-certs/
cp /etc/letsencrypt/live/example.com/privkey.pem /opt/pba-certs/

# Notify or trigger deploy
ssh deploy@pba-host "cd ~/sedunlocksrv/deploy && ./deploy.sh ..."
```

**Deployment command:**
```bash
ssh -i ~/.ssh/id_deploy deploy@pba-host \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/opt/pba-certs/fullchain.pem \
    --key-path=/opt/pba-certs/privkey.pem \
    --use-ssh-key-encrypted \
    --cert-freshness=hash \
    --cert-timeout=120'
```

**Why hash?** Certbot copies complete files atomically. Hash will change on each renewal. Timeout of 120s is reasonable for certificate copy.

---

### Scenario 2: CI/CD Pipeline (Strict Coordination)

**Setup:**
- Certificate update service creates `/path/to/cert.pem.updated` marker
- Deploy step waits for marker, then calls deploy.sh

**Deployment command:**
```bash
./deploy.sh \
  --cert-path=/shared/certs/fullchain.pem \
  --key-path=/shared/certs/privkey.pem \
  --use-ssh-key-encrypted \
  --cert-freshness=marker \
  --cert-timeout=300
```

**Why marker?** 
- Explicit coordination between cert and deploy
- No timing guesses
- Clear success/failure indicator
- Auditable (marker file is proof of handshake)

---

### Scenario 3: Proxmox Cluster (Manual Cert Sync)

**Setup:**
- Proxmox certs synced to `/etc/pve/pveproxy-ssl.pem` by admin/automation
- Deploy triggered manually after verification

**Deployment command:**
```bash
ssh deploy@proxmox-node \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/etc/pve/pveproxy-ssl.pem \
    --key-path=/etc/pve/pveproxy-ssl-key.pem \
    --use-ssh-key-encrypted \
    --cert-freshness=hash \
    --cert-timeout=60 \
    --dry-run'

# After verifying dry-run output:
ssh deploy@proxmox-node \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/etc/pve/pveproxy-ssl.pem \
    --key-path=/etc/pve/pveproxy-ssl-key.pem \
    --use-ssh-key-encrypted \
    --cert-freshness=hash \
    --cert-timeout=60'
```

**Why hash?**
- Manual deployment usually tight timing (cert just copied)
- Hash detects actual update
- Reasonable timeout catches copy issues

---

### Scenario 4: Development/Testing (Skip Check)

**Deployment command:**
```bash
./deploy.sh \
  --cert-path=./test-cert.pem \
  --key-path=./test-key.pem \
  --use-ssh-key-encrypted \
  --cert-freshness=none \
  --dry-run
```

**Why none?** Dev/test, certificates are under local control, speed matters.

---

## Behavior Matrix

| Strategy | Detects Content Change | Detects Renewal | Speed | Best For |
|----------|------------------------|-----------------|-------|----------|
| **hash** | ✅ Yes | ✅ Yes | Medium | Most scenarios |
| **mtime** | ⚠️ Maybe* | ⚠️ Maybe* | Fast | Local/quick copy |
| **serial** | ❌ No | ✅ Yes | Medium | CA renewals |
| **marker** | ✅ (explicit) | ✅ (explicit) | Fast | CI/CD |
| **none** | ❌ No | ❌ No | Fastest | Testing |

*Depends on timing accuracy and grace period

---

## Environment Variables

Freshness settings can also be set via environment variables:

```bash
export CERT_FRESHNESS_STRATEGY="hash"      # Strategy: hash, mtime, serial, marker, none
export CERT_FRESHNESS_TIMEOUT="300"        # Max wait seconds
export CERT_FRESHNESS_GRACE="10"           # Grace period (mtime only)

./deploy.sh --cert-path=... --key-path=... --use-ssh-key-encrypted
```

---

## Troubleshooting

### "Certificate hash unchanged after Xs"

**Problem:** Waiting for hash to change, but it hasn't.

**Causes:**
1. Certificate copy process hasn't finished
2. Certificate file permissions prevent reading
3. Wrong certificate path provided

**Solutions:**
```bash
# Verify certificate exists and is readable
ls -l /path/to/cert.pem
cat /path/to/cert.pem | head -5

# Check current hash
sha256sum /path/to/cert.pem

# Increase timeout if legitimate cert update takes longer
deploy.sh ... --cert-timeout=600

# Use mtime instead if certificate copy is faster than hash computation
deploy.sh ... --cert-freshness=mtime --cert-grace=5
```

### "Certificate age 120s exceeds grace period 10s"

**Problem:** mtime strategy detected stale certificate.

**Causes:**
1. Certificate was recently copied but grace period is too short
2. Certificate is genuinely old (not recently updated)
3. Clock skew between systems

**Solutions:**
```bash
# Increase grace period to allow legitimate delays
deploy.sh ... --cert-freshness=mtime --cert-grace=30

# Or use hash strategy which doesn't rely on timing
deploy.sh ... --cert-freshness=hash

# If clock skew is the issue, sync system time:
# sudo timedatectl set-ntp true
```

### "Marker file not found after 300s"

**Problem:** Waiting for marker file that never appears.

**Causes:**
1. Certificate update process isn't creating marker file
2. Marker file path is wrong
3. Marker file is in different location than expected

**Solutions:**
```bash
# Verify marker file is being created by cert update process
# (should be: /path/to/cert.pem.updated)
ls -la /path/to/cert.pem*

# Check deployment logs for exact path expected
tail -50 deploy-TIMESTAMP.log | grep "Marker file"

# If cert process creates marker elsewhere, use --cert-freshness=hash instead
deploy.sh ... --cert-freshness=hash
```

---

## Examples

### Example 1: Basic Hash-Based (Most Common)

```bash
#!/bin/bash
# Deploy with default hash-based freshness check

CERT_PATH="/etc/letsencrypt/live/example.com/fullchain.pem"
KEY_PATH="/etc/letsencrypt/live/example.com/privkey.pem"

ssh -i ~/.ssh/id_deploy deploy@pba-server \
  "~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=${CERT_PATH} \
    --key-path=${KEY_PATH} \
    --use-ssh-key-encrypted \
    --cert-freshness=hash \
    --cert-timeout=300"
```

### Example 2: Fast mtime-Based (Local Network)

```bash
#!/bin/bash
# Deploy with mtime check for local/LAN certificates

CERT_PATH="/mnt/shared/certs/fullchain.pem"
KEY_PATH="/mnt/shared/certs/privkey.pem"

ssh deploy@pba-server \
  "~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=${CERT_PATH} \
    --key-path=${KEY_PATH} \
    --use-ssh-key-encrypted \
    --cert-freshness=mtime \
    --cert-grace=15 \
    --cert-timeout=60"
```

### Example 3: Explicit Marker-Based (CI/CD)

```bash
#!/bin/bash
# Deploy with explicit marker file coordination

CERT_PATH="/opt/pba/certs/fullchain.pem"
KEY_PATH="/opt/pba/certs/privkey.pem"
MARKER_FILE="${CERT_PATH}.updated"

# Wait for cert update process to finish
wait_for_cert_marker() {
    local timeout=300
    local elapsed=0
    while [ ! -f "${MARKER_FILE}" ] && [ ${elapsed} -lt ${timeout} ]; do
        sleep 1
        elapsed=$((elapsed + 1))
    done
    [ -f "${MARKER_FILE}" ] || return 1
}

# Cert update happens here (external process)
./update-certificates.sh

# Then deploy with marker-based check
if wait_for_cert_marker; then
    ./deploy.sh \
      --cert-path=${CERT_PATH} \
      --key-path=${KEY_PATH} \
      --use-ssh-key-encrypted \
      --cert-freshness=marker
else
    echo "Certificate marker file not found"
    exit 1
fi
```

---

## Testing Your Setup

### Test 1: Verify hash detection works

```bash
# Terminal 1: Start deployment with hash check
deploy.sh --cert-path=/tmp/test-cert.pem \
          --key-path=/tmp/test-key.pem \
          --use-ssh-key-encrypted \
          --cert-freshness=hash \
          --cert-timeout=30

# Terminal 2 (within 30 seconds): Copy new certificate
cp /etc/letsencrypt/live/example.com/fullchain.pem /tmp/test-cert.pem

# Expected: deploy.sh detects hash change and proceeds
```

### Test 2: Verify mtime detection works

```bash
# Create test certificate
openssl req -x509 -newkey rsa:2048 -keyout /tmp/test.key -out /tmp/test.crt -days 365 -nodes

# Start deployment with mtime check
deploy.sh --cert-path=/tmp/test.crt \
          --key-path=/tmp/test.key \
          --use-ssh-key-encrypted \
          --cert-freshness=mtime \
          --cert-grace=5 \
          --cert-timeout=30

# Within 5 seconds, update file
touch /tmp/test.crt

# Expected: deploy.sh detects fresh mtime and proceeds
```

---

## Summary Table

| **If your certs are**: | **Use**: | **Timeout** | **Grace** |
|---|---|---|---|
| Local filesystem | `hash` | 60s | N/A |
| Network (NFS, etc) | `mtime` | 120s | 20s |
| Cloud (S3, etc) | `mtime` | 300s | 60s |
| CI/CD with markers | `marker` | 300s | N/A |
| CA-renewed (serial) | `serial` | 300s | N/A |
| Testing/dev | `none` | N/A | N/A |
