# Complete Deployment Workflow

This document describes the **full end-to-end workflow** for deploying the SED unlock PBA to an OPAL 2.0 drive and setting up automated certificate updates.

**Note:** This is only an example workflow. Your specific setup may differ based on how you manage certificates (Certbot, cert-manager, etc.) and your infrastructure. This guide focuses on the mechanics of building, flashing, and updating the PBA via `deploy.sh`.

## Overview

The deployment process has two distinct phases:

1. **Initial Setup Phase** (one-time, local):
   - Clone repository
   - Configure build options
   - **Manually** build initial PBA image
   - **Manually** flash to OPAL drive using recovery USB
   - Run setup script for automated future deployments

2. **Operational Phase** (ongoing, automated):
   - Remote host obtains updated TLS certificates
   - Remote host calls `deploy.sh` to build and flash new PBA with certificates
   - Process repeats whenever certificates are renewed

---

## Phase 1: Initial Setup (One-Time, Local)

### Step 1: Clone Repository

```bash
git clone https://github.com/scoutdriver73/sedunlocksrv-pba.git ~/sedunlocksrv
cd ~/sedunlocksrv
```

### Step 2: Configure build.conf

Review and customize build options:

```bash
# Copy template to working config
cp build.conf.example build.conf

# Edit build.conf with your desired options
nano build.conf
```

**Common settings to configure:**

```bash
# TLS Certificates (for PBA HTTPS server)
TLS_CERT_PATH="/path/to/your/cert.pem"      # Full chain certificate
TLS_KEY_PATH="/path/to/your/key.pem"        # Private key
TLS_SERVER_NAME="pba.example.com"           # Required with custom certs; must match one SAN on the certificate
TLS_CA_CERT_PATH="/path/to/your/ca.pem"     # Optional for internal/private CA chains used by the SSH UI

# Network Configuration
NET_MODE="bond"                             # or "single"
NET_IFACES="eth0 eth1"                      # Interfaces to configure
NET_DHCP="true"                             # true = DHCP, false = static
IP_ADDR="192.168.10.100"                    # If static
NETMASK="255.255.255.0"                     # If static

# Security
EXPERT_PASSWORD="your-expert-mode-password" # For expert UI access
PASSWORD_COMPLEXITY_ON="true"               # Enforce password rules
MIN_PASSWORD_LENGTH="12"
```

`build.sh` validates network settings after loading `build.conf` and applying
CLI overrides. Static IPv4 builds fail before image creation if the address,
netmask, gateway, or DNS values are malformed; this prevents flashing a PBA
that can never bring up the expected interface.

See [build.sh section in README.md](../README.md) for all available options.

`TLS_SERVER_NAME` matters only for the SSH UI. The SSH helper always connects to `127.0.0.1:443`, but it uses `curl --resolve` so TLS verification is performed against the configured certificate name rather than the literal loopback address. With the generated self-signed cert, this defaults to `localhost`; with a custom cert/key, you must set a matching SAN explicitly. If the custom cert chains to an internal/private CA that Tiny Core does not already trust, also set `TLS_CA_CERT_PATH` so the SSH helper can verify that chain without falling back to `curl -k`.

If you want the built image to include the SSH UI, pass `--ssh` when you invoke `build.sh`. `BUILD_ARGS` is a `deploy.sh` option, not a `build.conf` variable.

### Step 3: Build Initial PBA Image

On your **local build host** (with Go, build tools, and drive access):

```bash
# Make script executable
chmod +x build.sh

# Run build (as root, with sudo)
sudo ./build.sh

# Output: sedunlocksrv-pba-<TIMESTAMP>.img in repo root
ls -lh sedunlocksrv-pba-*.img
```

**First build takes longer** (5-10 min) due to downloading TinyCore. Subsequent builds use cache (~2-3 min).

If `EXPERT_PASSWORD` not set in build.conf, you'll be prompted to enter it interactively.

### Step 4: Initial OPAL Setup and PBA Flash (New Drive)

This is a **manual, one-time operation** performed from a sedutil rescue USB on the machine that holds the OPAL drive. You need **two USB sticks**: one for the rescue system and one to carry the PBA image you built in Step 3.

