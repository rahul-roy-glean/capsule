package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/network"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

const (
	builderNetworkSlotStart = 128
	builderNetworkSlotEnd   = 191
	builderSlotLockDir      = "/var/run/capsule-builder-netns"
)

type builderNetwork struct {
	resourceID string
	manager    *network.NetNSNetwork
	namespace  *network.VMNamespace
	tap        *network.TapDevice
	netCfg     network.NetworkConfig
	slotLock   *os.File
}

func setupBuilderNetwork(vmID string, logger *logrus.Logger) (*builderNetwork, error) {
	if err := network.EnsureNetNSDir(); err != nil {
		return nil, fmt.Errorf("ensure netns dir: %w", err)
	}

	netnsNet, err := network.NewNetNSNetwork(network.NetNSConfig{
		BridgeName:    "fcbr0",
		Subnet:        "172.16.0.0/24",
		ExternalIface: getDefaultIface(),
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create netns network: %w", err)
	}

	if err := netnsNet.Setup(); err != nil {
		return nil, fmt.Errorf("setup netns network: %w", err)
	}

	resourceID := newBuilderNetworkResourceID()
	var nsInfo *network.VMNamespace
	var slotLock *os.File
	for candidate := builderNetworkSlotStart; candidate <= builderNetworkSlotEnd; candidate++ {
		if builderSlotInUse(candidate) {
			continue
		}
		lockFile, err := acquireBuilderSlotLock(candidate)
		if err != nil {
			if isBuilderSlotLockBusy(err) {
				continue
			}
			return nil, fmt.Errorf("acquire builder slot lock for slot %d: %w", candidate, err)
		}

		nsInfo, err = netnsNet.CreateNamespaceForVM(resourceID, candidate)
		if err == nil {
			slotLock = lockFile
			break
		}
		_ = releaseBuilderSlotLock(lockFile)
		if isLikelyBuilderNetworkConflict(err) {
			continue
		}
		return nil, fmt.Errorf("create builder namespace: %w", err)
	}
	if nsInfo == nil {
		return nil, fmt.Errorf("no builder network slots available in reserved range %d-%d", builderNetworkSlotStart, builderNetworkSlotEnd)
	}

	tap := nsInfo.GetTapDevice(netnsNet.GetSubnet())
	if tap == nil {
		_ = releaseBuilderSlotLock(slotLock)
		_ = netnsNet.ReleaseNamespace(resourceID)
		return nil, errors.New("builder namespace did not return a TAP device")
	}
	if nsInfo.HostReachableIP == nil {
		_ = releaseBuilderSlotLock(slotLock)
		_ = netnsNet.ReleaseNamespace(resourceID)
		return nil, errors.New("builder namespace did not expose a host-reachable IP")
	}

	if err := netnsNet.ForwardPorts(resourceID, []int{
		snapshot.ThawAgentHealthPort,
		snapshot.ThawAgentDebugPort,
	}); err != nil {
		_ = releaseBuilderSlotLock(slotLock)
		_ = netnsNet.ReleaseNamespace(resourceID)
		return nil, fmt.Errorf("forward capsule-thaw-agent ports into builder namespace: %w", err)
	}

	return &builderNetwork{
		resourceID: resourceID,
		manager:    netnsNet,
		namespace:  nsInfo,
		tap:        tap,
		netCfg:     tap.GetNetworkConfig(),
		slotLock:   slotLock,
	}, nil
}

func (n *builderNetwork) cleanup() error {
	if n == nil || n.manager == nil {
		return nil
	}

	var errs []error
	if n.resourceID != "" {
		if err := n.manager.ReleaseNamespace(n.resourceID); err != nil {
			errs = append(errs, fmt.Errorf("release builder namespace: %w", err))
		}
		n.resourceID = ""
	}
	if n.slotLock != nil {
		if err := releaseBuilderSlotLock(n.slotLock); err != nil {
			errs = append(errs, fmt.Errorf("release builder slot lock: %w", err))
		}
		n.slotLock = nil
	}
	n.manager = nil
	n.namespace = nil
	n.tap = nil
	return errors.Join(errs...)
}

func (n *builderNetwork) tapName() string {
	if n == nil || n.tap == nil {
		return ""
	}
	return n.tap.Name
}

func (n *builderNetwork) guestMAC() string {
	if n == nil {
		return ""
	}
	return n.netCfg.MAC
}

func (n *builderNetwork) guestIP() string {
	if n == nil {
		return ""
	}
	return stripCIDRSuffix(n.netCfg.IP)
}

func (n *builderNetwork) gatewayIP() string {
	if n == nil {
		return ""
	}
	return n.netCfg.Gateway
}

func (n *builderNetwork) netmask() string {
	if n == nil {
		return ""
	}
	return n.netCfg.Netmask
}

func (n *builderNetwork) pollIP() string {
	if n == nil || n.namespace == nil || n.namespace.HostReachableIP == nil {
		return ""
	}
	return n.namespace.HostReachableIP.String()
}

func (n *builderNetwork) firecrackerNetNSPath() string {
	if n == nil || n.namespace == nil {
		return ""
	}
	return n.namespace.GetFirecrackerNetNSPath()
}

func stripCIDRSuffix(ip string) string {
	if idx := strings.Index(ip, "/"); idx >= 0 {
		return ip[:idx]
	}
	return ip
}

func newBuilderNetworkResourceID() string {
	raw := strings.ReplaceAll(uuid.New().String(), "-", "")
	return "sb" + raw[:6]
}

func builderSlotInUse(slot int) bool {
	targetIP := fmt.Sprintf("10.200.%d.1", slot)
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if strings.HasPrefix(addr.String(), targetIP+"/") {
				return true
			}
		}
	}
	return false
}

func isLikelyBuilderNetworkConflict(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "file exists") ||
		strings.Contains(lower, "already exists") ||
		strings.Contains(lower, "address already in use")
}

func acquireBuilderSlotLock(slot int) (*os.File, error) {
	if err := os.MkdirAll(builderSlotLockDir, 0755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(builderSlotLockDir, fmt.Sprintf("slot-%d.lock", slot))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func releaseBuilderSlotLock(f *os.File) error {
	if f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}

func isBuilderSlotLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
