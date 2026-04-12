# Migration Summary: Deployment System Reorganized

## ✅ What Changed

All PBA deployment files have been moved to a new `deploy/` subdirectory for better organization.

### Directory Structure Before
```
/opt/sedunlocksrv/
├── build.sh
├── deploy.sh              ← Deployment scripts
├── setup-deploy.sh        ← in root
├── DEPLOY.md              ← Documentation
├── QUICKSTART.md          ← files scattered
├── DEPLOYMENT-SYSTEM.md   ← in root
├── ... (more .md files)
└── sedunlocksrv/
```

### Directory Structure After ✨
```
/opt/sedunlocksrv/
├── build.sh               ← Still in root (called by deploy.sh)
├── make-cert.sh           ← Build utilities
├── Dockerfile
├── LICENSE
├── README.md              ← Original project README
├── sedunlocksrv/          ← Source code
├── ssh/
├── tc/
└── deploy/                ← ✅ NEW: All deployment files here
    ├── deploy.sh          ← Main orchestrator
    ├── setup-deploy.sh    ← Interactive setup
    ├── README.md          ← Quick reference
    ├── QUICKSTART.md      ← Start here (5 min)
    ├── DEPLOY.md          ← Full reference
    ├── PROXMOX-SETUP.md   ← Proxmox integration
    ├── DEPLOYMENT-SYSTEM.md
    ├── README-DEPLOYMENT.md
    ├── IMPLEMENTATION-CHECKLIST.md
    └── FILE-INVENTORY.md
```

## 🔄 What Was Updated

### Scripts Updated with New Paths

**deploy.sh**
- `SCRIPT_DIR` → Gets own location
- `REPO_ROOT` → Points to parent directory (`..`)
- Calls `../build.sh` (one level up)
- Logs and backups in `$REPO_ROOT/` (parent directory)

**setup-deploy.sh**
- `DEPLOY_SCRIPT` → Now `/opt/sedunlocksrv/deploy/deploy.sh`
- Sudoers rule updated to `/opt/sedunlocksrv/deploy/deploy.sh`
- Certbot hook points to new location

## 📁 File Locations

### Deployment Files (in `deploy/`)
- `deploy.sh` - Main orchestrator ✨
- `setup-deploy.sh` - Setup helper ✨
- All documentation (.md files)

### Build/Source Files (parent directory)
- `build.sh` - PBA builder (unchanged)
- `sedunlocksrv/` - Go source (unchanged)
- `ssh/`, `tc/` - Additional sources (unchanged)

### Output/Logs (parent directory)
- `deploy-TIMESTAMP.log` - Deployment logs
- `pba-backups/` - Backed-up PBA images
- `sedunlocksrv-pba-*.img` - Generated PBA images

## 🚀 How to Use

### Running deploy.sh from new location:

**Option 1: From the deploy directory**
```bash
cd /opt/sedunlocksrv/deploy
sudo ./setup-deploy.sh                    # One-time setup
./deploy.sh --cert-path=... --key-path=...  # Deploy
```

**Option 2: From parent directory**
```bash
cd /opt/sedunlocksrv
sudo ./deploy/setup-deploy.sh
./deploy/deploy.sh --cert-path=... --key-path=...
```

**Option 3: Using full path (from anywhere)**
```bash
/opt/sedunlocksrv/deploy/deploy.sh --cert-path=... --key-path=...
```

### Certbot hook (auto-updated by setup.sh)
```bash
# Runs: /opt/sedunlocksrv/deploy/deploy.sh
# Automatically on certificate renewal
```

## ✔️ Verification

All scripts have been tested for:
- ✅ Correct relative paths to parent directory
- ✅ Correct absolute paths in sudoers
- ✅ Logs written to parent directory
- ✅ build.sh called correctly
- ✅ Certbot hooks reference new location

## 📖 Getting Started

1. **Go to deploy directory**
   ```bash
   cd /opt/sedunlocksrv/deploy
   ```

2. **Read the quick start guide**
   ```bash
   cat README.md
   cat QUICKSTART.md
   ```

3. **Run setup (one-time)**
   ```bash
   sudo ./setup-deploy.sh
   ```

4. **Test deployment**
   ```bash
   export OPAL_PASSWORD="your-password"
   ./deploy.sh --cert-path=/path/to/cert.pem --key-path=/path/to/key.pem --dry-run
   ```

## 🔗 Path Changes Summary

| Component | Old Path | New Path | Notes |
|-----------|----------|----------|-------|
| Main script | `/opt/sedunlocksrv/deploy.sh` | `/opt/sedunlocksrv/deploy/deploy.sh` | ✨ Updated |
| Setup script | `/opt/sedunlocksrv/setup-deploy.sh` | `/opt/sedunlocksrv/deploy/setup-deploy.sh` | ✨ Updated |
| Documentation | `/opt/sedunlocksrv/*.md` | `/opt/sedunlocksrv/deploy/*.md` | ✨ Moved |
| build.sh call | `./build.sh` | `../build.sh` | ✨ Updated in deploy.sh |
| Sudoers entry | `/opt/sedunlocksrv/deploy.sh` | `/opt/sedunlocksrv/deploy/deploy.sh` | ✨ Updated |
| Certbot hook | References old path | Updated by setup.sh | ✨ Auto-fixed |
| Logs/backups | Repo root | Repo root | Unchanged (still parent dir) |

## 🔄 Backward Compatibility

- Old paths no longer work (intentional cleanup)
- All scripts automatically use correct paths
- Setup helper updates everything needed

## ⚠️ If You Have Existing Setup

If you had previously run `setup-deploy.sh`, you'll need to:

1. **Update sudoers** (or re-run setup)
   ```bash
   sudo /opt/sedunlocksrv/deploy/setup-deploy.sh
   ```

2. **Update Certbot hook** (or re-run setup)
   - Hook will automatically reference new deploy.sh location

3. **Test everything**
   ```bash
   /opt/sedunlocksrv/deploy/deploy.sh --dry-run ...
   ```

## 💡 Benefits of This Organization

- ✅ **Cleaner structure** - Deployment files grouped together
- ✅ **Easier to find** - Know to look in `deploy/` for deployment docs
- ✅ **Easier to version** - Can manage deploy system separately
- ✅ **Build artifacts separate** - Logs/backups stay in root
- ✅ **Professional layout** - Clear separation of concerns

## 📝 Documentation Index (all in deploy/)

| File | Purpose |
|------|---------|
| `README.md` | Quick reference for deploy/ directory |
| `QUICKSTART.md` | 5-minute setup guide ⭐ START HERE |
| `DEPLOY.md` | Complete reference (options, troubleshooting) |
| `PROXMOX-SETUP.md` | Proxmox-specific integration |
| `DEPLOYMENT-SYSTEM.md` | Architecture & features |
| `README-DEPLOYMENT.md` | System overview |
| `IMPLEMENTATION-CHECKLIST.md` | Implementation tracking |
| `FILE-INVENTORY.md` | Detailed file descriptions |

---

## ✨ Ready to Use!

All files are organized and ready. Start with:
```bash
cd /opt/sedunlocksrv/deploy
sudo ./setup-deploy.sh
```

See `README.md` or `QUICKSTART.md` for next steps!
