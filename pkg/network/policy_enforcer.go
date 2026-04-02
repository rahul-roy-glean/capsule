//go:build linux
// +build linux

package network

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// PolicyEnforcer manages iptables/ipset rules for a single VM's network policy
// inside its network namespace.
type PolicyEnforcer struct {
	vmID       string // full VM ID
	id8        string // first 8 chars of vmID (for ipset naming)
	nsName     string // network namespace name
	vethVM     string // namespace-side veth interface name
	hostVethIP net.IP // host-side veth IP (10.200.{slot}.1)
	policy     *NetworkPolicy
	mu         sync.Mutex
	logger     *logrus.Entry

	// ingressPorts tracks all forwarded ports for POLICY-INGRESS chain
	ingressPorts []int

	// dnsProxy is the DNS proxy goroutine (nil if not needed)
	dnsProxy *DNSProxy
}

// PolicyEnforcerConfig holds configuration for creating a PolicyEnforcer.
type PolicyEnforcerConfig struct {
	VMID       string
	NSName     string
	VethVM     string
	HostVethIP net.IP
	Policy     *NetworkPolicy
	Logger     *logrus.Logger
}

// NewPolicyEnforcer creates a new PolicyEnforcer for a VM.
func NewPolicyEnforcer(cfg PolicyEnforcerConfig) *PolicyEnforcer {
	id8 := cfg.VMID
	if len(id8) > 8 {
		id8 = id8[:8]
	}
	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}
	return &PolicyEnforcer{
		vmID:       cfg.VMID,
		id8:        id8,
		nsName:     cfg.NSName,
		vethVM:     cfg.VethVM,
		hostVethIP: cfg.HostVethIP,
		policy:     cfg.Policy,
		logger: logger.WithFields(logrus.Fields{
			"component": "policy-enforcer",
			"vm_id":     cfg.VMID,
		}),
	}
}

// ipset names (must stay within 31-char limit)
func (e *PolicyEnforcer) cidrAllowSet() string { return fmt.Sprintf("POL-%s-CA", e.id8) }
func (e *PolicyEnforcer) cidrDenySet() string  { return fmt.Sprintf("POL-%s-CD", e.id8) }
func (e *PolicyEnforcer) domAllowSet() string  { return fmt.Sprintf("POL-%s-DA", e.id8) }

// chain names are defined in policy.go (cross-platform)

// Apply installs the full policy inside the VM's namespace.
// Called once at allocation time before the VM has traffic.
func (e *PolicyEnforcer) Apply() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.policy == nil {
		return nil // nil policy = unrestricted, no rules needed
	}

	e.logger.WithField("policy", e.policy.Name).Info("Applying network policy")

	// Clean slate
	e.removeUnlocked()

	// Create ipsets
	if err := e.createIPSets(); err != nil {
		return fmt.Errorf("create ipsets: %w", err)
	}

	// Create and populate chains
	if err := e.createChains(); err != nil {
		return fmt.Errorf("create chains: %w", err)
	}

	// Install DNS interception if needed
	if e.policy.DefaultEgressAction == PolicyActionDeny && e.policy.DNS.UsePolicyProxy {
		if err := e.installDNSRedirect(); err != nil {
			return fmt.Errorf("install dns redirect: %w", err)
		}
	}

	// Hook chains into FORWARD
	if err := e.hookForwardChain(); err != nil {
		return fmt.Errorf("hook forward chain: %w", err)
	}

	return nil
}

// Update atomically updates the policy. Used for quarantine and dynamic updates.
func (e *PolicyEnforcer) Update(newPolicy *NetworkPolicy) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if newPolicy == nil {
		e.removeUnlocked()
		e.policy = nil
		return nil
	}

	oldPolicy := e.policy
	e.policy = newPolicy

	// Swap CIDR ipsets atomically
	if err := e.swapCIDRSets(); err != nil {
		e.policy = oldPolicy
		return fmt.Errorf("swap cidr sets: %w", err)
	}

	// Rebuild chains via iptables-restore for atomic structural update
	if err := e.rebuildChains(); err != nil {
		return fmt.Errorf("rebuild chains: %w", err)
	}

	// Update DNS proxy domain lists if running
	if e.dnsProxy != nil {
		e.dnsProxy.UpdateDomains(newPolicy.DNS.AllowedDomains, newPolicy.DNS.BlockedDomains)
	}

	return nil
}

