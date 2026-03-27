# Password Complexity Configuration Guide

## Overview

The sedunlocksrv PBA now supports configurable password complexity requirements. These can be set at build time and are reflected in both the console TUI and web UI interfaces when users change passwords on unlocked OPAL drives.

## Configuration Options

### 1. PASSWORD_COMPLEXITY_ON (true/false or on/off)
**Default:** `true`  
**Description:** Master switch to enable/disable password complexity enforcement entirely.
- `true` or `on`: Enforce requirements configured below
- `false` or `off`: No password complexity requirements (any password accepted)

### 2. MIN_PASSWORD_LENGTH (integer)
**Default:** `12`  
**Description:** Minimum password length required. Only enforced when `PASSWORD_COMPLEXITY_ON=true`.

### 3. REQUIRE_UPPER (true/false)
**Default:** `true`  
**Description:** Require uppercase letters (A-Z). Only enforced when `PASSWORD_COMPLEXITY_ON=true`.

### 4. REQUIRE_LOWER (true/false)
**Default:** `true`  
**Description:** Require lowercase letters (a-z). Only enforced when `PASSWORD_COMPLEXITY_ON=true`.

### 5. REQUIRE_NUMBER (true/false)
**Default:** `true`  
**Description:** Require numeric digits (0-9). Only enforced when `PASSWORD_COMPLEXITY_ON=true`.

### 6. REQUIRE_SPECIAL (true/false)
**Default:** `true`  
**Description:** Require special characters. Only enforced when `PASSWORD_COMPLEXITY_ON=true`.

## Setting Configuration

### Method 1: Build Configuration File (build.conf)

```bash
# Copy build.conf.example to build.conf and edit
cp build.conf.example build.conf

# Add password policy settings
PASSWORD_COMPLEXITY_ON="true"
MIN_PASSWORD_LENGTH="14"
REQUIRE_UPPER="true"
REQUIRE_LOWER="true"
REQUIRE_NUMBER="true"
REQUIRE_SPECIAL="true"

# Then build
./build.sh --config=build.conf
```

### Method 2: CLI Flags

```bash
./build.sh \
  --password-complexity=on \
  --min-password-length=14 \
  --require-upper=true \
  --require-lower=true \
  --require-number=true \
  --require-special=true
```

### Method 3: Environment Variables (at runtime)

If you manually manage the `/etc/sedunlocksrv.conf` file in the PBA image or rootfs:

```bash
cat >> /etc/sedunlocksrv.conf <<EOF
PASSWORD_COMPLEXITY_ON="true"
MIN_PASSWORD_LENGTH="14"
REQUIRE_UPPER="true"
REQUIRE_LOWER="true"
REQUIRE_NUMBER="true"
REQUIRE_SPECIAL="true"
EOF
```

## Examples

### Scenario 1: Minimal Password Requirement (No Complexity)
```bash
./build.sh --password-complexity=off
```
**Result:** Users can enter any password, including single characters.

### Scenario 2: High Security (Strict Requirements)
```bash
./build.sh \
  --password-complexity=on \
  --min-password-length=20 \
  --require-upper=true \
  --require-lower=true \
  --require-number=true \
  --require-special=true
```
**Result:** Password must be 20+ chars with uppercase, lowercase, digit, and special character.

### Scenario 3: Moderate Security (Length-Based Only)
```bash
./build.sh \
  --password-complexity=on \
  --min-password-length=16 \
  --require-upper=false \
  --require-lower=false \
  --require-number=false \
  --require-special=false
```
**Result:** Password must be 16+ chars, any content.

### Scenario 4: Default Complexity
```bash
./build.sh
# Uses built-in defaults:
# - Minimum 12 characters
# - Uppercase required
# - Lowercase required
# - Numbers required
# - Special characters required
```

## User Interface Display

### Console UI (During Password Change)
When a user presses "P" for password change, they see:
```
Requirements: min 12 chars, uppercase, lowercase, number, special
```

Or if complexity is disabled:
```
Requirements: no complexity requirements
```

### Web UI
The `/password-policy` endpoint returns the configured policy as JSON:
```json
{
  "minLength": 12,
  "requireUpper": true,
  "requireLower": true,
  "requireNumber": true,
  "requireSpecial": true
}
```

