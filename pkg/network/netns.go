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

// NetNSNetwork manages per-VM network namespaces with point-to-point veth routing.
//
// Each VM gets its own network namespace containing:
//   - An inner bridge (br-vm) with the snapshot-expected gateway (172.16.0.1/24)
//   - A TAP device (tap-slot-0) attached to br-vm
//   - A veth pair connecting the namespace to the host with 10.200.{slot}.0/30 addressing
//
// This provides complete VM isolation by construction — no shared L2 domain,
// no path between namespaces, no VM-to-VM or VM-to-host connectivity.
type NetNSNetwork struct {
	subnet        *net.IPNet
	gateway       net.IP
	vmIP          net.IP // Same IP for all VMs (172.16.0.2)
	externalIface string
	extMTU        int                     // MTU of external interface (e.g., 1460 on GCP)
	namespaces    map[string]*VMNamespace // vmID -> namespace info
	mu            sync.Mutex
	logger        *logrus.Entry
}

// VMNamespace holds info about a VM's network namespace
type VMNamespace struct {
	Name            string // Namespace name (fc-{shortID})
	Path            string // Path to namespace file (/var/run/netns/fc-{shortID})
	VethHost        string // Host-side veth name
	VethVM          string // VM-side veth name (inside namespace)
	TapName         string // TAP device name inside namespace (always tap-slot-0)
	IP              net.IP // Guest IP address (same for all VMs: 172.16.0.2)
	Gateway         net.IP // Gateway IP (172.16.0.1)
	MAC             string // MAC address for TAP
	Slot            int    // Slot number (determines veth addressing: 10.200.{slot}.0/30)
	HostReachableIP net.IP // IP reachable from host namespace (10.200.{slot}.2)
	Handle          netns.NsHandle
}

// NetNSConfig holds configuration for network namespace setup
type NetNSConfig struct {
	BridgeName    string // Unused in per-VM namespace mode; kept for config compatibility
	Subnet        string // CIDR notation for guest subnet, e.g., "172.16.0.0/24"
	ExternalIface string
	Logger        *logrus.Logger
}

// vethSupernet is the supernet covering all per-VM veth /30 subnets.
const vethSupernet = "10.200.0.0/16"

// procSysIPv4ConfDir is overridden in tests.
var procSysIPv4ConfDir = "/proc/sys/net/ipv4/conf"

// innerBridgeName is the bridge created inside each namespace.
const innerBridgeName = "br-vm"

// snapshotTAPNameNetNS is the TAP device name inside each namespace,
// matching the name baked into the snapshot state file.
const snapshotTAPNameNetNS = "tap-slot-0"

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

	extIface := cfg.ExternalIface
	// Auto-detect the external interface from the default route if the
	// configured one doesn't exist (e.g., "eth0" on hosts that use "ens4").
	if _, err := net.InterfaceByName(extIface); err != nil {
		if detected := detectDefaultRouteIface(); detected != "" {
			logger.WithField("component", "netns-network").WithFields(logrus.Fields{
				"configured": extIface,
				"detected":   detected,
			}).Info("Configured external interface not found, using detected default route interface")
			extIface = detected
		}
	}

	// Detect MTU from external interface to avoid fragmentation.
	// GCP uses 1460, AWS uses 9001, default is 1500.
	extMTU := 1500
	if iface, err := net.InterfaceByName(extIface); err == nil {
		extMTU = iface.MTU
	}

	return &NetNSNetwork{
		subnet:        subnet,
		gateway:       gateway,
		vmIP:          vmIP,
		externalIface: extIface,
		extMTU:        extMTU,
		namespaces:    make(map[string]*VMNamespace),
		logger:        logger.WithField("component", "netns-network"),
	}, nil
}

