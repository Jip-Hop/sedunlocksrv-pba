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

func TestMatchBootEntryCmdlineSplitEfiAndRoot(t *testing.T) {
	efiMount := t.TempDir()
	rootMount := t.TempDir()

	grubCfg := filepath.Join(efiMount, "EFI", "proxmox", "grub.cfg")
	kernel := filepath.Join(rootMount, "boot", "vmlinuz-6.17.4-2-pve")
	initrd := filepath.Join(rootMount, "boot", "initrd.img-6.17.4-2-pve")

	for _, path := range []string{grubCfg, kernel, initrd} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
	}

	cfg := "menuentry 'Proxmox VE GNU/Linux' {\n" +
		"    linux /boot/vmlinuz-6.17.4-2-pve root=/dev/mapper/pve-root ro quiet\n" +
		"    initrd /boot/initrd.img-6.17.4-2-pve\n" +
		"}\n"
	if err := os.WriteFile(grubCfg, []byte(cfg), 0o644); err != nil {
		t.Fatalf("WriteFile(grubCfg): %v", err)
	}
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("WriteFile(kernel): %v", err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o644); err != nil {
		t.Fatalf("WriteFile(initrd): %v", err)
	}

	entries := collectBootCatalog(efiMount)
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	cmdline, _, ok := matchBootEntryCmdline(entries, kernel, initrd)
	if !ok {
		t.Fatal("matchBootEntryCmdline() did not find cmdline for split EFI/root layout")
	}
	want := "root=/dev/mapper/pve-root ro quiet"
	if cmdline != want {
		t.Fatalf("cmdline = %q, want %q", cmdline, want)
	}
}

func TestMatchBootEntryCmdlineRequiresMatchingKernel(t *testing.T) {
	entries := []BootEntry{
		makeBootEntry(
			"/boot/vmlinuz-6.17.3-1-pve",
			[]string{"/boot/initrd.img-6.17.3-1-pve"},
			"root=/dev/mapper/pve-root ro",
			"test",
		),
	}

	if _, _, ok := matchBootEntryCmdline(entries, "/boot/vmlinuz-6.17.4-2-pve", "/boot/initrd.img-6.17.4-2-pve"); ok {
		t.Fatal("matchBootEntryCmdline() matched a cmdline for the wrong kernel version")
	}
}

func TestParseDefaultGrubCmdline(t *testing.T) {
	mountPoint := t.TempDir()
	grubDefaults := filepath.Join(mountPoint, "etc", "default", "grub")
	if err := os.MkdirAll(filepath.Dir(grubDefaults), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", grubDefaults, err)
	}

	content := "" +
		"GRUB_DEFAULT=0\n" +
		"GRUB_CMDLINE_LINUX=\"root=/dev/mapper/pve-root ro\"\n" +
		"GRUB_CMDLINE_LINUX_DEFAULT='quiet iommu=pt'\n"
	if err := os.WriteFile(grubDefaults, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", grubDefaults, err)
	}

	got, ok, err := parseDefaultGrubCmdline(mountPoint)
	if err != nil {
		t.Fatalf("parseDefaultGrubCmdline() error: %v", err)
	}
	if !ok {
		t.Fatal("parseDefaultGrubCmdline() did not find a cmdline")
	}
	want := "root=/dev/mapper/pve-root ro quiet iommu=pt"
	if got != want {
		t.Fatalf("cmdline = %q, want %q", got, want)
	}
}

func TestParseKernelCmdlineFile(t *testing.T) {
	mountPoint := t.TempDir()
	cmdlinePath := filepath.Join(mountPoint, "etc", "kernel", "cmdline")
	if err := os.MkdirAll(filepath.Dir(cmdlinePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cmdlinePath, err)
	}

	want := "root=/dev/mapper/pve-root boot=zfs quiet"
	if err := os.WriteFile(cmdlinePath, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", cmdlinePath, err)
	}

	got, ok, err := parseKernelCmdlineFile(mountPoint)
	if err != nil {
		t.Fatalf("parseKernelCmdlineFile() error: %v", err)
	}
	if !ok {
		t.Fatal("parseKernelCmdlineFile() did not find a cmdline")
	}
	if got != want {
		t.Fatalf("cmdline = %q, want %q", got, want)
	}
}