The web UI (index.html) displays these requirements to the user before they set a password.

## Technical Implementation

### Architecture
1. **Build Time:** Configuration is set via CLI flags or build.conf
2. **Image Generation:** Values are written to `/etc/sedunlocksrv.conf` in the PBA image
3. **Runtime:** The tc-config init script sources the conf file and exports environment variables
4. **Policy Loading:** The Go binary reads environment variables via `loadPolicy()` function
5. **Display:** Both console and web UI query `passwordPolicy` and display requirements

### Code Flow
```
build.sh (flags) 
  → build.sh (write to sedunlocksrv.conf)
    → tc-config (export env vars)
      → main.go loadPolicy() (read env vars)
        → passwordPolicySummary() (format for display)
          → Console TUI & Web API (show to user)
```

### Key Functions

**loadPolicy()** - Reads environment variables and creates a PasswordPolicy struct
```go
func loadPolicy() PasswordPolicy {
    // Checks PASSWORD_COMPLEXITY_ON
    // If false, returns policy with all requirements disabled
    // Otherwise applies configured requirements
}
```

**validatePassword()** - Validates user input against the loaded policy
```go
func validatePassword(password string) error {
    // Returns error if password doesn't meet requirements
    // Skips validation if PASSWORD_COMPLEXITY_ON is false
}
```

**passwordPolicySummary()** - Creates human-readable requirement summary
```go
func passwordPolicySummary() string {
    // Returns "no complexity requirements" if disabled
    // Otherwise returns "min X chars, uppercase, lowercase, ..."
}
```

## Configuration Priority

When multiple configuration methods are used, they apply in this order:

1. **build.conf defaults** (lowest priority)
2. **CLI arguments** (override build.conf)
3. **Environment variables** (set at PBA runtime via /etc/sedunlocksrv.conf)

Example:
```bash
# build.conf has MIN_PASSWORD_LENGTH="12"
# CLI passes --min-password-length=16
# Runtime /etc/sedunlocksrv.conf has MIN_PASSWORD_LENGTH="20"

# Final used value: 20 (from runtime env)
```

## Testing

Unit tests verify:
- Password complexity can be disabled entirely
- Custom requirements are applied correctly
- Both "true"/"false" and "on"/"off" syntax work
- Invalid passwords are rejected when complexity is enabled
- Valid passwords are accepted when complexity is disabled

Run tests:
```bash
cd sedunlocksrv
go test -v -run "Password"
```

## Backward Compatibility

- **Default behavior unchanged:** If no flags are set, defaults apply (12 chars, all complexity requirements)
- **Existing builds continue to work:** Builds made before this feature were added are unaffected
- **API compatible:** The `/password-policy` endpoint returns the policy as before

## Troubleshooting

### Problem: Password requirements not appearing in UI
**Solution:** Verify the environment variables are exported in tc-config:
```bash
grep "PASSWORD_COMPLEXITY_ON\|MIN_PASSWORD_LENGTH" /etc/sedunlocksrv.conf
```

### Problem: Build fails with "PASSWORD_COMPLEXITY_ON must be on/off"
**Solution:** Check value is `on`/`off` or `true`/`false` (case-insensitive):
```bash
# Good
--password-complexity=on
--password-complexity=true
--password-complexity=ON

# Bad
--password-complexity=1
--password-complexity=enable
```

### Problem: Minimum length not enforced
**Causes:**
- `PASSWORD_COMPLEXITY_ON=false` disables all requirements
- Check the environment variable is properly exported in tc-config

## Related Files

- `build.sh` - Build script with password complexity flag parsing
- `build.conf.example` - Example configuration file
- `tc/sedunlocksrv.conf` - Configuration file template  
- `tc/tc-config` - Startup script that sources configuration
- `sedunlocksrv/main.go` - Password policy implementation
- `sedunlocksrv/main_test.go` - Unit tests for password policy
- `sedunlocksrv/index.html` - Web UI

## Future Enhancements

Potential improvements not in this release:
- Custom character sets (e.g., require "@" specifically)
- Blacklist/whitelist specific characters
- Password history (prevent reuse)
- Account lockout policies (tied to EXPERT_PASSWORD)
- Per-drive password policies