// Setup enables IP forwarding and configures host-level MASQUERADE for the
// veth supernet (10.200.0.0/16). No shared bridge is created — each VM gets
// its own namespace with an inner bridge.
func (n *NetNSNetwork) Setup() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.logger.WithFields(logrus.Fields{
		"subnet":  n.subnet.String(),
		"gateway": n.gateway.String(),
		"vm_ip":   n.vmIP.String(),
	}).Info("Setting up per-VM network namespace infrastructure")

	// Enable IP forwarding
	if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		n.logger.WithError(err).Warn("Failed to enable IP forwarding")
	}

	// Host-level MASQUERADE for veth supernet so traffic from namespaces can reach the internet.
	// Each namespace's veth-h is in 10.200.{slot}.0/30; we cover them all with 10.200.0.0/16.
	addIPTablesRuleIfMissing("iptables",
		[]string{"-t", "nat", "-C", "POSTROUTING", "-s", vethSupernet, "-o", n.externalIface, "-j", "MASQUERADE"},
		[]string{"-t", "nat", "-A", "POSTROUTING", "-s", vethSupernet, "-o", n.externalIface, "-j", "MASQUERADE"},
	)

	// FORWARD rules for veth traffic to/from external interface
	addIPTablesRuleIfMissing("iptables",
		[]string{"-C", "FORWARD", "-s", vethSupernet, "-o", n.externalIface, "-j", "ACCEPT"},
		[]string{"-A", "FORWARD", "-s", vethSupernet, "-o", n.externalIface, "-j", "ACCEPT"},
	)
	addIPTablesRuleIfMissing("iptables",
		[]string{"-C", "FORWARD", "-d", vethSupernet, "-i", n.externalIface, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		[]string{"-A", "FORWARD", "-d", vethSupernet, "-i", n.externalIface, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	)

	// Clamp TCP MSS to path MTU
	addIPTablesRuleIfMissing("iptables",
		[]string{"-t", "mangle", "-C", "FORWARD", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"},
		[]string{"-t", "mangle", "-A", "FORWARD", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"},
	)

	n.logger.Info("Host-level NAT rules configured for per-VM namespaces")
	return nil
}

// addIPTablesRuleIfMissing checks if a rule exists (checkArgs) and adds it (addArgs) if missing.
// detectDefaultRouteIface returns the network interface used by the default route.
func detectDefaultRouteIface() string {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return ""
	}
	for _, r := range routes {
		if r.Dst == nil { // default route
			link, err := netlink.LinkByIndex(r.LinkIndex)
			if err != nil {
				continue
			}
			return link.Attrs().Name
		}
	}
	return ""
}

func addIPTablesRuleIfMissing(binary string, checkArgs, addArgs []string) error {
	if err := exec.Command(binary, checkArgs...).Run(); err != nil {
		if err := exec.Command(binary, addArgs...).Run(); err != nil {
			return fmt.Errorf("failed to add iptables rule: %w", err)
		}
	}
	return nil
}

func setInterfaceSysctl(iface, key, value string) error {
	if iface == "" {
		return fmt.Errorf("sysctl interface is empty")
	}
	path := filepath.Join(procSysIPv4ConfDir, iface, key)
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("write %s=%s: %w", path, value, err)
	}
	return nil
}

func configureHostVethSysctls(iface string) error {
	// Per-VM veth links carry traffic that is intentionally DNATed into the
	// namespace and bridged again to the guest. Strict rp_filter on ephemeral
	// veth-h interfaces treats that return path as martian traffic and breaks
	// host -> 10.200.{slot}.2 readiness probes. Loose mode preserves source
	// validation while allowing this expected asymmetric path.
	if err := setInterfaceSysctl(iface, "rp_filter", "2"); err != nil {
		return err
	}
	return nil
}

// CreateNamespaceForVM creates an isolated network namespace for a VM.
//
// The slot parameter determines the veth /30 subnet (10.200.{slot}.0/30).
// Inside the namespace, an inner bridge (br-vm) with gateway 172.16.0.1/24
// and TAP device (tap-slot-0) are created, replicating the topology the
// snapshot expects.
func (n *NetNSNetwork) CreateNamespaceForVM(vmID string, slot int) (*VMNamespace, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	shortID := vmID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	nsName := fmt.Sprintf("fc-%s", shortID)
	vethHost := fmt.Sprintf("veth-%s-h", shortID)
	vethVM := fmt.Sprintf("veth-%s-v", shortID)

	// Veth addressing: 10.200.{slot}.0/30
	// Host side: 10.200.{slot}.1, Namespace side: 10.200.{slot}.2
	vethHostIP := net.IPv4(10, 200, byte(slot), 1)
	vethVMIP := net.IPv4(10, 200, byte(slot), 2)
	vethMask := net.CIDRMask(30, 32)

	n.logger.WithFields(logrus.Fields{
		"vm_id":        vmID,
		"namespace":    nsName,
		"slot":         slot,
		"veth_host":    vethHost,
		"veth_vm":      vethVM,
		"veth_host_ip": vethHostIP.String(),
		"veth_vm_ip":   vethVMIP.String(),
	}).Info("Creating per-VM network namespace")

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save current (host) namespace
	origNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get current namespace: %w", err)
	}
	defer origNS.Close()

	// 1. Create namespace
	newNS, err := netns.NewNamed(nsName)
	if err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}

	// Switch back to host namespace for veth creation
	if err := netns.Set(origNS); err != nil {
		newNS.Close()
		return nil, fmt.Errorf("failed to switch back to original namespace: %w", err)
	}

	// 2. Create veth pair in host namespace (MTU matches external interface to avoid fragmentation)
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: vethHost, MTU: n.extMTU},
		PeerName:  vethVM,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to create veth pair: %w", err)
	}

	// 3. Move veth-v into namespace
	vethVMLink, err := netlink.LinkByName(vethVM)
	if err != nil {
		netlink.LinkDel(veth)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get veth vm link: %w", err)
	}
	if err := netlink.LinkSetNsFd(vethVMLink, int(newNS)); err != nil {
		netlink.LinkDel(veth)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to move veth to namespace: %w", err)
	}

	// 4. Host side: assign 10.200.{slot}.1/30, bring up
	vethHostLink, err := netlink.LinkByName(vethHost)
	if err != nil {
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get veth host link: %w", err)
	}
	if err := netlink.AddrAdd(vethHostLink, &netlink.Addr{
		IPNet: &net.IPNet{IP: vethHostIP, Mask: vethMask},
	}); err != nil {
		if err.Error() != "file exists" {
			netlink.LinkDel(vethHostLink)
			netns.DeleteNamed(nsName)
			newNS.Close()
			return nil, fmt.Errorf("failed to add IP to veth host: %w", err)
		}
	}
	if err := netlink.LinkSetUp(vethHostLink); err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up veth host: %w", err)
	}
	if err := configureHostVethSysctls(vethHost); err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to configure host veth sysctls: %w", err)
	}
	n.logger.WithFields(logrus.Fields{
		"interface": vethHost,
		"rp_filter": "2",
	}).Debug("Configured host-side veth sysctls")

	// 6. Configure inside the namespace (netlink operations respect thread namespace)
	if err := netns.Set(newNS); err != nil {
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to switch to new namespace: %w", err)
	}

	// 6h. Bring up loopback
	if lo, err := netlink.LinkByName("lo"); err == nil {
		netlink.LinkSetUp(lo)
	}

	// 6a. Assign 10.200.{slot}.2/30 to veth-v, bring up
	vethVMLink, err = netlink.LinkByName(vethVM)
	if err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get veth vm link in namespace: %w", err)
	}
	if err := netlink.AddrAdd(vethVMLink, &netlink.Addr{
		IPNet: &net.IPNet{IP: vethVMIP, Mask: vethMask},
	}); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to add IP to veth vm: %w", err)
	}
	if err := netlink.LinkSetMTU(vethVMLink, n.extMTU); err != nil {
		n.logger.WithError(err).Warn("Failed to set MTU on veth vm")
	}
	if err := netlink.LinkSetUp(vethVMLink); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up veth vm: %w", err)
	}

	// 6b. Create inner bridge br-vm with gateway IP 172.16.0.1/24
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: innerBridgeName, MTU: n.extMTU},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to create inner bridge: %w", err)
	}
	brLink, err := netlink.LinkByName(innerBridgeName)
	if err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get inner bridge link: %w", err)
	}
	ones, _ := n.subnet.Mask.Size()
	if err := netlink.AddrAdd(brLink, &netlink.Addr{
		IPNet: &net.IPNet{IP: n.gateway, Mask: net.CIDRMask(ones, 32)},
	}); err != nil {
		if err.Error() != "file exists" {
			netns.Set(origNS)
			netlink.LinkDel(vethHostLink)
			netns.DeleteNamed(nsName)
			newNS.Close()
			return nil, fmt.Errorf("failed to add IP to inner bridge: %w", err)
		}
	}
	if err := netlink.LinkSetMTU(brLink, n.extMTU); err != nil {
		n.logger.WithError(err).Warn("Failed to set MTU on inner bridge")
	}
	if err := netlink.LinkSetUp(brLink); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up inner bridge: %w", err)
	}

	// 6c. Create TAP device tap-slot-0, attach to br-vm, bring up
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: snapshotTAPNameNetNS, MTU: n.extMTU},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Flags:     netlink.TUNTAP_DEFAULTS,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to create TAP in namespace: %w", err)
	}
	tapLink, err := netlink.LinkByName(snapshotTAPNameNetNS)
	if err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to get TAP link: %w", err)
	}
	if err := netlink.LinkSetMTU(tapLink, n.extMTU); err != nil {
		n.logger.WithError(err).Warn("Failed to set MTU on TAP")
	}
	if err := netlink.LinkSetMaster(tapLink, brLink); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to attach TAP to inner bridge: %w", err)
	}
	if err := netlink.LinkSetUp(tapLink); err != nil {
		netns.Set(origNS)
		netlink.LinkDel(vethHostLink)
		netns.DeleteNamed(nsName)
		newNS.Close()
		return nil, fmt.Errorf("failed to bring up TAP: %w", err)
	}

	// 6d. Add default route via host-side veth IP (10.200.{slot}.1)
	if err := netlink.RouteAdd(&netlink.Route{Gw: vethHostIP}); err != nil {
		n.logger.WithError(err).Warn("Failed to add default route in namespace (may already exist)")
	}

	// Switch back to host namespace before running iptables via ip-netns-exec
	if err := netns.Set(origNS); err != nil {
		return nil, fmt.Errorf("failed to switch back to original namespace: %w", err)
	}

	// 6e. MASQUERADE inside namespace: translate 172.16.0.0/24 -> veth-v for outbound
	subnetCIDR := n.subnet.String()
	exec.Command("ip", "netns", "exec", nsName,
		"iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", subnetCIDR, "-o", vethVM, "-j", "MASQUERADE").Run()

	// 6f. FORWARD: allow bridge -> veth outbound
	exec.Command("ip", "netns", "exec", nsName,
		"iptables", "-A", "FORWARD",
		"-i", innerBridgeName, "-o", vethVM, "-j", "ACCEPT").Run()

	// 6g. FORWARD: allow veth -> bridge for established connections
	exec.Command("ip", "netns", "exec", nsName,
		"iptables", "-A", "FORWARD",
		"-i", vethVM, "-o", innerBridgeName,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()

	nsInfo := &VMNamespace{
		Name:            nsName,
		Path:            filepath.Join("/var/run/netns", nsName),
		VethHost:        vethHost,
		VethVM:          vethVM,
		TapName:         snapshotTAPNameNetNS,
		IP:              n.vmIP,
		Gateway:         n.gateway,
		MAC:             generateMAC(n.vmIP),
		Slot:            slot,
		HostReachableIP: vethVMIP, // 10.200.{slot}.2 — reachable from host via veth pair
		Handle:          newNS,
	}

	n.namespaces[vmID] = nsInfo

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"namespace": nsName,
		"slot":      slot,
		"ip":        n.vmIP.String(),
	}).Info("Per-VM network namespace created successfully")

	return nsInfo, nil
}

