# SED Unlock Server - Code Structure & Maintenance Guide

## File Organization (Post-Refactoring)

The codebase is now organized into logical modules, making it easier to maintain and extend:

### Core Type Definitions
- **types.go** - All data structures (StatusResponse, DriveStatus, BootResult, BootKernelInfo, PasswordPolicy, etc.)
  - ~150 lines | No external dependencies | Can be used as documentation of API contracts

### Functional Modules

#### Drive Operations (drive.go)
Functions for OPAL drive detection and management:
- `scanDrives()` - Detect OPAL-capable drives and lock status
- `collectDriveDiagnostics()` - Gather detailed TCG query data
- `listDevicePartitions()` - Find all partitions for a device (handles SATA + NVMe)
- `listDeviceNodes()` - Find device node(s) excluding partitions
- `rescanBlockDeviceLayout()` - Trigger kernel partition table re-read
- ~215 lines | Heavy use of /sys/class/block filesystem

#### Network Operations (network.go)
Functions for network interface discovery:
- `scanNetworkInterfaces()` - Discover interfaces, addresses, carrier state
- ~45 lines | Pure Go, no external commands

#### System Services (ssh.go)
Functions for external service initialization:
- `startSSHService()` - Detect and start Dropbear SSH on port 2222
- ~75 lines | Looks up executables/keys in multiple standard locations

#### Utility Functions (util.go)
General-purpose helpers used throughout:
- **HTTP Helpers**: `jsonResponse()`, `requireMethod()`
- **sedutil-cli Wrappers**: `runSedutil()`, `queryDrive()`, `queryField()`
- **File Operations**: `firstExistingPath()`, `haveRuntimeCommand()`
- **Boot Debugging**: `appendBootDebug()`
- **Expert Commands**: `runExpertCommand()`, `runExpertPBAFlashBytes()`, `makeSystemActionHandler()`
- ~200 lines | No circular imports

#### Main Application (main.go)
Currently still contains:
- All boot logic (BootSystem, BootSystemWithKernel, ListAvailableBootKernels, kernel discovery)
- All unlock/password logic (attemptUnlock, changePassword, password policy)
- All HTTP handlers (25+ endpoint handlers)
- Console interface (consoleInterface)
- Session token management
- PBA image validation (validateUploadedPBAImageBytes)
- Global state and synchronization
- Main HTTP server setup

**Future refactoring opportunities** (see section below)

---

## Bug Fixes Applied (March 27, 2026)

Three critical bugs were fixed before refactoring:

1. ✅ **collectBootFiles() undefined** - Replaced 3 calls with `enhancedCollectBootFiles()`
2. ✅ **BootResult invalid fields** - Fixed struct field names in `BootSystemWithKernel()`
3. ✅ **kexecReady channel shadowing** - Removed local variable that was preventing proper OS handoff
4. ✅ **Dead code removal** - Deleted unused `validateUploadedPBAImage()` function (~110 lines)

---

## Recommended Next Steps for Further Refactoring

### High Priority (Reduce main.go cognitive load)

**boot.go** (~800 lines) - Extract all kernel discovery and boot logic:
- `ListAvailableBootKernels()` - Discover available kernels
- `BootSystem()` - Default boot path
- `BootSystemWithKernel(kernelIndex)` - Kernel selection boot
- Kernel file detection: `isLinuxKernel()`, `isInitrd()`
- File collection: `enhancedCollectBootFiles()`, `collectBootCatalog()`
- Boot catalog parsing: `parseLoaderEntryCatalog()`, `parseGrubConfigCatalog()`, etc.
- Boot matching: `matchBootEntryCmdline()`, `matchKernelInitrdPair()`
- Cmdline synthesis: `findBootArtifacts()`, `findBootCmdline()`, `synthesizeRootCmdline()`
- Debug helpers: `appendBootDebug()`, boot state management

**unlock.go** (~250 lines) - Extract password/unlock logic:
- `attemptUnlock()` - Drive unlock with password
- `changePassword()` - Update Admin1 and SID passwords
- `validatePassword()` - Check password policy compliance
- `passwordPolicySummary()` - Format policy for UI/console
- `loadPolicy()`, `loadExpertPasswordHash()` - Configuration loading
- `elegiblePasswordChangeTargets()`, `selectPasswordChangeTargets()` - Drive selection