// Remove tears down all policy rules and ipsets. Idempotent.
func (e *PolicyEnforcer) Remove() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removeUnlocked()
}

func (e *PolicyEnforcer) removeUnlocked() {
	// Stop DNS proxy
	if e.dnsProxy != nil {
		e.dnsProxy.Stop()
		e.dnsProxy = nil
	}

	// Unhook from FORWARD
	e.nsExec("iptables", "-D", "FORWARD", "-i", innerBridgeName, "-o", e.vethVM, "-j", policyEgressChain)
	e.nsExec("iptables", "-D", "FORWARD", "-i", e.vethVM, "-o", innerBridgeName, "-j", policyIngressChain)

	// Flush and delete chains
	e.nsExec("iptables", "-F", policyEgressChain)
	e.nsExec("iptables", "-X", policyEgressChain)
	e.nsExec("iptables", "-F", policyIngressChain)
	e.nsExec("iptables", "-X", policyIngressChain)

	// Remove DNS redirect rules
	e.nsExec("iptables", "-t", "nat", "-D", "PREROUTING",
		"-i", innerBridgeName, "-s", "172.16.0.0/24", "-p", "udp", "--dport", "53",
		"-j", "REDIRECT", "--to-ports", "5353")
	e.nsExec("iptables", "-t", "nat", "-D", "PREROUTING",
		"-i", innerBridgeName, "-s", "172.16.0.0/24", "-p", "tcp", "--dport", "53",
		"-j", "REDIRECT", "--to-ports", "5353")

	// Destroy ipsets
	e.nsExec("ipset", "destroy", e.cidrAllowSet())
	e.nsExec("ipset", "destroy", e.cidrDenySet())
	e.nsExec("ipset", "destroy", e.domAllowSet())
}

// AddIngressPort adds a port to the ingress allow list.
func (e *PolicyEnforcer) AddIngressPort(port int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ingressPorts = append(e.ingressPorts, port)
}

// SetInitialIngressPorts sets the initial set of ingress ports before Apply.
func (e *PolicyEnforcer) SetInitialIngressPorts(ports []int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ingressPorts = append([]int{}, ports...)
}

// createIPSets creates the ipset data structures inside the namespace.
func (e *PolicyEnforcer) createIPSets() error {
	maxElem := fmt.Sprintf("%d", e.policy.EffectiveMaxIPSetEntries())

	// CIDR allow set (hash:net)
	if err := e.nsExecErr("ipset", "create", e.cidrAllowSet(), "hash:net", "maxelem", maxElem); err != nil {
		return fmt.Errorf("create cidr allow set: %w", err)
	}

	// CIDR deny set (hash:net)
	if err := e.nsExecErr("ipset", "create", e.cidrDenySet(), "hash:net", "maxelem", maxElem); err != nil {
		return fmt.Errorf("create cidr deny set: %w", err)
	}

	// Domain IP allow set (hash:ip with timeout for TTL-based expiry)
	if err := e.nsExecErr("ipset", "create", e.domAllowSet(), "hash:ip", "timeout", "300", "maxelem", maxElem); err != nil {
		return fmt.Errorf("create dom allow set: %w", err)
	}

	// Populate CIDR sets
	if e.policy.DefaultEgressAction == PolicyActionDeny {
		for _, rule := range e.policy.AllowedEgress {
			for _, cidr := range rule.CIDRs {
				e.nsExec("ipset", "add", e.cidrAllowSet(), cidr)
			}
		}
	} else {
		for _, rule := range e.policy.DeniedEgress {
			for _, cidr := range rule.CIDRs {
				e.nsExec("ipset", "add", e.cidrDenySet(), cidr)
			}
		}
	}

	return nil
}

// createChains creates POLICY-EGRESS and POLICY-INGRESS chains.
func (e *PolicyEnforcer) createChains() error {
	// Create chains
	if err := e.nsExecErr("iptables", "-N", policyEgressChain); err != nil {
		return fmt.Errorf("create egress chain: %w", err)
	}
	if err := e.nsExecErr("iptables", "-N", policyIngressChain); err != nil {
		return fmt.Errorf("create ingress chain: %w", err)
	}

	// Populate egress chain
	if err := e.populateEgressChain(); err != nil {
		return err
	}

	// Populate ingress chain
	return e.populateIngressChain()
}

