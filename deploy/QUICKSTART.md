# Quick Start: PBA Deployment

Get from zero to a working automated deployment in ~20 minutes.

For the full workflow (including initial OPAL drive setup), see [../DEPLOYMENT-WORKFLOW.md](../DEPLOYMENT-WORKFLOW.md).
For complete command-line reference, see [README.md](README.md).

## 1. Check Prerequisites

```bash
go version           # Go 1.22+
gcc --version        # build-essential
openssl version      # OpenSSL
which sedutil-cli    # sedutil-cli installed
lsblk | grep nvme    # OPAL NVMe drive visible
```

If anything is missing:
```bash
sudo apt update && sudo apt install -y \
    build-essential curl xorriso libarchive-tools cpio xz-utils \
    util-linux dosfstools openssl grub-common grub-efi-amd64-bin openssh-client
```

Install Go 1.22+ from [go.dev/dl](https://go.dev/dl/). Avoid the distro
`golang-go` package; it is usually too old for this repository.

## 2. Run Setup (one-time, as root)

```bash
cd ~/sedunlocksrv/deploy
chmod +x setup-deploy.sh deploy.sh
sudo ./setup-deploy.sh
```

The script will prompt you for:
1. **Service account name** (default: `_sedunlocksrv`)
2. **SSH public key** -- it searches `~/.ssh/` for Ed25519 keys and offers a menu
3. **OPAL admin password** -- typed interactively and confirmed

After setup, verify these files exist:
```bash
ls -la ~/sedunlocksrv/.ssh/opal-password.enc   # mode 600 -- in repo root, not deploy/
ls -la ~/sedunlocksrv/.ssh/auth.conf
```

> **Ed25519 keys only.** ECDSA and RSA are not supported (`setup-deploy.sh` will reject
> non-Ed25519 keys). Use `ssh-keygen -t ed25519` to generate a key if needed.

## 3. Test with Dry-Run

Always test before flashing to a live drive. The `--dry-run` flag builds the PBA image
but does not write to the drive.

SSH agent forwarding (`-A`) is **required** -- it allows the script to sign the KDF
challenge with your private key to decrypt the stored OPAL password:

```bash
ssh -A -i ~/.ssh/id_ed25519 deploy@target \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/fullchain.pem \
    --key-path=/path/to/key.pem \
    --dry-run'
```

Check the output for:
```
OK  Password decrypted using SSH key signature
OK  Build complete
```

If the dry-run succeeds, check the log:
```bash
ssh deploy@target 'tail -n 30 ~/sedunlocksrv/deploy-*.log'
```

## 4. First Real Deployment

```bash
ssh -A -i ~/.ssh/id_ed25519 deploy@target \
  '~/sedunlocksrv/deploy/deploy.sh \
    --cert-path=/path/to/fullchain.pem \
    --key-path=/path/to/key.pem'
```

Watch progress:
```bash
ssh -A deploy@target 'tail -f ~/sedunlocksrv/deploy-*.log'
```

Success message: `Deployment completed successfully`

## 5. Automate Certificate Updates

Once manual deployment works, set up automatic triggering when certificates renew.

**Example: Proxmox + cron**

Create `~/sedunlocksrv/deploy/redeploy-if-cert-changed.sh`:
```bash
#!/bin/bash
CERT_PATH="/etc/pve/pveproxy-ssl.pem"
STATE_FILE="~/sedunlocksrv/.cert-hash"
SSH_KEY="$HOME/.ssh/id_ed25519"
TARGET="deploy@localhost"

# For cron, point to a persistent SSH agent socket:
# export SSH_AUTH_SOCK=/run/user/$(id -u)/ssh-agent.sock

CURRENT_HASH=$(sha256sum "$CERT_PATH" | awk '{print $1}')
if [ ! -f "$STATE_FILE" ] || [ "$(cat "$STATE_FILE")" != "$CURRENT_HASH" ]; then
    ssh -A -i "$SSH_KEY" "$TARGET" \
      '~/sedunlocksrv/deploy/deploy.sh \
        --cert-path=/etc/pve/pveproxy-ssl.pem \
        --key-path=/etc/pve/pveproxy-ssl-key.pem'
    [ $? -eq 0 ] && echo "$CURRENT_HASH" > "$STATE_FILE"
fi
```

Register cron:
```bash
chmod +x ~/sedunlocksrv/deploy/redeploy-if-cert-changed.sh
echo '0 3 * * * root ~/sedunlocksrv/deploy/redeploy-if-cert-changed.sh' \
  > /etc/cron.d/pba-redeploy
```

See [../DEPLOYMENT-WORKFLOW.md](../DEPLOYMENT-WORKFLOW.md) for more trigger examples
(Certbot hook, systemd timer, CI/CD).

## Troubleshooting

### "Failed to sign KDF challenge"

The SSH agent must hold the Ed25519 key that was used during setup:
```bash
ssh-add -l                               # list loaded keys
ssh-add ~/.ssh/id_ed25519                # load key if missing
cat ~/sedunlocksrv/.ssh/auth.conf         # check registered key fingerprint
ssh-keygen -lf ~/.ssh/id_ed25519.pub    # verify fingerprint matches

# Wrong key? Re-run setup:
sudo ./setup-deploy.sh
```

### "opal-password.enc not found"

Setup did not complete successfully:
```bash
sudo ./setup-deploy.sh     # re-run setup
```

### "Build failed"

```bash
go version     # needs 1.22+
df -h /        # needs ~10 GB free
ssh deploy@target 'tail -n 100 ~/sedunlocksrv/deploy-*.log | grep -A5 ERROR'
```

### "Certificate and key do not match"

Cert and key must be from the same renewal:
```bash
openssl x509 -noout -modulus -in cert.pem | openssl md5
openssl pkey  -noout -modulus -in key.pem | openssl md5
# Output must be identical
```

### "sedutil-cli operation failed"

```bash
sedutil-cli --scan               # confirm drive detected
sedutil-cli --query /dev/nvme0   # verify OPAL 2.0 compliance and initialization
```

If the drive has never been initialized, the initial manual setup in
[../DEPLOYMENT-WORKFLOW.md](../DEPLOYMENT-WORKFLOW.md) Phase 1 must be completed first.
