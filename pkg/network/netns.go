//go:build linux
// +build linux

package network

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// NetNSNetwork manages network namespaces for microVMs.
//
// This approach allows ALL VMs to use the same IP address (e.g., 172.16.0.2) because
// each VM runs in its own network namespace with isolated routing tables and interfaces.
// This is the approach BuildBuddy uses and simplifies snapshot restore significantly:
// - No slot-based TAP allocation needed
// - Snapshot can be created with a fixed IP
// - All clones use the same IP without conflict
//
// Architecture:
//
//	Host namespace:
//	  - veth-{vmid}-host (connected to bridge)
//	VM namespace (netns-{vmid}):
//	  - veth-{vmid}-vm (same IP for all VMs, e.g., 172.16.0.2)
//	  - TAP device for Firecracker (tap0)
//	  - NAT masquerade from VM subnet to veth
type NetNSNetwork struct {
	bridgeName    string
	subnet        *net.IPNet
	gateway       net.IP
	vmIP          net.IP // Same IP for all VMs
	externalIface string
	mtu           int // MTU to use, matches external interface (GCP uses 1460)
	namespaces    map[string]*VMNamespace // vmID -> namespace info
	mu            sync.Mutex
	logger        *logrus.Entry
}

// VMNamespace holds info about a VM's network namespace
type VMNamespace struct {
	Name     string // Namespace name
	Path     string // Path to namespace file
	VethHost string // Host-side veth name
	VethVM   string // VM-side veth name (inside namespace)
	TapName  string // TAP device name inside namespace
	IP       net.IP // IP address (same for all VMs)
	Gateway  net.IP // Gateway IP
	MAC      string // MAC address for TAP
	Handle   netns.NsHandle
}

// NetNSConfig holds configuration for network namespace setup
type NetNSConfig struct {
	BridgeName    string
	Subnet        string // CIDR notation, e.g., "172.16.0.0/24"
	ExternalIface string
	Logger        *logrus.Logger
}

// NewNetNSNetwork creates a new network namespace manager
func NewNetNSNetwork(cfg NetNSConfig) (*NetNSNetwork, error) {
	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet CIDR: %w", err)
	}

	gateway := incrementIP(subnet.IP, 1)
	vmIP := incrementIP(subnet.IP, 2) // All VMs use .2

	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}

	// Detect MTU from external interface (GCP uses 1460, not 1500)
	mtu := detectInterfaceMTU(cfg.ExternalIface)
	if mtu <= 0 {
		mtu = 1460 // Safe default for GCP
	}

	return &NetNSNetwork{
		bridgeName:    cfg.BridgeName,
		subnet:        subnet,
		gateway:       gateway,
		vmIP:          vmIP,
		externalIface: cfg.ExternalIface,
		mtu:           mtu,
		namespaces:    make(map[string]*VMNamespace),
		logger:        logger.WithField("component", "netns-network"),
	}, nil
}

// Setup initializes the bridge (namespaces are created per-VM)
func (n *NetNSNetwork) Setup() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.logger.WithFields(logrus.Fields{
		"bridge":  n.bridgeName,
		"subnet":  n.subnet.String(),
		"gateway": n.gateway.String(),
		"vm_ip":   n.vmIP.String(),
	}).Info("Setting up network namespace infrastructure")

	// Create bridge
	if err := n.createBridge(); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	// Enable IP forwarding
	if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		n.logger.WithError(err).Warn("Failed to enable IP forwarding")
	}

	// Setup NAT for bridge traffic to external interface
	if err := n.setupBridgeNAT(); err != nil {
		return fmt.Errorf("failed to setup bridge NAT: %w", err)
	}

	return nil
}

// createBridge creates the bridge interface (same as NAT approach)
func (n *NetNSNetwork) createBridge() error {
	link, err := netlink.LinkByName(n.bridgeName)
	if err == nil {
		n.logger.WithField("bridge", n.bridgeName).Debug("Bridge already exists")
		// Ensure MTU matches external interface
		if err := netlink.LinkSetMTU(link, n.mtu); err != nil {
			n.logger.WithError(err).Warn("Failed to set MTU on existing bridge")
		}
		return netlink.LinkSetUp(link)
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: n.bridgeName,
			MTU:  n.mtu,
		},
	}

	if err := netlink.LinkAdd(bridge); err != nil {
		return fmt.Errorf("failed to add bridge: %w", err)
	}

	link, err = netlink.LinkByName(n.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get bridge link: %w", err)
	}

	// Add gateway IP to bridge
	ones, _ := n.subnet.Mask.Size()
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   n.gateway,
			Mask: net.CIDRMask(ones, 32),
		},
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if err.Error() != "file exists" {
			return fmt.Errorf("failed to add address to bridge: %w", err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up bridge: %w", err)
	}

	n.logger.WithFields(logrus.Fields{
		"bridge": n.bridgeName,
		"mtu":    n.mtu,
	}).Info("Bridge created successfully")
	return nil
}