// populateEgressChain adds rules to the POLICY-EGRESS chain.
func (e *PolicyEnforcer) populateEgressChain() error {
	p := e.policy

	// 1. Conntrack: allow established/related
	e.nsExecErr("iptables", "-A", policyEgressChain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")

	// 2. MMDS (169.254.169.254) if AllowMetadata
	if p.InternalAccess.MetadataAllowed() {
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-d", "169.254.169.254", "-j", "ACCEPT")
	}

	// 3. Host veth access if AllowHostAccess
	if p.InternalAccess.AllowHostAccess && e.hostVethIP != nil {
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-d", e.hostVethIP.String()+"/32", "-j", "ACCEPT")
	}

	// 4. AllowedInternalCIDRs (ACCEPT before RFC1918 DROP)
	for _, cidr := range p.InternalAccess.AllowedInternalCIDRs {
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-d", cidr, "-j", "ACCEPT")
	}

	// 4b. AllowOtherVMs (entire VM supernet)
	if p.InternalAccess.AllowOtherVMs {
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-d", vethSupernet, "-j", "ACCEPT")
	}

	// 5. Allow explicitly-listed RFC1918 destinations before the blanket
	// RFC1918 DROP. This lets policies whitelist internal services like the
	// access plane (e.g. 10.0.16.7) without opening all of 10.0.0.0/8.
	if p.DefaultEgressAction == PolicyActionDeny {
		for _, rule := range p.AllowedEgress {
			for _, cidr := range rule.CIDRs {
				if isRFC1918(cidr) {
					e.nsExecErr("iptables", "-A", policyEgressChain,
						"-d", cidr, "-j", "ACCEPT")
				}
			}
		}
	}

	// 6-8. RFC1918 DROP
	e.nsExecErr("iptables", "-A", policyEgressChain, "-d", "10.0.0.0/8", "-j", "DROP")
	e.nsExecErr("iptables", "-A", policyEgressChain, "-d", "172.16.0.0/12", "-j", "DROP")
	e.nsExecErr("iptables", "-A", policyEgressChain, "-d", "192.168.0.0/16", "-j", "DROP")

	// 9. CIDR ipset rules (non-RFC1918 destinations)
	if p.DefaultEgressAction == PolicyActionDeny {
		// deny-default: match CIDR allow set → ACCEPT
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-m", "set", "--match-set", e.cidrAllowSet(), "dst", "-j", "ACCEPT")
	} else {
		// allow-default: match CIDR deny set → DROP
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-m", "set", "--match-set", e.cidrDenySet(), "dst", "-j", "DROP")
	}

	// 10. Domain IP allow set (deny-default only)
	if p.DefaultEgressAction == PolicyActionDeny {
		e.nsExecErr("iptables", "-A", policyEgressChain,
			"-m", "set", "--match-set", e.domAllowSet(), "dst", "-j", "ACCEPT")
	}

	// 11. Default action
	defaultTarget := "ACCEPT"
	if p.DefaultEgressAction == PolicyActionDeny {
		defaultTarget = "DROP"
	}
	e.nsExecErr("iptables", "-A", policyEgressChain, "-j", defaultTarget)

	return nil
}

// populateIngressChain adds rules to the POLICY-INGRESS chain.
func (e *PolicyEnforcer) populateIngressChain() error {
	// 1. Allow established/related
	e.nsExecErr("iptables", "-A", policyIngressChain,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	// 2. Per forwarded port
	for _, port := range e.ingressPorts {
		portStr := fmt.Sprintf("%d", port)
		if len(e.policy.Ingress.AllowedSourceCIDRs) > 0 {
			for _, cidr := range e.policy.Ingress.AllowedSourceCIDRs {
				e.nsExecErr("iptables", "-A", policyIngressChain,
					"-p", "tcp", "--dport", portStr, "-s", cidr, "-j", "ACCEPT")
			}
		} else {
			e.nsExecErr("iptables", "-A", policyIngressChain,
				"-p", "tcp", "--dport", portStr, "-j", "ACCEPT")
		}
	}

	// Also allow ports from the policy itself
	for _, pr := range e.policy.Ingress.AllowedPorts {
		end := pr.End
		if end == 0 {
			end = pr.Start
		}
		for port := pr.Start; port <= end; port++ {
			portStr := fmt.Sprintf("%d", port)
			if len(e.policy.Ingress.AllowedSourceCIDRs) > 0 {
				for _, cidr := range e.policy.Ingress.AllowedSourceCIDRs {
					e.nsExecErr("iptables", "-A", policyIngressChain,
						"-p", "tcp", "--dport", portStr, "-s", cidr, "-j", "ACCEPT")
				}
			} else {
				e.nsExecErr("iptables", "-A", policyIngressChain,
					"-p", "tcp", "--dport", portStr, "-j", "ACCEPT")
			}
		}
	}

	// 3. Default drop
	e.nsExecErr("iptables", "-A", policyIngressChain, "-j", "DROP")

	return nil
}

// hookForwardChain inserts the policy chains into the namespace's FORWARD chain.
// This replaces the default br-vm → veth ACCEPT rules with policy chains.
func (e *PolicyEnforcer) hookForwardChain() error {
	// Remove the default permissive FORWARD rules that CreateNamespaceForVM installed
	e.nsExec("iptables", "-D", "FORWARD", "-i", innerBridgeName, "-o", e.vethVM, "-j", "ACCEPT")
	e.nsExec("iptables", "-D", "FORWARD", "-i", e.vethVM, "-o", innerBridgeName,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	// Insert policy chains
	if err := e.nsExecErr("iptables", "-I", "FORWARD", "1",
		"-i", innerBridgeName, "-o", e.vethVM, "-j", policyEgressChain); err != nil {
		return fmt.Errorf("hook egress chain: %w", err)
	}
	if err := e.nsExecErr("iptables", "-I", "FORWARD", "2",
		"-i", e.vethVM, "-o", innerBridgeName, "-j", policyIngressChain); err != nil {
		return fmt.Errorf("hook ingress chain: %w", err)
	}

	return nil
}

// installDNSRedirect adds nat PREROUTING REDIRECT rules for port 53 → 5353.
func (e *PolicyEnforcer) installDNSRedirect() error {
	// UDP DNS redirect
	if err := e.nsExecErr("iptables", "-t", "nat", "-A", "PREROUTING",
		"-i", innerBridgeName, "-s", "172.16.0.0/24", "-p", "udp", "--dport", "53",
		"-j", "REDIRECT", "--to-ports", "5353"); err != nil {
		return fmt.Errorf("udp dns redirect: %w", err)
	}

	// TCP DNS redirect
	if err := e.nsExecErr("iptables", "-t", "nat", "-A", "PREROUTING",
		"-i", innerBridgeName, "-s", "172.16.0.0/24", "-p", "tcp", "--dport", "53",
		"-j", "REDIRECT", "--to-ports", "5353"); err != nil {
		return fmt.Errorf("tcp dns redirect: %w", err)
	}

	return nil
}

// swapCIDRSets atomically swaps CIDR ipsets with new policy content.
func (e *PolicyEnforcer) swapCIDRSets() error {
	p := e.policy
	maxElem := fmt.Sprintf("%d", p.EffectiveMaxIPSetEntries())

	// Create temporary sets
	tmpCA := e.cidrAllowSet() + "-tmp"
	tmpCD := e.cidrDenySet() + "-tmp"

	e.nsExec("ipset", "destroy", tmpCA)
	e.nsExec("ipset", "destroy", tmpCD)

	if err := e.nsExecErr("ipset", "create", tmpCA, "hash:net", "maxelem", maxElem); err != nil {
		return err
	}
	if err := e.nsExecErr("ipset", "create", tmpCD, "hash:net", "maxelem", maxElem); err != nil {
		e.nsExec("ipset", "destroy", tmpCA)
		return err
	}

	// Populate temp sets
	if p.DefaultEgressAction == PolicyActionDeny {
		for _, rule := range p.AllowedEgress {
			for _, cidr := range rule.CIDRs {
				e.nsExec("ipset", "add", tmpCA, cidr)
			}
		}
	} else {
		for _, rule := range p.DeniedEgress {
			for _, cidr := range rule.CIDRs {
				e.nsExec("ipset", "add", tmpCD, cidr)
			}
		}
	}

	// Atomic swap
	e.nsExecErr("ipset", "swap", tmpCA, e.cidrAllowSet())
	e.nsExecErr("ipset", "swap", tmpCD, e.cidrDenySet())

	// Cleanup temps (now contain old data)
	e.nsExec("ipset", "destroy", tmpCA)
	e.nsExec("ipset", "destroy", tmpCD)

	return nil
}

// rebuildChains flushes and repopulates policy chains.
func (e *PolicyEnforcer) rebuildChains() error {
	e.nsExec("iptables", "-F", policyEgressChain)
	e.nsExec("iptables", "-F", policyIngressChain)
	if err := e.populateEgressChain(); err != nil {
		return err
	}
	return e.populateIngressChain()
}

// StartDNSProxy starts the DNS proxy for this enforcer. Must be called after Apply
// and within the VM's network namespace context.
func (e *PolicyEnforcer) StartDNSProxy(runInNS func(func() error) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.policy == nil || !e.policy.DNS.UsePolicyProxy || e.policy.DefaultEgressAction != PolicyActionDeny {
		return nil
	}

	proxy := NewDNSProxy(DNSProxyConfig{
		ListenAddr:      "0.0.0.0:5353",
		UpstreamServers: e.policy.DNS.Servers,
		AllowedDomains:  e.policy.DNS.AllowedDomains,
		BlockedDomains:  e.policy.DNS.BlockedDomains,
		DomIPSetName:    e.domAllowSet(),
		NSName:          e.nsName,
		MaxIPsPerDomain: e.policy.EffectiveMaxIPsPerDomain(),
		MaxIPSetEntries: e.policy.EffectiveMaxIPSetEntries(),
		VMID:            e.vmID,
		Logger:          e.logger.Logger,
	})

	if err := proxy.Start(runInNS); err != nil {
		return fmt.Errorf("start dns proxy: %w", err)
	}

	e.dnsProxy = proxy
	return nil
}

// GetPolicy returns the current effective policy.
func (e *PolicyEnforcer) GetPolicy() *NetworkPolicy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.policy
}

// BuildEgressChainRules is defined in policy.go (cross-platform).

// nsExec runs a command in the VM's network namespace (best-effort, ignores errors).
func (e *PolicyEnforcer) nsExec(name string, args ...string) {
	fullArgs := append([]string{"netns", "exec", e.nsName, name}, args...)
	exec.Command("ip", fullArgs...).Run()
}

// nsExecErr runs a command in the VM's network namespace and returns any error.
func (e *PolicyEnforcer) nsExecErr(name string, args ...string) error {
	fullArgs := append([]string{"netns", "exec", e.nsName, name}, args...)
	out, err := exec.Command("ip", fullArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)", name, strings.Join(args, " "), err, string(out))
	}
	return nil
}

// rfc1918Nets are the private IPv4 address ranges (RFC 1918).
var rfc1918Nets = []*net.IPNet{
	parseCIDR("10.0.0.0/8"),
	parseCIDR("172.16.0.0/12"),
	parseCIDR("192.168.0.0/16"),
}

func parseCIDR(s string) *net.IPNet {
	_, n, _ := net.ParseCIDR(s)
	return n
}

// isRFC1918 returns true if the given CIDR string overlaps with any RFC1918 range.
func isRFC1918(cidr string) bool {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		// Might be a bare IP
		ip = net.ParseIP(cidr)
		if ip == nil {
			return false
		}
		for _, rfc := range rfc1918Nets {
			if rfc.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, rfc := range rfc1918Nets {
		if rfc.Contains(ip) || rfc.Contains(lastIP(ipNet)) {
			return true
		}
	}
	return false
}

// lastIP returns the last IP in a subnet.
func lastIP(n *net.IPNet) net.IP {
	ip := make(net.IP, len(n.IP))
	for i := range ip {
		ip[i] = n.IP[i] | ^n.Mask[i]
	}
	return ip
}
