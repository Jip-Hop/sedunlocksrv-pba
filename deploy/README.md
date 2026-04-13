# PBA Deployment System

Automation for building and flashing OPAL 2.0 PBA images with updated TLS certificates.

> **New to this project?** See [QUICKSTART.md](QUICKSTART.md) for a step-by-step setup guide, or [../DEPLOYMENT-WORKFLOW.md](../DEPLOYMENT-WORKFLOW.md) for the complete end-to-end workflow.

## Prerequisite: Manual Initial Flash Required

`deploy.sh` is for **updating** an already-initialized OPAL drive. Before using it, you must:
1. Manually build the initial PBA image with `../build.sh`
2. Manually flash it to the OPAL drive using a sedutil recovery USB
3. Set your OPAL admin password during that initial setup

Only after that does `deploy.sh` make sense. See [../DEPLOYMENT-WORKFLOW.md](../DEPLOYMENT-WORKFLOW.md) Phase 1.

## Files in This Directory

| File | Purpose |
|------|---------|
| `deploy.sh` | Main deployment orchestrator (build -> validate -> flash) |
| `setup-deploy.sh` | Interactive one-time setup (service account, sudoers, password encryption) |
| `README.md` | This file - complete reference |
| `QUICKSTART.md` | Step-by-step setup and first deployment guide |
| `CERTIFICATE-FRESHNESS.md` | Race condition protection for automated cert pipelines |

## What deploy.sh Does

1. **Validates** TLS certificates and OPAL drive (OPAL 2.0 compliance, shadow MBR enabled)
2. **Waits** for certificate freshness (configurable strategy -- see [CERTIFICATE-FRESHNESS.md](CERTIFICATE-FRESHNESS.md))
3. **Builds** new PBA image via `../build.sh` with the supplied certificates
4. **Validates** generated image structure (MBR signature, FAT32, boot sector)
5. **Flashes** validated PBA to OPAL 2.0 drive shadow MBR via `sedutil-cli`
6. **Logs** everything to `deploy-TIMESTAMP.log` in the repository root

## Password Security

The OPAL admin password is encrypted at rest using AES-256-CBC. The encryption key is derived
by signing a deterministic challenge (`sedunlocksrv-kdf-{salt}`) with your Ed25519 SSH private
key via `ssh-keygen -Y sign`. This means:

- Password never stored in plaintext
- Decryption requires the SSH private key -- the public key and salt alone are not sufficient
- Password decrypts automatically when deploying via `ssh -A` (agent forwarding)
- No passwords in SSH commands, process list, or logs
- **Ed25519 keys only** -- ECDSA and RSA are not supported (ECDSA: non-deterministic; RSA: rejected by setup-deploy.sh)

**Security caveat:** `sedutil-cli` accepts the OPAL password as a command-line argument, making
it briefly visible in `ps aux` during the flash step. This is a known limitation of `sedutil-cli`.

### How the encryption works

```
Setup:    random salt generated
          challenge = "sedunlocksrv-kdf-{salt}"
          signature = ssh-keygen -Y sign(challenge, private_key)
          enc_key   = sha256(signature)
          openssl enc -aes-256-cbc -k enc_key ... -> opal-password.enc

Deploy:   same challenge + same private key -> same signature -> same enc_key
          openssl dec(opal-password.enc, enc_key) -> OPAL password (in memory only)
```

Files stored on host (in repository root `.ssh/`, i.e. `~/sedunlocksrv/.ssh/`):
- `.ssh/opal-password.enc` -- encrypted password (mode 600)
- `.ssh/signing-key.pub` -- SSH public key
- `.ssh/auth.conf` -- salt and metadata (mode 644)

## Setup

Run once as root on the host with the OPAL drive:

```bash
cd ~/sedunlocksrv/deploy
sudo ./setup-deploy.sh
```

What it configures:
- Service account (`_sedunlocksrv`) and home directory
- Sudoers entry for `deploy.sh`, `build.sh`, `/usr/bin/sedutil-cli` (only these three commands)
- Searches `~/.ssh/` for Ed25519 public keys; prompts to select or provide a path
- Prompts for OPAL admin password, encrypts and stores it
- Validates all required tools are present

## Usage

### Deploying via SSH

SSH agent forwarding (`-A`) is required so the script can sign the KDF challenge with your private key:

```bash
# Dry-run first -- builds PBA but does NOT flash to drive
ssh -A -i ~/.ssh/id_ed25519 deploy@target \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/fullchain.pem \
    --key-path=/path/to/key.pem \
    --dry-run'

# Real deployment
ssh -A -i ~/.ssh/id_ed25519 deploy@target \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/fullchain.pem \
    --key-path=/path/to/key.pem'
```

### With custom build options

```bash
ssh -A -i ~/.ssh/id_ed25519 deploy@target \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/fullchain.pem \
    --key-path=/path/to/key.pem \
    --build-args=--ssh,--net-mode=bond,--debug-level=0'
```