// setupBridgeNAT configures NAT for traffic from bridge to external
func (n *NetNSNetwork) setupBridgeNAT() error {
	subnetCIDR := n.subnet.String()

	// MASQUERADE for outbound traffic
	masqueradeRule := []string{
		"-t", "nat", "-C", "POSTROUTING",
		"-s", subnetCIDR,
		"-o", n.externalIface,
		"-j", "MASQUERADE",
	}
	if err := exec.Command("iptables", masqueradeRule...).Run(); err != nil {
		addRule := []string{
			"-t", "nat", "-A", "POSTROUTING",
			"-s", subnetCIDR,
			"-o", n.externalIface,
			"-j", "MASQUERADE",
		}
		if err := exec.Command("iptables", addRule...).Run(); err != nil {
			return fmt.Errorf("failed to add MASQUERADE rule: %w", err)
		}
	}

	// FORWARD rules
	forwardOut := []string{"-C", "FORWARD", "-i", n.bridgeName, "-o", n.externalIface, "-j", "ACCEPT"}
	if err := exec.Command("iptables", forwardOut...).Run(); err != nil {
		addRule := []string{"-A", "FORWARD", "-i", n.bridgeName, "-o", n.externalIface, "-j", "ACCEPT"}
		if err := exec.Command("iptables", addRule...).Run(); err != nil {
			return fmt.Errorf("failed to add forward out rule: %w", err)
		}
	}

	forwardIn := []string{"-C", "FORWARD", "-i", n.externalIface, "-o", n.bridgeName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := exec.Command("iptables", forwardIn...).Run(); err != nil {
		addRule := []string{"-A", "FORWARD", "-i", n.externalIface, "-o", n.bridgeName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
		if err := exec.Command("iptables", addRule...).Run(); err != nil {
			return fmt.Errorf("failed to add forward in rule: %w", err)
		}
	}

	// TCP MSS clamping: fixes HTTPS/TLS through NAT with non-standard MTU
	mssClampRule := []string{"-C", "FORWARD", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}
	if err := exec.Command("iptables", mssClampRule...).Run(); err != nil {
		addRule := []string{"-I", "FORWARD", "1", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}
		if err := exec.Command("iptables", addRule...).Run(); err != nil {
			n.logger.WithError(err).Warn("Failed to add TCP MSS clamping rule")
		}
	}

	n.logger.Info("Bridge NAT rules configured successfully")
	return nil
}

// CreateNamespaceForVM creates a network namespace and devices for a VM
func (n *NetNSNetwork) CreateNamespaceForVM(vmID string) (*VMNamespace, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	shortID := vmID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	nsName := fmt.Sprintf("fc-%s", shortID)
	vethHost := fmt.Sprintf("veth-%s-h", shortID)
	vethVM := fmt.Sprintf("veth-%s-v", shortID)
	tapName := "tap0" // Standard name inside namespace

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"namespace": nsName,
		"veth_host": vethHost,
		"veth_vm":   vethVM,
		"tap":       tapName,
		"ip":        n.vmIP.String(),
	}).Info("Creating network namespace for VM")

	// Lock OS thread for namespace operations
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save current namespace
	origNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get current namespace: %w", err)
	}
	defer origNS.Close()

	// Create new namespace
	newNS, err := netns.NewNamed(nsName)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}

	// Switch back to original namespace for veth creation
	if err := netns.Set(origNS); err != nil {
		newNS.Close()
		return nil, fmt.Errorf("failed to switch back to original namespace: %w", err)
	}

	// Create veth pair in host namespace with correct MTU
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: vethHost,
			MTU:  n.mtu,
		},
		PeerName: vethVM,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to create veth pair: %w", err)
	}

	// Get veth peer (vm side)
	vethVMLink, err := netlink.LinkByName(vethVM)
	if err != nil {
		netlink.LinkDel(veth)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get veth vm link: %w", err)
	}

	// Move veth VM side to new namespace
	if err := netlink.LinkSetNsFd(vethVMLink, int(newNS)); err != nil {
		netlink.LinkDel(veth)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to move veth to namespace: %w", err)
	}

	// Attach veth host side to bridge
	vethHostLink, err := netlink.LinkByName(vethHost)
	if err != nil {
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get veth host link: %w", err)
	}

	bridge, err := netlink.LinkByName(n.bridgeName)
	if err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get bridge: %w", err)
	}

	if err := netlink.LinkSetMaster(vethHostLink, bridge); err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to attach veth to bridge: %w", err)
	}

	// Bring up veth host side
	if err := netlink.LinkSetUp(vethHostLink); err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up veth host: %w", err)
	}

	// Configure inside the namespace
	if err := netns.Set(newNS); err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to switch to new namespace: %w", err)
	}

	// Configure veth VM side
	vethVMLink, err = netlink.LinkByName(vethVM)
	if err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get veth vm link in namespace: %w", err)
	}

	// Add IP address to veth VM side
	ones, _ := n.subnet.Mask.Size()
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   n.vmIP,
			Mask: net.CIDRMask(ones, 32),
		},
	}
	if err := netlink.AddrAdd(vethVMLink, addr); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to add IP to veth: %w", err)
	}

	// Bring up veth VM side
	if err := netlink.LinkSetUp(vethVMLink); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up veth vm: %w", err)
	}

	// Bring up loopback
	lo, err := netlink.LinkByName("lo")
	if err == nil {
		netlink.LinkSetUp(lo)
	}

	// Add default route via gateway
	route := &netlink.Route{
		Gw: n.gateway,
	}
	if err := netlink.RouteAdd(route); err != nil {
		n.logger.WithError(err).Warn("Failed to add default route (may already exist)")
	}

	// Create TAP device inside namespace with correct MTU
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: tapName,
			MTU:  n.mtu,
		},
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_DEFAULTS,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to create TAP in namespace: %w", err)
	}

	// Bring up TAP
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get TAP link: %w", err)
	}
	if err := netlink.LinkSetUp(tapLink); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up TAP: %w", err)
	}

	// Switch back to original namespace
	if err := netns.Set(origNS); err != nil {
		return nil, fmt.Errorf("failed to switch back to original namespace: %w", err)
	}

	nsInfo := &VMNamespace{
		Name:     nsName,
		Path:     filepath.Join("/var/run/netns", nsName),
		VethHost: vethHost,
		VethVM:   vethVM,
		TapName:  tapName,
		IP:       n.vmIP,
		Gateway:  n.gateway,
		MAC:      generateMAC(n.vmIP),
		Handle:   newNS,
	}

	n.namespaces[vmID] = nsInfo

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"namespace": nsName,
		"ip":        n.vmIP.String(),
	}).Info("Network namespace created successfully")

	return nsInfo, nil
}

