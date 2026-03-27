# Password Complexity Build Flags - Implementation Complete ✅

## What Was Implemented

You now have full support for configurable password complexity requirements that apply to both the console TUI and web UI password change interfaces.

### 6 New Build Configuration Options

| Flag | Environment Variable | Default | Type | Purpose |
|------|---------------------|---------|------|---------|
| `--password-complexity` | `PASSWORD_COMPLEXITY_ON` | `on` | on/off or true/false | Master switch for all complexity (off = accept any password) |
| `--min-password-length` | `MIN_PASSWORD_LENGTH` | `12` | integer | Minimum required password length |
| `--require-upper` | `REQUIRE_UPPER` | `true` | true/false | Require uppercase letters (A-Z) |
| `--require-lower` | `REQUIRE_LOWER` | `true` | true/false | Require lowercase letters (a-z) |
| `--require-number` | `REQUIRE_NUMBER` | `true` | true/false | Require numeric digits (0-9) |
| `--require-special` | `REQUIRE_SPECIAL` | `true` | true/false | Require special characters (!@#$%...) |

## Quick Start Examples

### Build with No Password Complexity
```bash
./build.sh --password-complexity=off
```
Users can enter any password.

### Build with Custom Length Requirement Only
```bash
./build.sh \
  --min-password-length=20 \
  --require-upper=false \
  --require-lower=false \
  --require-number=false \
  --require-special=false
```
Users must enter 20+ character passwords (any content).

### Build with Strict Security (Default)
```bash
./build.sh
# Or explicitly:
./build.sh \
  --password-complexity=on \
  --min-password-length=12 \
  --require-upper=true \
  --require-lower=true \
  --require-number=true \
  --require-special=true
```
Users must enter 12+ character passwords with uppercase, lowercase, digits, and special characters.

## How It Works End-to-End

```
┌─────────────────────────────────────────────────────────────┐
│ 1. BUILD TIME                                               │
│ ./build.sh --password-complexity=off --min-password-length=16
│ (or set in build.conf)                                      │
└─────────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────────┐
│ 2. IMAGE GENERATION                                         │
│ build.sh writes values to /etc/sedunlocksrv.conf in PBA     │
│ PASSWORD_COMPLEXITY_ON="off"                                │
│ MIN_PASSWORD_LENGTH="16"                                    │
│ REQUIRE_UPPER="false"                                       │
│ etc...                                                      │
└─────────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────────┐
│ 3. PBA BOOT                                                 │
│ tc-config script sources /etc/sedunlocksrv.conf            │
│ Exports all PASSWORD_* variables as env vars               │
└─────────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────────┐
│ 4. RUNTIME POLICY LOADING                                  │
│ Go program reads env vars via loadPolicy()                 │
│ Builds PasswordPolicy struct with configured requirements   │
└─────────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────────┐
│ 5. USER INTERFACE DISPLAY                                   │
│ Console: "Requirements: no complexity requirements"         │
│ Web API: /password-policy returns JSON policy              │
└─────────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────────┐
│ 6. PASSWORD VALIDATION                                      │
│ User enters password                                        │
│ validatePassword() checks against loaded policy            │
│ Accepts or rejects based on configuration                  │
└─────────────────────────────────────────────────────────────┘
```

## Files Modified

### Build System
- **build.sh**
  - Added password policy variable defaults (lines ~63-68)
  - Added CLI flag parsing (lines ~193-200)
  - Added validation for password policy values (lines ~236-255)
  - Added variables to sedunlocksrv.conf template (lines ~580-586)

- **build.conf.example**
  - Added password policy documentation with examples

### Configuration Files
- **tc/sedunlocksrv.conf**
  - Added all 6 password policy variables with documentation
  - Shown as defaults meant to be overridden

- **tc/tc-config**
  - Added password policy variable declarations
  - Added exports for all 6 variables

### Go Application
- **sedunlocksrv/main.go**
  - Enhanced `loadPolicy()` function:
    - Checks `PASSWORD_COMPLEXITY_ON` master flag
    - If disabled, returns all-zeros policy (no requirements)
    - If enabled, applies individual requirements
    - Accepts both "true"/"false" and "on"/"off" syntax
  - Enhanced `passwordPolicySummary()` function:
    - Shows "no complexity requirements" when disabled
    - Dynamically builds requirement list
    - Handles zero minimum length

- **sedunlocksrv/main_test.go**
  - Added 5 comprehensive unit tests:
    - `TestPasswordPolicyComplexityDisabled`: Complexity off
    - `TestPasswordPolicyComplexityEnabledWithDefaults`: Default values
    - `TestPasswordPolicyCustomRequirements`: Custom settings
    - `TestPasswordComplexityOffAcceptsSimplePassword`: Off behavior
    - `TestPasswordComplexityBooleanVariationsSyntax`: Boolean variants

### Documentation
- **PASSWORD_POLICY_CONFIG.md** (NEW)
  - Complete configuration guide
  - Usage examples for all scenarios
  - Technical implementation details
  - Troubleshooting section
  - Testing instructions

## UI Integration - Already Works!

### Console TUI
When user presses "P" for password change, they see:
```
Requirements: min 12 chars, uppercase, lowercase, number, special
BIOS note: if SID password changes fail, check firmware/TPM settings...
Target device (...):
```

Or if disabled:
```
Requirements: no complexity requirements
```

### Web UI
- The `/password-policy` API endpoint returns the policy as JSON
- The web interface (index.html) already queries this endpoint
- Requirements are displayed to users before they set a password
- Works automatically - no changes needed to UI code!

## Configuration Priority

1. **build.conf** - Set before building
2. **CLI flags** - Override build.conf (e.g., `./build.sh --min-password-length=20`)
3. **Runtime env** - Can be modified in /etc/sedunlocksrv.conf after image creation

## Testing

All changes include comprehensive test coverage:

```bash
cd sedunlocksrv
go test -v -run "Password"
```

Tests verify:
- ✅ Complexity can be disabled
- ✅ Individual requirements can be toggled
- ✅ Default values apply when not set
- ✅ Custom length requirements work
- ✅ Boolean syntax variations (true/false/on/off) work
- ✅ Invalid passwords rejected when complexity enabled
- ✅ Simple passwords accepted when complexity disabled

## Backward Compatibility

✅ **100% backward compatible:**
- Default behavior unchanged (12 chars + all complexity types)
- Existing builds continue to work as before
- API compatible with existing web UI clients
- No breaking changes to any function signatures

## Example Build Commands

### Default (strict security)
```bash
./build.sh
```

### Minimal security
```bash
./build.sh --password-complexity=off
```

### Moderate security (16 chars, no complexity)
```bash
./build.sh --min-password-length=16 --require-upper=false --require-lower=false --require-number=false --require-special=false
```

### Via build.conf
```bash
cat >> build.conf <<EOF
PASSWORD_COMPLEXITY_ON="true"
MIN_PASSWORD_LENGTH="14"
REQUIRE_UPPER="true"
REQUIRE_LOWER="true"
REQUIRE_NUMBER="true"
REQUIRE_SPECIAL="true"
EOF
./build.sh --config=build.conf
```

## Feature Summary

| Feature | Status | Details |
|---------|--------|---------|
| Master complexity on/off switch | ✅ | PASSWORD_COMPLEXITY_ON env var |
| Configurable minimum length | ✅ | MIN_PASSWORD_LENGTH (default 12) |
| Uppercase requirement toggle | ✅ | REQUIRE_UPPER (default true) |
| Lowercase requirement toggle | ✅ | REQUIRE_LOWER (default true) |
| Number requirement toggle | ✅ | REQUIRE_NUMBER (default true) |
| Special char requirement toggle | ✅ | REQUIRE_SPECIAL (default true) |
| Build-time configuration | ✅ | CLI flags or build.conf |
| Runtime display (Console TUI) | ✅ | Shows in 'P' password change menu |
| Runtime display (Web API) | ✅ | /password-policy endpoint |
| Unit test coverage | ✅ | 5 test cases |
| Documentation | ✅ | PASSWORD_POLICY_CONFIG.md |

## Next Steps

1. ✅ Review the password policy configuration: [PASSWORD_POLICY_CONFIG.md](PASSWORD_POLICY_CONFIG.md)
2. ✅ Build with desired configuration: `./build.sh --password-complexity=off`
3. ✅ Test the PBA image to verify requirements are enforced
4. ✅ Check console TUI displays correct requirements
5. ✅ Verify web UI shows policy via `/password-policy` endpoint

## Related Commits

```
c2019a4 - feat: add password complexity build flags and runtime configuration
```

---

**Status:** ✅ **COMPLETE AND TESTED**

All requested password complexity features have been implemented and integrated with both console TUI and web UI interfaces. Configuration is flexible and backward-compatible.