> ⚠️ **Warning:** Enabling OPAL locking is irreversible without the password. If you forget your password you will lose access to all data on the drive. Test carefully before committing.

> ℹ️ **Already initialized?** If the drive already has OPAL locking enabled and you only need to update the PBA image, skip to [Step 4b](#step-4b-manual-pba-update-already-initialized-drive) below.

---

#### 4.1 — Prepare the rescue USB

Download the Drive Trust Alliance rescue image for your system type:

- **BIOS / 32-bit UEFI:** [RESCUE32.img.gz](https://github.com/Drive-Trust-Alliance/exec/blob/master/RESCUE32.img.gz?raw=true)
- **64-bit UEFI (most modern systems):** [RESCUE64.img.gz](https://github.com/Drive-Trust-Alliance/exec/blob/master/RESCUE64.img.gz?raw=true)

> *UEFI rescue requires Secure Boot to be turned off in firmware settings.*

Decompress and write to the first USB stick:

```bash
# Decompress
gunzip RESCUE64.img.gz

# Write to USB stick (replace /dev/sdUSB with your USB device — NOT your OPAL drive)
sudo dd if=RESCUE64.img of=/dev/sdUSB bs=4M status=progress conv=fsync
```

On Windows, use [Win32DiskImager](https://sourceforge.net/projects/win32diskimager/) to write the `.img` file to the USB stick.

#### 4.2 — Copy PBA image to second USB stick

Format the second USB stick with FAT32, then copy the PBA image you built in Step 3:

```bash
# Find the PBA image in the repo root
ls -lh ~/sedunlocksrv/sedunlocksrv-pba-*.img

# Copy to FAT32 USB stick (replace /dev/sdPBA1 with your USB partition)
sudo mount /dev/sdPBA1 /mnt
sudo cp ~/sedunlocksrv/sedunlocksrv-pba-*.img /mnt/sedunlocksrv-pba.img
sudo umount /mnt
```

#### 4.3 — Boot the rescue USB on the target machine

Insert both USB sticks into the target machine (the one with the OPAL drive). Boot from the rescue USB. You will see a login prompt:

```
DriveTrust login: _
```

Enter `root` — there is no password.

#### 4.4 — Verify your drive has OPAL 2 support

```
sedutil-cli --scan
```

Expected output (look for a `2` or `12` in the second column):

```
/dev/sda    2  Samsung SSD 850 EVO 500GB   EMT01B6Q
/dev/sdb   12  Samsung SSD 850 EVO 250GB   EMT01B6Q
```

A `2` or `12` means OPAL 2 support confirmed. If your drive isn't listed or shows a `0`, stop — sedutil does not support this drive.

#### 4.5 — Enable OPAL locking with debug password

The following commands use `debug` as a temporary password (standard DTA practice). You will set a real password in step 4.8.

Replace `/dev/sdX` with your actual OPAL drive device:

```bash
sedutil-cli --initialsetup debug /dev/sdX
sedutil-cli --enablelockingrange 0 debug /dev/sdX
sedutil-cli --setlockingrange 0 lk debug /dev/sdX
sedutil-cli --setmbrdone off debug /dev/sdX
```

Expected final line of `--initialsetup` output:
```
INFO: Initial setup of TPer complete on /dev/sdX
```

#### 4.6 — Flash the custom PBA image

Connect the second USB stick (with the PBA image). Find which device it is:

```bash
fdisk -l
```

Look for a small FAT32 device (e.g. `/dev/sdb1`). Mount it and flash:

```bash
mount /dev/sdb1 /mnt
sedutil-cli --loadpbaimage debug /mnt/sedunlocksrv-pba.img /dev/sdX
```

Expected output:

```
INFO: Writing PBA to /dev/sdX
33554432 of 33554432 100% blk=1500
INFO: PBA image sedunlocksrv-pba.img written to /dev/sdX
```

#### 4.7 — Verify the PBA unlocks the drive

Run the PBA test tool using the `debug` password:

```bash
linuxpba
```

When prompted for a passphrase, enter `debug`. Expected output:

```
Drive /dev/sdX  <your drive model>  is OPAL Unlocked   <--- IMPORTANT
```

If the drive does not show `OPAL Unlocked`, do not proceed. See [Recovery information](https://github.com/Drive-Trust-Alliance/sedutil/wiki/Encrypting-your-drive#recovery-information) on the DTA wiki to disable locking and start over.

#### 4.8 — Set a real drive password

> **Fork recommendation:** Set a simple initial password here (e.g. `test`), then change it to your real password via the web interface on first PBA boot. This avoids typing a complex password on the recovery keyboard where keyboard maps may differ.

```bash
# Set SID password (replace 'test' with your chosen initial password)
sedutil-cli --setsidpassword debug test /dev/sdX

# Set Admin1 password (must match SID if you want single-password unlock)
sedutil-cli --setadmin1pwd debug test /dev/sdX
```

Verify the new password works before locking the drive:

```bash
sedutil-cli --setmbrdone on test /dev/sdX
```

Expected: `INFO: MBRDone set on`

#### 4.9 — Power off completely

```bash
poweroff
```

> ⚠️ **You must do a full power cycle** — not a reboot. The drive only locks when power is removed. Remove the rescue USB sticks after the machine is off.

#### 4.10 — First boot into PBA and set final password

Power the machine back on. It should boot directly into the sedunlocksrv PBA. From a browser on the same network:

1. Navigate to `https://<machine-ip>/` (accept the self-signed certificate warning)
2. Enter the initial password (`test`) to unlock the drive
3. After unlock, use the **Password Change** tab to set your real password
4. Press **Boot** (kexec warm handoff) or **Reboot** to continue into the OS

If the web interface is unreachable because the network settings were wrong,
use the physical console TUI. Press any key, choose `Network`, set single vs
bond mode and DHCP/static IPv4 values, then apply. This rewrites the runtime
network keys in `/etc/sedunlocksrv.conf` and runs the PBA network helper for
the current boot only. Rebuild and reflash with corrected `build.conf` values
to make the fix persistent across PBA reboots.

---

### Step 4b: Manual PBA Update (Already-Initialized Drive)

Use this procedure when the drive already has OPAL locking enabled and you only need to reflash a new PBA image — for example, when updating TLS certificates or build configuration without using `deploy.sh`.

> **Automated alternative:** If you have completed Phase 2 (setup-deploy.sh), run `deploy.sh` instead of this manual procedure. `deploy.sh` handles MBRDone coordination, certificate freshness checks, and logging automatically.

**You need:**
- The rescue USB (see step 4.1 above)
- A USB stick with the new `sedunlocksrv-pba.img` (see step 4.2 above)
- Your current OPAL Admin1 password

Boot the rescue USB, log in as `root`, then:

```bash
# 1. Confirm the drive device
sedutil-cli --scan

# 2. Connect USB with new PBA image and mount it
fdisk -l   # find the PBA USB partition (e.g. /dev/sdb1)
mount /dev/sdb1 /mnt

# 3. Allow shadow MBR to be overwritten
sedutil-cli --setmbrdone off <your-password> /dev/sdX

# 4. Flash the new PBA
sedutil-cli --loadpbaimage <your-password> /mnt/sedunlocksrv-pba.img /dev/sdX

# 5. Re-enable normal boot
sedutil-cli --setmbrdone on <your-password> /dev/sdX
```

Optionally test before rebooting:

```bash
linuxpba   # enter your real password — drive should show "OPAL Unlocked"
```

Then power off completely and test boot.

---

## Phase 2: Operational Setup (Automated Future Deployments)

### Step 5: Run setup-deploy.sh on Host with OPAL Drive

After initial manual flash, **configure the host for automated deployments**:

```bash
# On the host machine with the OPAL drive connected
cd ~/sedunlocksrv/deploy

# Make scripts executable
chmod +x setup-deploy.sh deploy.sh

# Run as root
sudo ./setup-deploy.sh
```

**Prompts:**

```
Service account name [_sedunlocksrv]: 
SSH public key path [~/.ssh/id_ed25519.pub]: 
Private key access method (1=agent, 2=file, 3=paste): 
Enter OPAL admin password: 
Confirm OPAL admin password: 
```

**What setup-deploy.sh does:**

- Assigns/Creates a service account (`_sedunlocksrv`)
  - Prompts user to specifiy a service account
  - If a service acount is not provided, then the default is created.
- Configures sudoers for automated operations
- Searches for SSH public keys in `~/.ssh/`
  - If none found: prompts for custom path
  - If one found: offers as default
  - If multiple found: shows menu to select
- **Rejects ECDSA keys** (non-deterministic signatures; Ed25519 required)
- Prompts for private key access (agent, file path, or paste-to-RAM)
- Generates random salt and signs a challenge with the private key
- Derives encryption key from the signature (SHA-256)
- Encrypts OPAL password with derived key (AES-256-CBC)
- Stores encrypted password in `.ssh/opal-password.enc`
- Stores signing public key in `.ssh/signing-key.pub`
- Stores salt and metadata in `.ssh/auth.conf`
- Validates all tools are available

**Private key is never stored on disk.** Only the public key and salt are persisted.
Decryption at runtime requires the SSH agent to hold the matching private key.

**Result:** Host is now ready for remote automated deployments.

### Step 6: Configure Certificate Management (Your Responsibility)

This is **out of scope** for this documentation, but conceptually:

You need a process on your remote host that:

1. **Detects certificate updates** — e.g., Certbot renewal, cert-manager sync, manual update
2. **Copies/stages new certificates** — to a known location the deployment script can access
3. **Calls deploy.sh** — with paths to the new certificates

**Example trigger mechanisms** (implement based on your infrastructure):

- Certbot renewal hook that calls `deploy.sh`
- Scheduled cron job that checks certificate age and triggers deployment
- Kubernetes cert-manager webhook that calls deploy script on renewal
- CI/CD pipeline (GitLab CI, GitHub Actions) that rebuilds PBA on certificate change
- Manual script that operator runs when certificates are updated

See [deploy/README.md](deploy/README.md) for cron, systemd timer, and CI/CD examples.

---

## Phase 3: Automated Certificate Deployment

### Step 7: Deploy with Updated Certificates

**Before this step:** Review [CERTIFICATE-FRESHNESS.md](CERTIFICATE-FRESHNESS.md) to understand race condition protection. Choose the detection strategy that matches your certificate management process:

- **Serial number** (default): First-run safe; detects CA renewals automatically; no extra configuration required
- **Modification time**: Fast, for local/LAN certificates  
- **Hash-based**: For self-signed certs or where serial numbers may be reused
- **Marker file**: For tight CI/CD coordination
- **None**: For manual deployments

When certificates are renewed/updated, **call deploy.sh remotely** via SSH:

```bash
# From your remote management host (or CI/CD system):
# NOTE: -A enables agent forwarding (required for password decryption)
ssh -A -i ~/.ssh/id_ed25519 deploy@target-host \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/new/fullchain.pem \
    --key-path=/path/to/new/privkey.pem \
    --tls-server-name=pba.example.com'
```

**Note:** 
- SSH agent forwarding (`-A`) is **required** so deploy.sh can sign the KDF challenge with your private key
- The private key never leaves the agent — only the signature is produced on the remote host
- No password needs to be provided on the command line
- **Ed25519 keys only** — ECDSA keys are not supported
- Certificate paths must be accessible to the script (usually on target host's filesystem)
- `--tls-server-name` is required for deploy.sh custom-certificate builds and must match a DNS/IP SAN or CN on the certificate. Add `--tls-ca-cert=/path/to/ca.pem` if the SSH UI should trust an internal/private CA chain.

### Step 7a: Dry-Run (Safe Testing)

Always test before deploying for real:

```bash
ssh -A -i ~/.ssh/id_ed25519 deploy@target-host \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/new/fullchain.pem \
    --key-path=/path/to/new/privkey.pem \
    --tls-server-name=pba.example.com \
    --dry-run'
```

This will:
- Validate certificates and OPAL drive
- Build new PBA image
- Exit without flashing to drive

**Check output for:**
```
✅ Build complete
✓ Password decrypted using SSH key signature
```

### Step 7b: Real Deployment

Once dry-run succeeds, deploy for real:

```bash
ssh -A -i ~/.ssh/id_ed25519 deploy@target-host \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/new/fullchain.pem \
    --key-path=/path/to/new/privkey.pem \
    --tls-server-name=pba.example.com'
```

**Monitor progress:**

```bash
# Watch logs on target host
ssh -A -i ~/.ssh/id_ed25519 deploy@target-host \
  'tail -f ~/sedunlocksrv/deploy-*.log'

# Check for success message
ssh -A -i ~/.ssh/id_ed25519 deploy@target-host \
  'grep -i "deployment completed" ~/sedunlocksrv/deploy-*.log'
```

---

## Complete Example: Proxmox with Let's Encrypt

Here's a complete example using **Proxmox + Let's Encrypt certificates + cron job**:

### Build Configuration (build.conf)

```bash
# Proxmox certificates for PBA HTTPS server
TLS_CERT_PATH="/etc/pve/pveproxy-ssl.pem"      # Proxmox cert
TLS_KEY_PATH="/etc/pve/pveproxy-ssl-key.pem"   # Proxmox key
TLS_SERVER_NAME="pba.example.com"              # Must match one SAN on the Proxmox certificate
TLS_CA_CERT_PATH="/etc/pve/pve-root-ca.pem"    # Optional if the bundled cert chains to a private Proxmox CA

# Network for PBA
NET_MODE="single"
NET_DHCP="true"
# Expert password is NOT stored in build.conf when using deploy.sh automation;
# pass it via --expert-password-stdin or the interactive TTY prompt instead.
```

### Initial Setup

```bash
# Clone and configure
git clone https://github.com/scoutdriver73/sedunlocksrv-pba.git ~/sedunlocksrv
cd ~/sedunlocksrv
cp build.conf.example build.conf
# Edit build.conf as above

# Build initial PBA
sudo ./build.sh

# Flash to OPAL drive — see Step 4 above for the full procedure
# (rescue USB, fdisk -l, loadpbaimage, setsidpassword, setadmin1pwd)

# Configure for automated deployments
cd deploy
chmod +x setup-deploy.sh deploy.sh
sudo ./setup-deploy.sh
```

### Cron Job for Certificate Updates

Create `/etc/cron.d/pba-redeploy` to rebuild PBA when Proxmox certificates change:

```bash
# Every day at 3 AM, check if Proxmox certs changed and redeploy PBA
0 3 * * * root ~/sedunlocksrv/deploy/redeploy-if-cert-changed.sh
```

**Example script** (`~/sedunlocksrv/deploy/redeploy-if-cert-changed.sh`):

> **Important:** This script runs from cron, which has no SSH agent. You must configure
> a persistent agent socket or use a key file. See the troubleshooting section for details.

```bash
#!/bin/bash

CERT_PATH="/etc/pve/pveproxy-ssl.pem"
STATE_FILE="~/sedunlocksrv/.cert-serial.baseline"
SSH_KEY="$HOME/.ssh/id_ed25519"
TARGET="deploy@localhost"

# For SSH agent forwarding in cron, point to a persistent agent socket:
# export SSH_AUTH_SOCK=/run/user/$(id -u)/ssh-agent.sock

# Get current certificate serial number
CURRENT_SERIAL=$(openssl x509 -in "$CERT_PATH" -noout -serial 2>/dev/null | grep -oP '\d+$')

# Check if serial changed versus the last deploy's baseline
# Note: deploy.sh also manages this file internally (serial strategy, default);
# this outer check avoids starting the SSH connection when nothing has changed.
if [ ! -f "$STATE_FILE" ] || [ "$(cat "$STATE_FILE")" != "$CURRENT_SERIAL" ]; then
    echo "Certificate serial changed (or first run), rebuilding PBA..."
    
    # Deploy via SSH with agent forwarding
    # Option A: interactive prompt (requires -t for TTY)
    ssh -A -t -i "$SSH_KEY" "$TARGET" \
      'sudo ~/sedunlocksrv/deploy/deploy.sh \
        --cert-path=/etc/pve/pveproxy-ssl.pem \
        --key-path=/etc/pve/pveproxy-ssl-key.pem \
        --tls-server-name=pba.example.com'
    
    # Option B: pipe expert password (no shell escaping needed)
    # echo 'p@$$w0rd!' | ssh -A -i "$SSH_KEY" "$TARGET" \
    #   'sudo ~/sedunlocksrv/deploy/deploy.sh --expert-password-stdin \
    #     --cert-path=/etc/pve/pveproxy-ssl.pem \
    #     --key-path=/etc/pve/pveproxy-ssl-key.pem \
    #     --tls-server-name=pba.example.com'
    
    if [ $? -eq 0 ]; then
        echo "PBA redeployed successfully"
        # deploy.sh writes the baseline itself; no manual update needed here
    else
        echo "PBA deployment failed"
        exit 1
    fi
else
    echo "Certificate serial unchanged, no redeploy needed"
fi
```

---

## Troubleshooting

### "SSH key decryption failed" / "Failed to sign KDF challenge"

This means deploy.sh could not sign the challenge with the private key:

```bash
# Verify SSH agent is running and has keys loaded
ssh-add -l

# If empty, load your Ed25519 key
ssh-add ~/.ssh/id_ed25519

# For remote sessions, ensure agent forwarding is enabled
ssh -A -i ~/.ssh/id_ed25519 deploy@target-host

# Check which key was used during setup
cat ~/sedunlocksrv/.ssh/auth.conf

# Verify your key matches the stored fingerprint
ssh-keygen -lf ~/.ssh/id_ed25519.pub

# If keys don't match, re-run setup with the correct key
sudo ./setup-deploy.sh
```

**Common causes:**
- SSH agent not running or key not loaded (`ssh-add` required)
- Agent forwarding not enabled (`ssh -A` flag missing)
- Different SSH key than what was used during setup-deploy.sh
- ECDSA key used (not supported — use Ed25519)

### "Certificate and key do not match"

Full certificate chain doesn't match the private key:

```bash
# Verify they're from the same source
openssl x509 -noout -modulus -in /path/to/cert.pem | openssl md5
openssl pkey -noout -modulus -in /path/to/key.pem | openssl md5

# Modulus should be identical
```

### Build fails with "Missing dependencies"

Ensure build host has all required tools:

```bash
# For Ubuntu/Debian
sudo apt update
sudo apt install -y \
  build-essential gcc make \
  curl wget git \
  xorriso libarchive-tools \
  cpio rsync \
  jq openssh-client file unzip \
  dropbear-bin \
  grub-efi-amd64-bin grub-pc-bin grub-common \
  util-linux dosfstools openssl

# Do NOT install golang-go — the distro package is too old.
# Install Go 1.22+ from https://go.dev/dl/ e.g.:
curl -OL https://go.dev/dl/go1.26.1.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.26.1.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# Verify Go version
go version  # Should be 1.22+
```

### "Failed to write PBA image to /dev/nvme0"

Permissions or drive locked:

```bash
# Verify device is accessible
ls -la /dev/nvme0

# Check if drive is locked
sedutil-cli --password - listLockingRanges /dev/nvme0

# Verify sudoers (should show deploy.sh, build.sh, sedutil-cli)
sudo -l | grep sedutil-cli
```

---

## Security Checklist

- [ ] SSH private key is password-protected
- [ ] SSH private key is not stored on build host
- [ ] Encrypted password file (`~/sedunlocksrv/.ssh/opal-password.enc`) has mode 600
- [ ] Setup-deploy.sh was run as root with correct SSH public key
- [ ] Certificate renewal process is automated/reliable on remote host
- [ ] Deploy logs don't contain passwords (they use fingerprint-based decryption)
- [ ] Sudoers entry for `_sedunlocksrv` is minimal (only necessary commands)
- [ ] Old PBA images are backed up (see deploy.sh logs)

---

## Next Steps

Once this workflow is operational:

1. **Test first** — Use `--dry-run` flag to validate builds without flashing
2. **Monitor certificate expiry** — Set alerts for upcoming renewals
3. **Maintain backups** — Keep recent PBA images in case rollback is needed
4. **Document your certificate trigger** — So team members know how deployments happen
5. **Plan key rotation** — SSH key rotation should re-run setup-deploy.sh with new key

For detailed command reference, see:
- [deploy/README.md](deploy/README.md) — Complete deploy.sh documentation
- [deploy/QUICKSTART.md](deploy/QUICKSTART.md) — Step-by-step setup guide
