//go:build linux
// +build linux

package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigureHostVethSysctlsSetsLooseRPFilter(t *testing.T) {
	oldProcSysDir := procSysIPv4ConfDir
	procSysIPv4ConfDir = t.TempDir()
	t.Cleanup(func() {
		procSysIPv4ConfDir = oldProcSysDir
	})

	iface := "veth-test-h"
	ifaceDir := filepath.Join(procSysIPv4ConfDir, iface)
	if err := os.MkdirAll(ifaceDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", ifaceDir, err)
	}
	rpFilterPath := filepath.Join(ifaceDir, "rp_filter")
	if err := os.WriteFile(rpFilterPath, []byte("1"), 0644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", rpFilterPath, err)
	}

	if err := configureHostVethSysctls(iface); err != nil {
		t.Fatalf("configureHostVethSysctls(%q) error = %v", iface, err)
	}

	got, err := os.ReadFile(rpFilterPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", rpFilterPath, err)
	}
	if string(got) != "2" {
		t.Fatalf("rp_filter = %q, want %q", string(got), "2")
	}
}

func TestConfigureHostVethSysctlsMissingInterface(t *testing.T) {
	oldProcSysDir := procSysIPv4ConfDir
	procSysIPv4ConfDir = t.TempDir()
	t.Cleanup(func() {
		procSysIPv4ConfDir = oldProcSysDir
	})

	if err := configureHostVethSysctls("veth-missing-h"); err == nil {
		t.Fatal("configureHostVethSysctls() error = nil, want non-nil")
	}
}