### Automation

**Cron job** (check for cert changes and redeploy):
```bash
# /etc/cron.d/pba-redeploy -- runs daily at 3 AM
0 3 * * * root ~/sedunlocksrv/deploy/redeploy-if-cert-changed.sh
```

See [../DEPLOYMENT-WORKFLOW.md](../DEPLOYMENT-WORKFLOW.md) for a complete example cron script
that hashes the certificate to detect changes.

**Systemd timer:**
```ini
# /etc/systemd/system/sedunlocksrv-refresh.service
[Service]
Type=oneshot
User=deploy
ExecStart=/usr/bin/ssh -A -i ~/.ssh/id_ed25519 \
    target-host '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/etc/pve/pveproxy-ssl.pem \
    --key-path=/etc/pve/pveproxy-ssl-key.pem'
```

**CI/CD (GitHub Actions, GitLab CI):**
```bash
ssh -A -i "$DEPLOY_KEY" deploy@target \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path="$CERT_PATH" \
    --key-path="$KEY_PATH"'
```

## Command-Line Reference

```
deploy.sh --cert-path=PATH --key-path=PATH [OPTIONS]

Required:
  --cert-path=PATH           Absolute path to fullchain.pem
  --key-path=PATH            Absolute path to key.pem

Optional:
  --build-args=ARGS          Comma-separated additional arguments for build.sh
                              e.g. --build-args=--ssh,--net-mode=bond
  --opal-drive=DEVICE        Target OPAL drive (default: /dev/nvme0)
  --dry-run                  Build and validate only, do not flash to drive
  --cert-freshness=METHOD    hash|mtime|serial|marker|none (default: hash)
  --cert-timeout=SECS        Max seconds to wait for cert update (default: 300)
  --cert-grace=SECS          Grace period for mtime strategy (default: 10)
  --quiet                    Suppress non-error output
  --help                     Show usage and exit

Environment variables:
  OPAL_DRIVE                 Override target drive (/dev/nvme0)
  SYSLOG_TAG                 Custom syslog tag (default: deploy.sh)
```

## Logging

All output is written to a timestamped log file in the **repository root** (one level above `deploy/`):

```bash
tail -f ~/sedunlocksrv/deploy-*.log
```

Events are also sent to syslog:

```bash
journalctl -u sedunlocksrv-deploy -f
journalctl -u sedunlocksrv-deploy -p err     # errors only
```

## File Structure (after setup)

```
~/sedunlocksrv/
+-- build.sh                     <- PBA image builder
+-- sedunlocksrv/                <- Go source
+-- .ssh/
|   +-- opal-password.enc        <- Encrypted OPAL password (600)
|   +-- signing-key.pub          <- SSH public key
|   +-- auth.conf                <- Salt and metadata (644)
+-- deploy-TIMESTAMP.log         <- Per-deployment log
+-- deploy/
    +-- deploy.sh                <- Deployment orchestrator
    +-- setup-deploy.sh          <- One-time setup
/etc/sudoers.d/sedunlocksrv      <- Created by setup-deploy.sh
```

## Prerequisites

```bash
# Debian/Ubuntu
sudo apt update && sudo apt install -y \
    build-essential curl xorriso bsdtar cpio xz-utils \
    util-linux mount fdisk dosfstools gzip rsync libarchive-tools \
    grub-common grub-pc-bin grub-efi-amd64-bin grub-efi-ia32-bin \
    openssl openssh-client jq file unzip git
```

Also required: Go 1.22+ (install from [go.dev/dl](https://go.dev/dl), not `golang-go`), `sedutil-cli` in `$PATH`, and an initialized OPAL 2.0 drive.

## Troubleshooting

### "Failed to sign KDF challenge" / SSH key decryption fails

The SSH agent must hold the Ed25519 key that was used during setup:

```bash
ssh-add -l                                              # verify key is loaded in agent
ssh-add ~/.ssh/id_ed25519                               # load it if missing
cat ~/sedunlocksrv/.ssh/auth.conf                    # check which key was registered
ssh-keygen -lf ~/.ssh/id_ed25519.pub                    # confirm fingerprint matches

# If wrong key was used during setup, re-run:
sudo ./setup-deploy.sh
```

### "Certificate and key do not match"

```bash
openssl x509 -noout -modulus -in cert.pem | openssl md5
openssl pkey  -noout -modulus -in key.pem | openssl md5
# Both lines must produce identical output
```

### "sedutil-cli: OPAL operation failed"

```bash
sedutil-cli --scan                           # confirm drive is detected
sedutil-cli --query /dev/nvme0               # verify OPAL 2.0 compliance
# If OPAL not yet initialized, complete the initial setup manually first
```

### "No PBA image generated by build.sh"

```bash
go version   # needs Go 1.21+
df -h /      # check disk space (cache needs ~10 GB)
# Check build log for details:
tail -n 100 ~/sedunlocksrv/deploy-*.log | grep -A5 "ERROR\|FAIL"
```