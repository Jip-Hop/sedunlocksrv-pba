package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	runtimeNetworkConfigPath = "/etc/sedunlocksrv.conf"
	runtimeNetworkHelperPath = "/usr/local/sbin/sedunlocksrv-net"
)

type RuntimeNetworkConfig struct {
	NetMode            string
	NetIfaces          string
	NetExclude         string
	NetDHCP            bool
	IPAddr             string
	Netmask            string
	Gateway            string
	DNS                string
	BondName           string
	BondMode           string
	BondMiimon         string
	BondLacpRate       string
	BondXmitHashPolicy string
}

var runtimeNetworkKeyOrder = []string{
	"NET_MODE",
	"NET_IFACES",
	"NET_EXCLUDE",
	"NET_DHCP",
	"IP_ADDR",
	"NETMASK",
	"GATEWAY",
	"DNS",
	"BOND_NAME",
	"BOND_MODE",
	"BOND_MIIMON",
	"BOND_LACP_RATE",
	"BOND_XMIT_HASH_POLICY",
}

func defaultRuntimeNetworkConfig() RuntimeNetworkConfig {
	return RuntimeNetworkConfig{
		NetMode:            "single",
		NetDHCP:            true,
		BondName:           "bond0",
		BondMode:           "4",
		BondMiimon:         "100",
		BondLacpRate:       "1",
		BondXmitHashPolicy: "1",
	}
}

func readRuntimeNetworkConfig() (RuntimeNetworkConfig, error) {
	cfg := defaultRuntimeNetworkConfig()
	f, err := os.Open(runtimeNetworkConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseShellAssignment(scanner.Text())
		if !ok {
			continue
		}
		switch key {
		case "NET_MODE":
			cfg.NetMode = value
		case "NET_IFACES":
			cfg.NetIfaces = value
		case "NET_EXCLUDE":
			cfg.NetExclude = value
		case "NET_DHCP":
			cfg.NetDHCP = parseConfigBool(value, true)
		case "IP_ADDR":
			cfg.IPAddr = value
		case "NETMASK":
			cfg.Netmask = value
		case "GATEWAY":
			cfg.Gateway = value
		case "DNS":
			cfg.DNS = value
		case "BOND_NAME":
			cfg.BondName = value
		case "BOND_MODE":
			cfg.BondMode = value
		case "BOND_MIIMON":
			cfg.BondMiimon = value
		case "BOND_LACP_RATE":
			cfg.BondLacpRate = value
		case "BOND_XMIT_HASH_POLICY":
			cfg.BondXmitHashPolicy = value
		}
	}
	if err := scanner.Err(); err != nil {
		return cfg, err
	}
	cfg.normalize()
	return cfg, nil
}

func parseConfigBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseShellAssignment(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	idx := strings.IndexByte(line, '=')
	if idx <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, parseShellValue(line[idx+1:]), true
}

func parseShellValue(value string) string {
	value = trimShellComment(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "'\\''", "'")
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '\'' || quote == '"') && value[len(value)-1] == quote {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func trimShellComment(value string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimSpace(value[:i])
			}
		}
	}
	return strings.TrimSpace(value)
}

func (cfg *RuntimeNetworkConfig) normalize() {
	cfg.NetMode = strings.ToLower(strings.TrimSpace(cfg.NetMode))
	cfg.NetIfaces = normalizeWords(cfg.NetIfaces)
	cfg.NetExclude = normalizeWords(cfg.NetExclude)
	cfg.IPAddr = strings.TrimSpace(cfg.IPAddr)
	cfg.Netmask = strings.TrimSpace(cfg.Netmask)
	cfg.Gateway = strings.TrimSpace(cfg.Gateway)
	cfg.DNS = normalizeWords(strings.ReplaceAll(cfg.DNS, ",", " "))
	cfg.BondName = strings.TrimSpace(cfg.BondName)
	if cfg.BondName == "" {
		cfg.BondName = "bond0"
	}
	cfg.BondMode = strings.TrimSpace(cfg.BondMode)
	cfg.BondMiimon = strings.TrimSpace(cfg.BondMiimon)
	cfg.BondLacpRate = strings.TrimSpace(cfg.BondLacpRate)
	cfg.BondXmitHashPolicy = strings.TrimSpace(cfg.BondXmitHashPolicy)
}

