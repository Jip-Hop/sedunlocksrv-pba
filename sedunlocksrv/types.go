// types.go — Data structures for SED Unlock Server
package main

import "time"

// ============================================================
// HTTP API DATA TYPES
// ============================================================

type DriveStatus struct {
	Device string `json:"device"`
	Locked bool   `json:"locked"`
	Opal   bool   `json:"opal"`
}

type NetworkInterfaceStatus struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	State     string   `json:"state"`
	Carrier   bool     `json:"carrier"`
	Loopback  bool     `json:"loopback"`
	Addresses []string `json:"addresses,omitempty"`
}

type StatusResponse struct {
	Drives            []DriveStatus            `json:"drives"`
	Interfaces        []NetworkInterfaceStatus `json:"interfaces"`
	BootReady         bool                     `json:"bootReady"`
	BootDrives        []string                 `json:"bootDrives,omitempty"`
	FailedAttempts    int                      `json:"failedAttempts"`
	MaxAttempts       int                      `json:"maxAttempts"`
	AttemptsRemaining int                      `json:"attemptsRemaining"`
	Build             string                   `json:"build,omitempty"`
}

type DriveDiagnostics struct {
	Device              string `json:"device"`
	Opal                bool   `json:"opal"`
	Locked              bool   `json:"locked"`
	LockingSupported    string `json:"lockingSupported"`
	LockingEnabled      string `json:"lockingEnabled"`
	MBREnabled          string `json:"mbrEnabled"`
	MBRDone             string `json:"mbrDone"`
	MediaEncrypt        string `json:"mediaEncrypt"`
	LockingRange0Locked string `json:"lockingRange0Locked"`
	QueryRaw            string `json:"queryRaw"`
}

type DiagnosticsResponse struct {
	GeneratedAt string             `json:"generatedAt"`
	Drives      []DriveDiagnostics `json:"drives"`
}

type UnlockResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
}

type UnlockResponse struct {
	Results []UnlockResult `json:"results"`
	Token   string         `json:"token,omitempty"`
}

type PasswordChangeResult struct {
	Device  string `json:"device"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type PasswordChangeResponse struct {
	Results []PasswordChangeResult `json:"results"`
}

type BootResult struct {
	Kernel        string        `json:"kernel"`
	Initrd        string        `json:"initrd"`
	Cmdline       string        `json:"cmdline"`
	Drives        []DriveStatus `json:"drives"`
	Warning       string        `json:"warning,omitempty"`
	FullyUnlocked bool          `json:"fullyUnlocked"`
	Debug         []string      `json:"debug,omitempty"`
}

type BootLaunchStatus struct {
	InProgress    bool             `json:"inProgress"`
	Accepted      bool             `json:"accepted"`
	Error         string           `json:"error,omitempty"`
	Debug         []string         `json:"debug,omitempty"`
	Result        *BootResult      `json:"result,omitempty"`
	StartedAt     time.Time        `json:"-"`
	DiscoveryDone bool             `json:"discoveryDone,omitempty"`
	Kernels       []BootKernelInfo `json:"kernels,omitempty"`
}

type BootAttemptError struct {
	Message string
	Debug   []string
}

func (e BootAttemptError) Error() string {
	return e.Message
}

type FlashStatus struct {
	InProgress bool     `json:"inProgress"`
	Lines      []string `json:"lines"`
	Error      string   `json:"error,omitempty"`
	Done       bool     `json:"done"`
	Success    bool     `json:"success"`
}

type BootEntry struct {
	KernelRef    string
	KernelBase   string
	KernelSuffix string
	InitrdRefs   []string
	InitrdBases  []string
	InitrdSuffix []string
	Cmdline      string
	Source       string
}

// BootKernelInfo describes a discovered kernel available for boot selection
type BootKernelInfo struct {
	Index      int    `json:"index"`
	Device     string `json:"-"`
	Kernel     string `json:"kernel"`
	KernelName string `json:"kernelName"` // e.g., "vmlinuz-6.8.12-9-pve"
	Initrd     string `json:"initrd"`
	InitrdName string `json:"initrdName"` // e.g., "initrd.img-6.8.12-9-pve"
	Cmdline    string `json:"cmdline"`
	Source     string `json:"source"`   // e.g., "GRUB" or "loader.conf"
	Recovery   bool   `json:"recovery"` // true if cmdline indicates recovery mode
}

// PasswordPolicy describes complexity requirements for setting a new password.
// It is NOT applied to unlock attempts — the drive may have been initialized
// with a password that doesn't meet these requirements.
type PasswordPolicy struct {
	MinLength      int  `json:"minLength"`
	RequireUpper   bool `json:"requireUpper"`
	RequireLower   bool `json:"requireLower"`
	RequireNumber  bool `json:"requireNumber"`
	RequireSpecial bool `json:"requireSpecial"`
}
