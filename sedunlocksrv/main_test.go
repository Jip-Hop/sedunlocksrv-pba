package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindBootArtifactsRecursiveEFIGrubLayout(t *testing.T) {
	mountPoint := t.TempDir()
	kernel := filepath.Join(mountPoint, "EFI", "proxmox", "6.8.12-9-pve", "vmlinuz-6.8.12-9-pve")
	initrd := filepath.Join(mountPoint, "EFI", "proxmox", "6.8.12-9-pve", "initrd.img-6.8.12-9-pve")
	grubCfg := filepath.Join(mountPoint, "EFI", "proxmox", "grub.cfg")

	for _, path := range []string{kernel, initrd, grubCfg} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
	}

	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("WriteFile(kernel): %v", err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o644); err != nil {
		t.Fatalf("WriteFile(initrd): %v", err)
	}
	cfg := "menuentry 'Proxmox VE GNU/Linux' {\n" +
		"    linux /EFI/proxmox/6.8.12-9-pve/vmlinuz-6.8.12-9-pve root=ZFS=rpool/ROOT/pve-1 boot=zfs\n" +
		"    initrd /EFI/proxmox/6.8.12-9-pve/initrd.img-6.8.12-9-pve\n" +
		"}\n"
	if err := os.WriteFile(grubCfg, []byte(cfg), 0o644); err != nil {
		t.Fatalf("WriteFile(grubCfg): %v", err)
	}

	gotKernel, gotInitrd, gotCmdline, ok := findBootArtifacts(mountPoint)
	if !ok {
		t.Fatal("findBootArtifacts() did not find EFI boot artifacts")
	}
	if gotKernel != kernel {
		t.Fatalf("kernel = %q, want %q", gotKernel, kernel)
	}
	if gotInitrd != initrd {
		t.Fatalf("initrd = %q, want %q", gotInitrd, initrd)
	}
	wantCmdline := "root=ZFS=rpool/ROOT/pve-1 boot=zfs"
	if gotCmdline != wantCmdline {
		t.Fatalf("cmdline = %q, want %q", gotCmdline, wantCmdline)
	}
}

func TestFindBootArtifactsRecursiveKernelFallback(t *testing.T) {
	mountPoint := t.TempDir()
	kernel := filepath.Join(mountPoint, "EFI", "proxmox", "6.8.12-9-pve", "vmlinuz-6.8.12-9-pve")
	initrd := filepath.Join(mountPoint, "EFI", "proxmox", "6.8.12-9-pve", "initrd.img-6.8.12-9-pve")

	for _, path := range []string{kernel, initrd} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	gotKernel, gotInitrd, gotCmdline, ok := findBootArtifacts(mountPoint)
	if !ok {
		t.Fatal("findBootArtifacts() did not match recursive kernel/initrd pair")
	}
	if gotKernel != kernel {
		t.Fatalf("kernel = %q, want %q", gotKernel, kernel)
	}
	if gotInitrd != initrd {
		t.Fatalf("initrd = %q, want %q", gotInitrd, initrd)
	}
	if gotCmdline != "" {
		t.Fatalf("cmdline = %q, want empty string", gotCmdline)
	}
}