func normalizeWords(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func (cfg RuntimeNetworkConfig) addressingMode() string {
	if cfg.NetDHCP {
		return "dhcp"
	}
	return "static"
}

func (cfg RuntimeNetworkConfig) validate() error {
	cfg.normalize()
	if cfg.NetMode != "single" && cfg.NetMode != "bond" {
		return fmt.Errorf("network mode must be single or bond")
	}
	if err := validateInterfaceList("NET_IFACES", cfg.NetIfaces); err != nil {
		return err
	}
	if err := validateInterfaceList("NET_EXCLUDE", cfg.NetExclude); err != nil {
		return err
	}
	if cfg.NetMode == "bond" {
		if count := len(strings.Fields(cfg.NetIfaces)); count == 1 {
			return fmt.Errorf("bond mode needs at least two NET_IFACES values, or leave NET_IFACES blank for autodetect")
		}
	}
	if cfg.NetDHCP {
		return nil
	}
	ip, err := parseUsableIPv4("IP_ADDR", cfg.IPAddr)
	if err != nil {
		return err
	}
	mask, ones, err := parseIPv4Netmask(cfg.Netmask)
	if err != nil {
		return err
	}
	if ones <= 30 {
		network := ip.Mask(mask)
		broadcast := ipv4Broadcast(network, mask)
		if ip.Equal(network) || ip.Equal(broadcast) {
			return fmt.Errorf("IP_ADDR is the network or broadcast address for NETMASK=%s", cfg.Netmask)
		}
	}
	if cfg.Gateway != "" {
		gateway, err := parseUsableIPv4("GATEWAY", cfg.Gateway)
		if err != nil {
			return err
		}
		if gateway.Equal(ip) {
			return fmt.Errorf("GATEWAY must not be the same address as IP_ADDR")
		}
		if !gateway.Mask(mask).Equal(ip.Mask(mask)) {
			return fmt.Errorf("GATEWAY must be in the same subnet as IP_ADDR/NETMASK")
		}
	}
	for _, dns := range strings.Fields(cfg.DNS) {
		if _, err := parseUsableIPv4("DNS", dns); err != nil {
			return err
		}
	}
	return nil
}

func validateInterfaceList(name, value string) error {
	for _, iface := range strings.Fields(value) {
		if !validInterfaceName(iface) {
			return fmt.Errorf("%s contains invalid interface name %q", name, iface)
		}
	}
	return nil
}

func validInterfaceName(name string) bool {
	if name == "" || len(name) > 15 || name == "lo" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == ':':
		default:
			return false
		}
	}
	return true
}

func parseUsableIPv4(name, value string) (net.IP, error) {
	ip := net.ParseIP(strings.TrimSpace(value)).To4()
	if ip == nil {
		return nil, fmt.Errorf("%s must be a dotted IPv4 address", name)
	}
	if ip.Equal(net.IPv4zero) || ip.Equal(net.IPv4bcast) {
		return nil, fmt.Errorf("%s cannot be 0.0.0.0 or 255.255.255.255", name)
	}
	return ip, nil
}

func parseIPv4Netmask(value string) (net.IPMask, int, error) {
	ip := net.ParseIP(strings.TrimSpace(value)).To4()
	if ip == nil {
		return nil, 0, fmt.Errorf("NETMASK must be a dotted IPv4 netmask")
	}
	mask := net.IPMask(ip)
	ones, bits := mask.Size()
	if bits != 32 || ones == 0 {
		return nil, 0, fmt.Errorf("NETMASK must be a contiguous non-zero IPv4 netmask")
	}
	return mask, ones, nil
}