// ReleaseNamespace removes a VM's network namespace
func (n *NetNSNetwork) ReleaseNamespace(vmID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	nsInfo, exists := n.namespaces[vmID]
	if !exists {
		return nil
	}

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"namespace": nsInfo.Name,
	}).Info("Releasing network namespace")

	// Delete veth host side (automatically removes peer)
	if link, err := netlink.LinkByName(nsInfo.VethHost); err == nil {
		netlink.LinkDel(link)
	}

	// Close namespace handle
	nsInfo.Handle.Close()

	// Delete named namespace
	netns.DeleteNamed(nsInfo.Name)

	delete(n.namespaces, vmID)

	return nil
}

// GetNamespace returns info about a VM's namespace
func (n *NetNSNetwork) GetNamespace(vmID string) (*VMNamespace, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	nsInfo, exists := n.namespaces[vmID]
	if !exists {
		return nil, fmt.Errorf("namespace not found for VM: %s", vmID)
	}
	return nsInfo, nil
}

// GetTapDevice returns a TapDevice-compatible struct for integration
func (ns *VMNamespace) GetTapDevice(subnet *net.IPNet) *TapDevice {
	return &TapDevice{
		Name:       ns.TapName,
		IP:         ns.IP,
		Gateway:    ns.Gateway,
		Subnet:     subnet,
		MAC:        ns.MAC,
		BridgeName: "", // Not directly on bridge
	}
}

// RunInNamespace runs a function in the VM's network namespace
func (n *NetNSNetwork) RunInNamespace(vmID string, fn func() error) error {
	n.mu.Lock()
	nsInfo, exists := n.namespaces[vmID]
	n.mu.Unlock()

	if !exists {
		return fmt.Errorf("namespace not found for VM: %s", vmID)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current namespace: %w", err)
	}
	defer origNS.Close()

	if err := netns.Set(nsInfo.Handle); err != nil {
		return fmt.Errorf("failed to switch to VM namespace: %w", err)
	}
	defer netns.Set(origNS)

	return fn()
}

// GetFirecrackerNetNSPath returns the path to the namespace file for Firecracker
// Firecracker can be launched with --network-namespace flag
func (ns *VMNamespace) GetFirecrackerNetNSPath() string {
	return ns.Path
}

// Cleanup removes all namespaces and the bridge
func (n *NetNSNetwork) Cleanup() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.logger.Info("Cleaning up network namespaces")

	// Release all namespaces
	for vmID, nsInfo := range n.namespaces {
		if link, err := netlink.LinkByName(nsInfo.VethHost); err == nil {
			netlink.LinkDel(link)
		}
		nsInfo.Handle.Close()
		netns.DeleteNamed(nsInfo.Name)
		delete(n.namespaces, vmID)
	}

	// Remove bridge
	if link, err := netlink.LinkByName(n.bridgeName); err == nil {
		netlink.LinkDel(link)
	}

	// Remove NAT rules
	subnetCIDR := n.subnet.String()
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", subnetCIDR, "-o", n.externalIface, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-D", "FORWARD",
		"-i", n.bridgeName, "-o", n.externalIface, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD",
		"-i", n.externalIface, "-o", n.bridgeName,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()

	return nil
}

// GetVMIP returns the IP address used by all VMs
func (n *NetNSNetwork) GetVMIP() net.IP {
	return n.vmIP
}

// GetSubnet returns the subnet used by the network
func (n *NetNSNetwork) GetSubnet() *net.IPNet {
	return n.subnet
}

// EnsureNetNSDir ensures /var/run/netns exists
func EnsureNetNSDir() error {
	return os.MkdirAll("/var/run/netns", 0755)
}