func TestSplitBootPrefersCatalogOverWeakFallback(t *testing.T) {
	efiMount := t.TempDir()
	rootMount := t.TempDir()

	grubCfg := filepath.Join(efiMount, "EFI", "proxmox", "grub.cfg")
	kernel := filepath.Join(rootMount, "boot", "vmlinuz-6.17.4-2-pve")
	initrd := filepath.Join(rootMount, "boot", "initrd.img-6.17.4-2-pve")
	grubDefaults := filepath.Join(rootMount, "etc", "default", "grub")

	for _, path := range []string{grubCfg, kernel, initrd, grubDefaults} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
	}

	cfg := "menuentry 'Proxmox VE GNU/Linux' {\n" +
		"    linux /boot/vmlinuz-6.17.4-2-pve root=/dev/mapper/pve-root ro quiet iommu=pt\n" +
		"    initrd /boot/initrd.img-6.17.4-2-pve\n" +
		"}\n"
	if err := os.WriteFile(grubCfg, []byte(cfg), 0o644); err != nil {
		t.Fatalf("WriteFile(grubCfg): %v", err)
	}
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("WriteFile(kernel): %v", err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o644); err != nil {
		t.Fatalf("WriteFile(initrd): %v", err)
	}
	if err := os.WriteFile(grubDefaults, []byte("GRUB_CMDLINE_LINUX_DEFAULT='quiet'\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(grubDefaults): %v", err)
	}

	gotKernel, gotInitrd, gotCmdline, ok := findBootArtifacts(rootMount)
	if !ok {
		t.Fatal("findBootArtifacts() did not find root boot artifacts")
	}
	if gotKernel != kernel || gotInitrd != initrd {
		t.Fatalf("artifacts = (%q, %q), want (%q, %q)", gotKernel, gotInitrd, kernel, initrd)
	}
	if gotCmdline != "quiet" {
		t.Fatalf("weak fallback cmdline = %q, want %q", gotCmdline, "quiet")
	}

	entries := collectBootCatalog(efiMount)
	matchedCmdline, _, ok := matchBootEntryCmdline(entries, gotKernel, gotInitrd)
	if !ok {
		t.Fatal("matchBootEntryCmdline() did not find a matching EFI catalog entry")
	}
	want := "root=/dev/mapper/pve-root ro quiet iommu=pt"
	if matchedCmdline != want {
		t.Fatalf("matched cmdline = %q, want %q", matchedCmdline, want)
	}
}

func TestLooksWeakCmdline(t *testing.T) {
	if !looksWeakCmdline("quiet") {
		t.Fatal("looksWeakCmdline(\"quiet\") = false, want true")
	}
	if !looksWeakCmdline("quiet splash iommu=pt") {
		t.Fatal("looksWeakCmdline() treated cosmetic-only cmdline as strong")
	}
	if looksWeakCmdline("root=/dev/mapper/pve-root ro quiet") {
		t.Fatal("looksWeakCmdline() treated root-based cmdline as weak")
	}
	if looksWeakCmdline("root=ZFS=rpool/ROOT/pve-1 boot=zfs quiet") {
		t.Fatal("looksWeakCmdline() treated ZFS cmdline as weak")
	}
}

func TestSynthesizeRootCmdline(t *testing.T) {
	got, ok := synthesizeRootCmdline("/dev/mapper/pve-root", "quiet")
	if !ok {
		t.Fatal("synthesizeRootCmdline() returned ok=false")
	}
	want := "root=/dev/mapper/pve-root ro quiet"
	if got != want {
		t.Fatalf("cmdline = %q, want %q", got, want)
	}
}