func ipv4Broadcast(network net.IP, mask net.IPMask) net.IP {
	broadcast := make(net.IP, len(network))
	for i := range network {
		broadcast[i] = network[i] | ^mask[i]
	}
	return broadcast
}

func (cfg RuntimeNetworkConfig) toAssignments() map[string]string {
	return map[string]string{
		"NET_MODE":              cfg.NetMode,
		"NET_IFACES":            cfg.NetIfaces,
		"NET_EXCLUDE":           cfg.NetExclude,
		"NET_DHCP":              fmt.Sprintf("%t", cfg.NetDHCP),
		"IP_ADDR":               cfg.IPAddr,
		"NETMASK":               cfg.Netmask,
		"GATEWAY":               cfg.Gateway,
		"DNS":                   cfg.DNS,
		"BOND_NAME":             cfg.BondName,
		"BOND_MODE":             cfg.BondMode,
		"BOND_MIIMON":           cfg.BondMiimon,
		"BOND_LACP_RATE":        cfg.BondLacpRate,
		"BOND_XMIT_HASH_POLICY": cfg.BondXmitHashPolicy,
	}
}

func writeRuntimeNetworkConfig(cfg RuntimeNetworkConfig) error {
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return err
	}

	assignments := cfg.toAssignments()
	networkKeys := map[string]struct{}{}
	for _, key := range runtimeNetworkKeyOrder {
		networkKeys[key] = struct{}{}
	}

	var output bytes.Buffer
	written := map[string]struct{}{}
	data, err := os.ReadFile(runtimeNetworkConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			line := scanner.Text()
			key, _, ok := parseShellAssignment(line)
			if ok {
				if _, isNetworkKey := networkKeys[key]; isNetworkKey {
					fmt.Fprintf(&output, "%s=%s\n", key, shellQuote(assignments[key]))
					written[key] = struct{}{}
					continue
				}
			}
			output.WriteString(line)
			output.WriteByte('\n')
		}
		if err := scanner.Err(); err != nil {
			return err
		}
	}
	for _, key := range runtimeNetworkKeyOrder {
		if _, ok := written[key]; ok {
			continue
		}
		fmt.Fprintf(&output, "%s=%s\n", key, shellQuote(assignments[key]))
	}

	dir := filepath.Dir(runtimeNetworkConfigPath)
	tmp, err := os.CreateTemp(dir, ".sedunlocksrv.conf.")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(output.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, runtimeNetworkConfigPath)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func applyRuntimeNetworkConfig() (string, error) {
	if _, err := os.Stat(runtimeNetworkHelperPath); err != nil {
		return "", fmt.Errorf("network helper unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, runtimeNetworkHelperPath, "--configure")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return output.String(), fmt.Errorf("network apply timed out")
	}
	if err != nil {
		return output.String(), err
	}
	return output.String(), nil
}

func runConsoleNetworkMenu() {
	clearConsoleScreen()
	fmt.Println(colorBlue + "NETWORK CONFIGURATION" + colorReset)

	cfg, err := readRuntimeNetworkConfig()
	if err != nil {
		fmt.Println("Could not read current network config:", err)
		waitForConsoleEnter()
		return
	}
	printRuntimeNetworkConfig(cfg)
	printAvailableNetworkInterfaces()

	fmt.Println("\nPress Enter to keep the current value. Type 'auto' for automatic interfaces, or 'none' to clear optional static fields.")
	if v := readConsoleLine(fmt.Sprintf("Network mode [single/bond] (%s): ", cfg.NetMode)); v != "" {
		cfg.NetMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v := readConsoleLine(fmt.Sprintf("Managed interfaces (%s): ", displayConfigValue(cfg.NetIfaces, "auto"))); v != "" {
		if strings.EqualFold(strings.TrimSpace(v), "auto") {
			cfg.NetIfaces = ""
		} else {
			cfg.NetIfaces = v
		}
	}
	if v := readConsoleLine(fmt.Sprintf("Addressing [dhcp/static] (%s): ", cfg.addressingMode())); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "dhcp":
			cfg.NetDHCP = true
		case "static":
			cfg.NetDHCP = false
		default:
			fmt.Println("\nAddressing must be dhcp or static.")
			waitForConsoleEnter()
			return
		}
	}

	if cfg.NetDHCP {
		cfg.IPAddr = ""
		cfg.Netmask = ""
		cfg.Gateway = ""
		cfg.DNS = ""
	} else {
		if v := readConsoleLine(fmt.Sprintf("Static IP address (%s): ", displayConfigValue(cfg.IPAddr, "required"))); v != "" {
			cfg.IPAddr = v
		}
		if v := readConsoleLine(fmt.Sprintf("Netmask (%s): ", displayConfigValue(cfg.Netmask, "required"))); v != "" {
			cfg.Netmask = v
		}
		if v := readConsoleLine(fmt.Sprintf("Gateway (%s): ", displayConfigValue(cfg.Gateway, "none"))); v != "" {
			cfg.Gateway = optionalConfigValue(v)
		}
		if v := readConsoleLine(fmt.Sprintf("DNS servers (%s): ", displayConfigValue(cfg.DNS, "none"))); v != "" {
			cfg.DNS = optionalConfigValue(v)
		}
	}

	cfg.normalize()
	if err := cfg.validate(); err != nil {
		fmt.Println("\nInvalid network configuration:", err)
		waitForConsoleEnter()
		return
	}

	fmt.Println("\nNew network configuration:")
	printRuntimeNetworkConfig(cfg)
	confirm := strings.ToLower(readConsoleLine("\nApply now? [y/N]: "))
	if confirm != "y" && confirm != "yes" {
		fmt.Println("Network changes were not applied.")
		waitForConsoleEnter()
		return
	}

	if err := writeRuntimeNetworkConfig(cfg); err != nil {
		fmt.Println("Failed to save network config:", err)
		waitForConsoleEnter()
		return
	}

	output, err := applyRuntimeNetworkConfig()
	if strings.TrimSpace(output) != "" {
		fmt.Println("\nNetwork helper output:")
		fmt.Print(output)
	}
	if err != nil {
		fmt.Println("\nNetwork apply failed:", err)
	} else {
		fmt.Println("\nNetwork configuration applied.")
	}
	waitForConsoleEnter()
}

func displayConfigValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func optionalConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "none") {
		return ""
	}
	return value
}

func printRuntimeNetworkConfig(cfg RuntimeNetworkConfig) {
	fmt.Printf("  Mode: %s\n", cfg.NetMode)
	fmt.Printf("  Interfaces: %s\n", displayConfigValue(cfg.NetIfaces, "auto"))
	fmt.Printf("  Addressing: %s\n", cfg.addressingMode())
	if !cfg.NetDHCP {
		fmt.Printf("  IP address: %s\n", displayConfigValue(cfg.IPAddr, "required"))
		fmt.Printf("  Netmask: %s\n", displayConfigValue(cfg.Netmask, "required"))
		fmt.Printf("  Gateway: %s\n", displayConfigValue(cfg.Gateway, "none"))
		fmt.Printf("  DNS: %s\n", displayConfigValue(cfg.DNS, "none"))
	}
}

func printAvailableNetworkInterfaces() {
	statuses := scanNetworkInterfaces()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	fmt.Println("\nDetected interfaces:")
	found := false
	for _, iface := range statuses {
		if iface.Loopback {
			continue
		}
		found = true
		link := "no-link"
		if iface.Carrier {
			link = "link"
		}
		fmt.Printf("  %s  %s  %s", iface.Name, iface.State, link)
		if iface.MAC != "" {
			fmt.Printf("  %s", iface.MAC)
		}
		if len(iface.Addresses) > 0 {
			fmt.Printf("  %s", strings.Join(iface.Addresses, ", "))
		}
		fmt.Println()
	}
	if !found {
		fmt.Println("  No non-loopback interfaces reported.")
	}
}
