//go:build !linux
// +build !linux

package network

import (
	"fmt"
	"net"

	"github.com/sirupsen/logrus"
)

// NetNSNetwork is a stub on non-Linux platforms.
type NetNSNetwork struct{}

// VMNamespace is a stub on non-Linux platforms.
type VMNamespace struct {
	Name            string
	Path            string
	VethHost        string
	VethVM          string
	TapName         string
	IP              net.IP
	Gateway         net.IP
	MAC             string
	Slot            int
	HostReachableIP net.IP
}

// NetNSConfig holds configuration for network namespace setup.
type NetNSConfig struct {
	BridgeName    string
	Subnet        string
	ExternalIface string
	Logger        *logrus.Logger
}

func NewNetNSNetwork(_ NetNSConfig) (*NetNSNetwork, error) {
	return nil, fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) Setup() error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) CreateNamespaceForVM(_ string, _ int) (*VMNamespace, error) {
	return nil, fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) ReleaseNamespace(_ string) error {
	return nil
}

func (n *NetNSNetwork) GetNamespace(_ string) (*VMNamespace, error) {
	return nil, fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) RunInNamespace(_ string, _ func() error) error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) ForwardPort(_ string, _ int) error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) ForwardPorts(_ string, _ []int) error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) EmergencyBlockEgress(_ string) error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) EmergencyUnblockEgress(_ string) error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}

func (n *NetNSNetwork) Cleanup() error {
	return nil
}

func (n *NetNSNetwork) GetVMIP() net.IP {
	return nil
}

func (n *NetNSNetwork) GetSubnet() *net.IPNet {
	return nil
}

func (ns *VMNamespace) GetTapDevice(_ *net.IPNet) *TapDevice {
	return nil
}

func (ns *VMNamespace) GetFirecrackerNetNSPath() string {
	return ""
}

// PoolEntry is a stub on non-Linux platforms.
type PoolEntry struct {
	PlaceholderID string
	Slot          int
	NsInfo        *VMNamespace
}

func (n *NetNSNetwork) StartPool(_ int, _ func() int, _ func(int)) {}

func (n *NetNSNetwork) StopPool() {}

func (n *NetNSNetwork) TryClaimFromPool() *PoolEntry { return nil }

func (n *NetNSNetwork) ReassignNamespace(_, _ string) {}

func (n *NetNSNetwork) NotifyReplenish() {}

func EnsureNetNSDir() error {
	return fmt.Errorf("network namespaces are only supported on Linux")
}
