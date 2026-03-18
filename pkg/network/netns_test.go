//go:build linux
// +build linux

package network

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/vishvananda/netlink"
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

func TestResolveExternalIfaceAutoUsesDefaultRoute(t *testing.T) {
	oldInterfaceByName := netInterfaceByName
	oldDefaultRouteIfaceFn := defaultRouteIfaceFn
	t.Cleanup(func() {
		netInterfaceByName = oldInterfaceByName
		defaultRouteIfaceFn = oldDefaultRouteIfaceFn
	})

	netInterfaceByName = func(name string) (*net.Interface, error) {
		if name != "ens4" {
			return nil, fmt.Errorf("unexpected interface lookup: %s", name)
		}
		return &net.Interface{Name: "ens4"}, nil
	}
	defaultRouteIfaceFn = func() string { return "ens4" }

	got, err := resolveExternalIface("auto", nil)
	if err != nil {
		t.Fatalf("resolveExternalIface(auto) error = %v", err)
	}
	if got != "ens4" {
		t.Fatalf("resolveExternalIface(auto) = %q, want %q", got, "ens4")
	}
}

func TestResolveExternalIfaceConfiguredMismatchReturnsError(t *testing.T) {
	oldInterfaceByName := netInterfaceByName
	oldDefaultRouteIfaceFn := defaultRouteIfaceFn
	t.Cleanup(func() {
		netInterfaceByName = oldInterfaceByName
		defaultRouteIfaceFn = oldDefaultRouteIfaceFn
	})

	netInterfaceByName = func(name string) (*net.Interface, error) {
		return &net.Interface{Name: name}, nil
	}
	defaultRouteIfaceFn = func() string { return "ens4" }

	if _, err := resolveExternalIface("eth0", nil); err == nil {
		t.Fatal("resolveExternalIface(eth0) error = nil, want mismatch error")
	}
}

func TestResolveExternalIfaceConfiguredUsesMatchingDefaultRoute(t *testing.T) {
	oldInterfaceByName := netInterfaceByName
	oldDefaultRouteIfaceFn := defaultRouteIfaceFn
	t.Cleanup(func() {
		netInterfaceByName = oldInterfaceByName
		defaultRouteIfaceFn = oldDefaultRouteIfaceFn
	})

	netInterfaceByName = func(name string) (*net.Interface, error) {
		if name != "ens4" {
			return nil, fmt.Errorf("unexpected interface lookup: %s", name)
		}
		return &net.Interface{Name: "ens4"}, nil
	}
	defaultRouteIfaceFn = func() string { return "ens4" }

	got, err := resolveExternalIface("ens4", nil)
	if err != nil {
		t.Fatalf("resolveExternalIface(ens4) error = %v", err)
	}
	if got != "ens4" {
		t.Fatalf("resolveExternalIface(ens4) = %q, want %q", got, "ens4")
	}
}

func TestIsDefaultRoute(t *testing.T) {
	tests := []struct {
		name  string
		route netlink.Route
		want  bool
	}{
		{
			name:  "nil Dst is default",
			route: netlink.Route{Dst: nil},
			want:  true,
		},
		{
			name: "0.0.0.0/0 is default (netlink v1.3.0+)",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.IPv4zero,
				Mask: net.CIDRMask(0, 32),
			}},
			want: true,
		},
		{
			name: "10.128.0.1/32 is not default",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.ParseIP("10.128.0.1"),
				Mask: net.CIDRMask(32, 32),
			}},
			want: false,
		},
		{
			name: "172.17.0.0/16 is not default",
			route: netlink.Route{Dst: &net.IPNet{
				IP:   net.ParseIP("172.17.0.0"),
				Mask: net.CIDRMask(16, 32),
			}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDefaultRoute(tt.route); got != tt.want {
				t.Errorf("isDefaultRoute() = %v, want %v", got, tt.want)
			}
		})
	}
}
