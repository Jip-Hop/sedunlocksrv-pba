// ssh.go — Dropbear SSH service integration
package main

import (
	"log"
	"os"
	"os/exec"
)

// startSSHService initializes and starts the Dropbear SSH server if available
func startSSHService() {
	dropbearBin := firstExistingPath("/usr/local/sbin/dropbear", "/usr/sbin/dropbear", "/usr/local/bin/dropbear")
	if dropbearBin == "" {
		if multi := firstExistingPath("/usr/local/bin/dropbearmulti", "/usr/bin/dropbearmulti"); multi != "" {
			symlinkPath := "/tmp/dropbear"
			_ = os.Remove(symlinkPath)
			if err := os.Symlink(multi, symlinkPath); err == nil {
				dropbearBin = symlinkPath
			} else {
				log.Printf("[ssh] failed to prepare dropbearmulti symlink: %v", err)
			}
		}
	}
	if dropbearBin == "" {
		log.Println("[ssh] dropbear not present; SSH UI disabled")
		return
	}

	ecdsaKey := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_ecdsa_host_key",
		"/etc/dropbear/dropbear_ecdsa_host_key",
	)
	ed25519Key := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_ed25519_host_key",
		"/etc/dropbear/dropbear_ed25519_host_key",
	)
	rsaKey := firstExistingPath(
		"/usr/local/etc/dropbear/dropbear_rsa_host_key",
		"/etc/dropbear/dropbear_rsa_host_key",
	)
	if ed25519Key == "" && ecdsaKey == "" && rsaKey == "" {
		log.Println("[ssh] dropbear keys not present; SSH UI disabled")
		return
	}

	args := []string{
		"-R",
		"-E",
		"-F",
		"-p", "2222",
	}
	if ed25519Key != "" {
		args = append(args, "-r", ed25519Key)
	}
	if ecdsaKey != "" {
		args = append(args, "-r", ecdsaKey)
	}
	if rsaKey != "" {
		args = append(args, "-r", rsaKey)
	}
	if banner := firstExistingPath("/usr/local/etc/dropbear/banner", "/etc/dropbear/banner"); banner != "" {
		args = append(args, "-b", banner)
	}

	cmd := exec.Command(dropbearBin, args...)
	if err := cmd.Start(); err != nil {
		log.Printf("[ssh] failed to start dropbear: %v", err)
		return
	}
	log.Printf("[ssh] dropbear started on port 2222 (pid %d)", cmd.Process.Pid)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("[ssh] dropbear exited: %v", err)
		}
	}()
}