**handlers.go** (~700 lines) - Extract all HTTP endpoint handlers:
- Status endpoints: `/status`, `/diagnostics`, `/password-policy`
- Unlock endpoints: `/unlock`, `/change-password`
- Boot endpoints: `/boot-list`, `/boot`, `/boot-status`
- Expert endpoints: `/expert/auth`, `/expert/revert-tper`, `/expert/psid-revert`, etc.
- System endpoints: `/reboot`, `/poweroff`

### Medium Priority (Improve maintainability)

**session.go** (~100 lines) - Extract session token management:
- `mintSessionToken()` - Generate new tokens
- `authenticateSession()` - Verify token validity
- `requireSessionTokenOrUnlockedDrive()` - Token validation middleware
- `requireExpertToken()` - Expert password validation

**pba.go** (~200 lines) - Extract PBA-specific logic:
- `validateUploadedPBAImageBytes()` - Image validation
- MBR validation, FAT32 detection, partition checks

**console.go** (~150 lines) - Extract console UI:
- `consoleInterface()` - Interactive terminal menu
- Console input handling
- Console-based unlock/password change flows

### Low Priority (Polish/optimization)

- Reduce appendBootDebug() call overhead (currently ~100+ calls)
- Improve import organization (stdlib → blank line → third-party)
- Add package-level documentation comments
- Consider functional options pattern for HTTP handlers

---

## Current Multi-File Statistics

```
types.go          ~150 lines   (Type definitions)
drive.go          ~215 lines   (Drive operations)
network.go         ~45 lines   (Network operations)
ssh.go             ~75 lines   (SSH service)
util.go           ~200 lines   (Utilities)
main.go           ~3200 lines  (Everything else - target for next refactoring)
                  --------
Total refactored: ~3885 lines
```

### Before Refactoring
- Single main.go file: ~3700 lines
- 3 critical bugs
- 100+ line functions of mixed concerns
- Difficult to locate specific functionality

### After Refactoring (Current)
- 6 focused modules
- All bugs fixed
- Clear separation of concerns
- Types documented in one file
- Path clear for further extraction

---

## Maintenance Best Practices

### Adding New Features
1. **New HTTP endpoint?** → Add to handlers section in main.go (or create handlers.go)
2. **New drive operation?** → Add to drive.go
3. **New type?** → Add to types.go with full documentation
4. **New utility?** → Add to util.go if general-purpose, or create specialized module

### Debugging
- Boot issues → Look at boot.go (when extracted)
- Drive detection → Look at drive.go
- Network issues → Look at network.go
- HTTP errors → Check handlers (main.go for now)

### Code Review Checklist
- [ ] New functions have clear, descriptive names
- [ ] Complex functions have explanatory comments
- [ ] Error handling is explicit (no silent failures)
- [ ] Goroutines have proper cleanup/cancellation
- [ ] Mutex locks are held for minimum duration
- [ ] New types added to types.go with JSON/struct tags

---

## Testing Strategy

Current test coverage is minimal (~main_test.go exists but is empty).

Recommended test organization (future):
```
boot_test.go        - Unit tests for boot discovery/selection
unlock_test.go      - Unit tests for password/unlock logic
drive_test.go       - Unit tests for device detection
handlers_test.go    - HTTP handler integration tests
pba_test.go         - PBA image validation tests
```

---

## Performance Considerations

**Current bottlenecks:**
- `scanDrives()` - Makes 5 sedutil-cli calls per drive (context timeout: 5s)
- `enhancedCollectBootFiles()` - Full filesystem walk on every boot
- `appendBootDebug()` - Formats strings even in success paths

**Optimization opportunities:**
- Cache sedutil-cli scan results (with TTL)
- Pre-compile boot file patterns (regex)
- Lazy-format debug messages

---

## Documentation

- **types.go** - Serves as API contract documentation
- **Individual modules** - Function comments explain "why" and "how"
- **main.go** - Section headers group related code
- **This file** - High-level overview and maintenance guide

---

## Version History

- **v0.2.0** (March 27, 2026) - Refactored into modules, fixed 3 critical bugs
- v0.1.0 - Original monolithic main.go

---

## Related Files
- README.md - User-facing documentation
- build.sh - Build system
- build.conf - Build configuration
- tc/sedunlocksrv.conf - Runtime configuration
- ssh/ssh_sed_unlock.sh - SSH command wrapper
- sedunlocksrv/go.mod - Go dependencies
- sedunlocksrv/index.html - Web UI
