// drive.go — Drive detection and operations
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// scanDrives detects all OPAL-capable drives and their lock status
func scanDrives() []DriveStatus {
	var statuses []DriveStatus

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "sedutil-cli", "--scan").Output()
	if err != nil {
		return statuses
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}

		opal := strings.HasPrefix(fields[1], "2")
		locked := false
		if opal {
			query, _ := queryDrive(dev)
			locked = strings.Contains(query, "Locked = Y")
		}
		statuses = append(statuses, DriveStatus{Device: dev, Locked: locked, Opal: opal})
	}
	return statuses
}

// collectDriveDiagnostics gathers detailed OPAL diagnostics for all drives
func collectDriveDiagnostics() []DriveDiagnostics {
	drives := scanDrives()
	diag := make([]DriveDiagnostics, 0, len(drives))
	for _, d := range drives {
		raw, _ := queryDrive(d.Device)
		diag = append(diag, DriveDiagnostics{
			Device:              d.Device,
			Opal:                d.Opal,
			Locked:              d.Locked,
			LockingSupported:    queryField(raw, "LockingSupported"),
			LockingEnabled:      queryField(raw, "LockingEnabled"),
			MBREnabled:          queryField(raw, "MBREnable"),
			MBRDone:             queryField(raw, "MBRDone"),
			MediaEncrypt:        queryField(raw, "MediaEncrypt"),
			LockingRange0Locked: queryField(raw, "Locked"),
			QueryRaw:            strings.TrimSpace(raw),
		})
	}
	return diag
}

// listDevicePartitions returns all partition device paths for the given device.
//
// sedutil-cli reports NVMe drives as /dev/nvme0 (the controller), but the
// actual block device is the namespace, e.g. /dev/nvme0n1, and partitions are
// nvme0n1p1, nvme0n1p2, etc.
//
// We scan /sys/class/block/ and use the pkname file to identify which block
// device is the parent of each partition. For NVMe, we include the controller
// name ("nvme0") AND any namespace names ("nvme0n1", "nvme0n2", etc.).
// For SATA/SAS (sda, sdb, etc.), pkname simply equals the base name.
func listDevicePartitions(device string) ([]string, error) {
	base := filepath.Base(device) // e.g. "nvme0" or "sda"

	// Helper to check if a block device is a partition (has "partition" file).
	isPartition := func(name string) bool {
		_, err := os.Stat(filepath.Join("/sys/class/block", name, "partition"))
		return err == nil
	}

	// Helper to check if a block device exists and is not a partition.
	isBlockDevice := func(name string) bool {
		_, err := os.Stat(filepath.Join("/sys/class/block", name, "dev"))
		return err == nil && !isPartition(name)
	}

	// Find all owner devices: the base device plus any NVMe namespaces.
	// For SATA (sda): owners = {sda}
	// For NVMe (nvme0): owners = {nvme0, nvme0n1, nvme0n2, ...}
	owners := []string{base}
	allEntries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}

	for _, entry := range allEntries {
		name := entry.Name()
		// Check only candidates matching the namespace pattern (base+"n" + digit).
		if strings.HasPrefix(name, base+"n") && isBlockDevice(name) {
			owners = append(owners, name)
		}
	}
	sort.Strings(owners) // For deterministic fallback matching

	// Collect partitions whose pkname matches one of our owners.
	var partitions []string
	for _, entry := range allEntries {
		name := entry.Name()
		if !isPartition(name) {
			continue
		}

		// Read parent device name from pkname file.
		pkRaw, err := os.ReadFile(filepath.Join("/sys/class/block", name, "pkname"))
		var parent string
		if err == nil {
			parent = strings.TrimSpace(string(pkRaw))
		}

		// If pkname fails, fall back to prefix matching (rare, but handles edge cases).
		// Use a sorted owner list to ensure deterministic behavior.
		if parent == "" {
			for _, owner := range owners {
				if strings.HasPrefix(name, owner) {
					parent = owner
					break
				}
			}
		}

		// Add partition if its parent is one of our owners.
		if parent != "" {
			for _, owner := range owners {
				if parent == owner {
					partitions = append(partitions, "/dev/"+name)
					break
				}
			}
		}
	}

	sort.Strings(partitions)
	return partitions, nil
}

// listDeviceNodes returns direct device nodes (without partitions)
func listDeviceNodes(device string) ([]string, error) {
	base := filepath.Base(device)
	nodes := []string{"/dev/" + base}

	allEntries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}

	// Find NVMe namespaces: entries starting with base+"n" that are block
	// devices but not partitions.
	for _, entry := range allEntries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+"n") {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "dev")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err == nil {
			continue
		}
		nodes = append(nodes, "/dev/"+name)
	}

	sort.Strings(nodes)
	return nodes, nil
}

// rescanBlockDeviceLayout signals the kernel to re-read partition tables
func rescanBlockDeviceLayout(device string) {
	nodes, err := listDeviceNodes(device)
	if err != nil {
		return
	}

	for _, node := range nodes {
		if f, err := os.OpenFile(node, os.O_RDONLY, 0); err == nil {
			_ = unix.IoctlSetInt(int(f.Fd()), unix.BLKRRPART, 0)
			f.Close()
		}
		if haveRuntimeCommand("blockdev") {
			_ = exec.Command("blockdev", "--rereadpt", node).Run()
		}
		if haveRuntimeCommand("partprobe") {
			_ = exec.Command("partprobe", node).Run()
		}
	}
}
