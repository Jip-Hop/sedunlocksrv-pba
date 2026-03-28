// network.go — Network interface discovery
package main

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scanNetworkInterfaces discovers all network interfaces and their addresses
func scanNetworkInterfaces() []NetworkInterfaceStatus {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	statuses := make([]NetworkInterfaceStatus, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses := []string{}
		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				addresses = append(addresses, addr.String())
			}
		}
		sort.Strings(addresses)

		state := "unknown"
		if b, err := os.ReadFile(filepath.Join("/sys/class/net", iface.Name, "operstate")); err == nil {
			state = strings.TrimSpace(string(b))
		}
		carrier := false
		if b, err := os.ReadFile(filepath.Join("/sys/class/net", iface.Name, "carrier")); err == nil {
			carrier = strings.TrimSpace(string(b)) == "1"
		}
		statuses = append(statuses, NetworkInterfaceStatus{
			Name:      iface.Name,
			MAC:       iface.HardwareAddr.String(),
			State:     state,
			Carrier:   carrier,
			Loopback:  (iface.Flags & net.FlagLoopback) != 0,
			Addresses: addresses,
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Loopback != statuses[j].Loopback {
			return !statuses[i].Loopback
		}
		return statuses[i].Name < statuses[j].Name
	})
	return statuses
}
