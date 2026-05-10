package main

import "testing"

func TestRuntimeNetworkConfigValidateStaticIPv4(t *testing.T) {
	cfg := defaultRuntimeNetworkConfig()
	cfg.NetDHCP = false
	cfg.IPAddr = "192.168.10.20"
	cfg.Netmask = "255.255.255.0"
	cfg.Gateway = "192.168.10.1"
	cfg.DNS = "1.1.1.1 8.8.8.8"

	if err := cfg.validate(); err != nil {
		t.Fatalf("valid static config rejected: %v", err)
	}
}

func TestRuntimeNetworkConfigRejectsInvalidStaticIPv4(t *testing.T) {
	cases := []struct {
		name string
		cfg  RuntimeNetworkConfig
	}{
		{
			name: "bad ip octet",
			cfg: RuntimeNetworkConfig{
				NetMode: "single",
				NetDHCP: false,
				IPAddr:  "192.168.10.999",
				Netmask: "255.255.255.0",
			},
		},
		{
			name: "non-contiguous netmask",
			cfg: RuntimeNetworkConfig{
				NetMode: "single",
				NetDHCP: false,
				IPAddr:  "192.168.10.20",
				Netmask: "255.0.255.0",
			},
		},
		{
			name: "gateway outside subnet",
			cfg: RuntimeNetworkConfig{
				NetMode: "single",
				NetDHCP: false,
				IPAddr:  "192.168.10.20",
				Netmask: "255.255.255.0",
				Gateway: "192.168.11.1",
			},
		},
		{
			name: "network address",
			cfg: RuntimeNetworkConfig{
				NetMode: "single",
				NetDHCP: false,
				IPAddr:  "192.168.10.0",
				Netmask: "255.255.255.0",
			},
		},
		{
			name: "unsafe interface name",
			cfg: RuntimeNetworkConfig{
				NetMode:   "single",
				NetDHCP:   true,
				NetIfaces: "eth0;reboot",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.validate(); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestParseShellAssignmentHandlesQuotedValues(t *testing.T) {
	key, value, ok := parseShellAssignment("TLS_SERVER_NAME='pba.example.com' # kept for SSH")
	if !ok {
		t.Fatal("assignment was not parsed")
	}
	if key != "TLS_SERVER_NAME" {
		t.Fatalf("key = %q, want TLS_SERVER_NAME", key)
	}
	if value != "pba.example.com" {
		t.Fatalf("value = %q, want pba.example.com", value)
	}
}