// ReleaseNamespace removes a VM's network namespace and all associated resources.
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

	// Remove any egress block rules on the host (best-effort)
	exec.Command("iptables", "-D", "FORWARD", "-i", nsInfo.VethHost, "-j", "DROP").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", nsInfo.VethHost, "-j", "DROP").Run()

	// Delete veth host side (automatically removes peer in namespace)
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

// GetTapDevice returns a TapDevice-compatible struct for integration with
// the rest of the runner system. The tap device is always tap-slot-0 with
// IP 172.16.0.2 and gateway 172.16.0.1 (matching snapshot constants).
func (ns *VMNamespace) GetTapDevice(subnet *net.IPNet) *TapDevice {
	return &TapDevice{
		Name:       ns.TapName,
		IP:         ns.IP,
		Gateway:    ns.Gateway,
		Subnet:     subnet,
		MAC:        ns.MAC,
		BridgeName: innerBridgeName,
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

// GetFirecrackerNetNSPath returns the path to the namespace file for Firecracker.
// Firecracker is launched via "ip netns exec {nsName}" to inherit this namespace.
func (ns *VMNamespace) GetFirecrackerNetNSPath() string {
	return ns.Path
}

// ForwardPort sets up DNAT inside the VM's namespace so that traffic arriving
// on the namespace-side veth (10.200.{slot}.2) at the given port is forwarded
// to the guest (172.16.0.2) at the same port. This allows the host to reach
// services running inside the VM via the host-reachable IP.
//
// Example: ForwardPort("vm-id", 8080) makes 10.200.{slot}.2:8080 → 172.16.0.2:8080
func (n *NetNSNetwork) ForwardPort(vmID string, port int) error {
	n.mu.Lock()
	nsInfo, exists := n.namespaces[vmID]
	n.mu.Unlock()

	if !exists {
		return fmt.Errorf("namespace not found for VM: %s", vmID)
	}

	guestIP := n.vmIP.String()
	portStr := fmt.Sprintf("%d", port)
	dest := fmt.Sprintf("%s:%d", guestIP, port)

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"namespace": nsInfo.Name,
		"port":      port,
		"dest":      dest,
	}).Info("Forwarding port into VM namespace")

	// DNAT: traffic arriving on veth-v at this port → guest IP
	if err := exec.Command("ip", "netns", "exec", nsInfo.Name,
		"iptables", "-t", "nat", "-A", "PREROUTING",
		"-i", nsInfo.VethVM, "-p", "tcp", "--dport", portStr,
		"-j", "DNAT", "--to-destination", dest).Run(); err != nil {
		return fmt.Errorf("failed to add DNAT rule for port %d: %w", port, err)
	}

	// FORWARD: allow inbound traffic on this port from veth to bridge
	if err := exec.Command("ip", "netns", "exec", nsInfo.Name,
		"iptables", "-A", "FORWARD",
		"-i", nsInfo.VethVM, "-o", innerBridgeName,
		"-p", "tcp", "--dport", portStr,
		"-j", "ACCEPT").Run(); err != nil {
		return fmt.Errorf("failed to add FORWARD rule for port %d: %w", port, err)
	}

	return nil
}

