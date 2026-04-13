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

The default strategy (`serial`) stores the certificate's X.509 serial number to `~/sedunlocksrv/.cert-serial.baseline` after each successful deploy. On subsequent runs it compares the current serial against the baseline — no external state management required.

---

## Detection Strategies

### 1. **Certificate Serial Number (Default): `--cert-freshness=serial`**

Uses a stored serial number baseline to detect whether the certificate has been renewed since the last successful deploy.

**Best for**: Automated pipelines (ACME hooks, cron jobs, CI/CD) — correct on first run and every run after

**How it works:**
- After each successful deploy, saves the certificate's X.509 serial number to `~/sedunlocksrv/.cert-serial.baseline`
- On the next run:
  - **No baseline file** (first run): proceeds immediately; baseline is written after deploy
  - **Serial changed**: proceeds immediately — new certificate confirmed
  - **Serial unchanged**: polls for a hash change up to `--cert-timeout` seconds (handles cert copy still in progress)

**Advantages:**
- ✅ First-run safe — never hangs on initial deployment
- ✅ Detects actual CA renewals, not incidental file changes
- ✅ Falls back to hash polling if copy is still in progress
- ✅ Baseline file is written by deploy.sh — no external state management needed

**Disadvantages:**
- ❌ Requires `openssl` for serial extraction
- ❌ Baseline file must be writable by the service user

**Use case:**
- All automated deployments: ACME renewal hooks, cron jobs, CI/CD pipelines
- Default strategy; no `--cert-freshness` argument required

**Example:**
```bash
deploy.sh --cert-path=/etc/letsencrypt/live/example.com/fullchain.pem \
          --key-path=/etc/letsencrypt/live/example.com/privkey.pem \
          --cert-timeout=300
# (serial is the default; --cert-freshness=serial not required)
```

---

### 2. **Hash-Based: `--cert-freshness=hash`**

Compares the SHA256 hash of the certificate file against the stored serial baseline to detect content changes.

**Best for**: Scenarios where serial number tracking is insufficient (e.g. self-signed certs re-issued with the same serial)

**How it works:**
- Same baseline-file logic as `serial` (first run proceeds; serial-change proceeds)
- When serial is unchanged, polls the file hash until it changes
- Explicitly selects hash as the primary detection method

**Advantages:**
- ✅ Detects content changes even if serial is reused
- ✅ Immune to system clock drift
- ✅ Works across network filesystems

**Disadvantages:**
- ❌ Uses CPU for repeated hashing
- ❌ Like `serial`, won't detect a change until the hash actually differs

**Use case:**
- Self-signed certificate environments where serial numbers are not reliably unique
- When you want to be explicit about hash-based detection

**Example:**
```bash
deploy.sh --cert-path=/etc/letsencrypt/live/example.com/fullchain.pem \
          --key-path=/etc/letsencrypt/live/example.com/privkey.pem \
          --cert-freshness=hash \
          --cert-timeout=300
```

---

### 3. **Modification Time (mtime): `--cert-freshness=mtime`**

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
| `--cert-freshness=METHOD` | `serial` | Which strategy to use |
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
    --cert-timeout=120'
```

**Why serial (default)?** Serial baseline is first-run safe and detects each CA-issued renewal automatically. If the baseline shows the serial unchanged (cert copy still in progress), it falls back to hash polling within the same timeout.

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
    --cert-timeout=60 \
    --dry-run'

# After verifying dry-run output:
ssh deploy@proxmox-node \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/etc/pve/pveproxy-ssl.pem \
    --key-path=/etc/pve/pveproxy-ssl-key.pem \
    --cert-timeout=60'
```

**Why serial (default)?**
- First run proceeds immediately; baseline written after deploy
- On renewal, serial changes → proceeds immediately
- Reasonable timeout covers any in-progress cert copy

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

| Strategy | Detects Renewal | First-Run Safe | Speed | Best For |
|----------|-----------------|----------------|-------|----------|
| **serial** (default) | ✅ Yes | ✅ Yes | Medium | All automated pipelines |
| **hash** | ✅ Yes | ✅ Yes | Medium | Self-signed / serial reuse |
| **mtime** | ⚠️ Maybe* | ⚠️ Maybe* | Fast | Local/quick copy |
| **marker** | ✅ (explicit) | ✅ Yes | Fast | CI/CD with tight coordination |
| **none** | ❌ No | ✅ Yes | Fastest | Testing / manual |

*Depends on timing accuracy and grace period

---

## Environment Variables

Freshness settings can also be set via environment variables:

```bash
export CERT_FRESHNESS_STRATEGY="serial"    # Strategy: serial, hash, mtime, marker, none
export CERT_FRESHNESS_TIMEOUT="300"        # Max wait seconds
export CERT_FRESHNESS_GRACE="10"           # Grace period (mtime only)

./deploy.sh --cert-path=... --key-path=... --use-ssh-key-encrypted
```

---

## Troubleshooting

### "Certificate serial unchanged — polling for hash change" (serial/hash strategy)

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