// ForwardPorts is a convenience method that forwards multiple ports.
func (n *NetNSNetwork) ForwardPorts(vmID string, ports []int) error {
	for _, port := range ports {
		if err := n.ForwardPort(vmID, port); err != nil {
			return err
		}
	}
	return nil
}

// EmergencyBlockEgress blocks all network egress for a VM by inserting DROP rules on
// the host FORWARD chain matching the VM's veth interface. This is the host-level
// emergency kill switch, independent of any namespace-level policy enforcement.
func (n *NetNSNetwork) EmergencyBlockEgress(vmID string) error {
	n.mu.Lock()
	nsInfo, exists := n.namespaces[vmID]
	n.mu.Unlock()

	if !exists {
		return fmt.Errorf("namespace not found for VM: %s", vmID)
	}

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"veth_host": nsInfo.VethHost,
	}).Info("Blocking VM egress via veth interface")

	// Block all forwarding through this VM's veth (both directions)
	if err := exec.Command("iptables", "-I", "FORWARD", "1",
		"-i", nsInfo.VethHost, "-j", "DROP").Run(); err != nil {
		return fmt.Errorf("failed to insert inbound egress drop rule: %w", err)
	}
	if err := exec.Command("iptables", "-I", "FORWARD", "1",
		"-o", nsInfo.VethHost, "-j", "DROP").Run(); err != nil {
		return fmt.Errorf("failed to insert outbound egress drop rule: %w", err)
	}

	return nil
}

// EmergencyUnblockEgress removes the egress block rules for a VM.
func (n *NetNSNetwork) EmergencyUnblockEgress(vmID string) error {
	n.mu.Lock()
	nsInfo, exists := n.namespaces[vmID]
	n.mu.Unlock()

	if !exists {
		return fmt.Errorf("namespace not found for VM: %s", vmID)
	}

	n.logger.WithFields(logrus.Fields{
		"vm_id":     vmID,
		"veth_host": nsInfo.VethHost,
	}).Info("Unblocking VM egress")

	exec.Command("iptables", "-D", "FORWARD", "-i", nsInfo.VethHost, "-j", "DROP").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", nsInfo.VethHost, "-j", "DROP").Run()

	return nil
}

// Cleanup removes all namespaces and host-level NAT rules.
func (n *NetNSNetwork) Cleanup() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.logger.Info("Cleaning up network namespaces")

	// Release all namespaces
	for vmID, nsInfo := range n.namespaces {
		// Remove egress block rules (best-effort)
		exec.Command("iptables", "-D", "FORWARD", "-i", nsInfo.VethHost, "-j", "DROP").Run()
		exec.Command("iptables", "-D", "FORWARD", "-o", nsInfo.VethHost, "-j", "DROP").Run()

		if link, err := netlink.LinkByName(nsInfo.VethHost); err == nil {
			netlink.LinkDel(link)
		}
		nsInfo.Handle.Close()
		netns.DeleteNamed(nsInfo.Name)
		delete(n.namespaces, vmID)
	}

	// Remove host-level NAT rules
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", vethSupernet, "-o", n.externalIface, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-D", "FORWARD",
		"-s", vethSupernet, "-o", n.externalIface, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD",
		"-d", vethSupernet, "-i", n.externalIface,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
	exec.Command("iptables", "-t", "mangle", "-D", "FORWARD",
		"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu").Run()

	return nil
}

// GetVMIP returns the IP address used by all VMs (172.16.0.2)
func (n *NetNSNetwork) GetVMIP() net.IP {
	return n.vmIP
}

// GetSubnet returns the guest subnet used by the network (172.16.0.0/24)
func (n *NetNSNetwork) GetSubnet() *net.IPNet {
	return n.subnet
}

// EnsureNetNSDir ensures /var/run/netns exists
func EnsureNetNSDir() error {
	return os.MkdirAll("/var/run/netns", 0755)
}
