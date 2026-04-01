package network

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// PolicyAction represents the default action for network policy.
type PolicyAction string

const (
	PolicyActionAllow PolicyAction = "allow"
	PolicyActionDeny  PolicyAction = "deny"
)

// NetworkPolicy defines the network access policy for a VM.
type NetworkPolicy struct {
	Name                     string               `json:"name,omitempty"`
	DefaultEgressAction      PolicyAction         `json:"default_egress_action"`
	AllowedEgress            []EgressRule         `json:"allowed_egress,omitempty"`
	DeniedEgress             []EgressRule         `json:"denied_egress,omitempty"`
	DNS                      DNSPolicy            `json:"dns,omitempty"`
	RateLimit                *RateLimitPolicy     `json:"rate_limit,omitempty"`
	InternalAccess           InternalAccessPolicy `json:"internal_access,omitempty"`
	Ingress                  IngressPolicy        `json:"ingress,omitempty"`
	AllowDynamicUpdates      bool                 `json:"allow_dynamic_updates,omitempty"`
	DynamicUpdateConstraints *DynamicConstraints  `json:"dynamic_update_constraints,omitempty"`
	MaxIPSetEntries          int                  `json:"max_ipset_entries,omitempty"`
	MaxIPsPerDomain          int                  `json:"max_ips_per_domain,omitempty"`
}

// EgressRule defines a single egress allow or deny rule.
type EgressRule struct {
	Description string      `json:"description,omitempty"`
	Domains     []string    `json:"domains,omitempty"`
	CIDRs       []string    `json:"cidrs,omitempty"`
	Ports       []PortRange `json:"ports,omitempty"`
	Protocols   []string    `json:"protocols,omitempty"`
}

// PortRange defines a range of ports.
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end,omitempty"`
}

// DNSPolicy controls DNS behaviour within the VM.
type DNSPolicy struct {
	Servers        []string `json:"servers,omitempty"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
	UsePolicyProxy bool     `json:"use_policy_proxy,omitempty"`
}

// RateLimitPolicy defines rate limiting parameters.
type RateLimitPolicy struct {
	EgressBandwidthMbps      int `json:"egress_bandwidth_mbps,omitempty"`
	IngressBandwidthMbps     int `json:"ingress_bandwidth_mbps,omitempty"`
	MaxNewConnectionsPerSec  int `json:"max_new_connections_per_sec,omitempty"`
	MaxConcurrentConnections int `json:"max_concurrent_connections,omitempty"`
}

// InternalAccessPolicy controls access to internal/platform networks.
type InternalAccessPolicy struct {
	AllowMetadata        *bool    `json:"allow_metadata,omitempty"`
	AllowHostAccess      bool     `json:"allow_host_access,omitempty"`
	AllowOtherVMs        bool     `json:"allow_other_vms,omitempty"`
	AllowedInternalCIDRs []string `json:"allowed_internal_cidrs,omitempty"`
}

// IngressPolicy controls inbound access to the VM.
type IngressPolicy struct {
	AllowedPorts       []PortRange `json:"allowed_ports,omitempty"`
	AllowedSourceCIDRs []string    `json:"allowed_source_cidrs,omitempty"`
}

// DynamicConstraints limits what runtime policy updates can do.
type DynamicConstraints struct {
	MaxAdditionalDomains int      `json:"max_additional_domains,omitempty"`
	DomainAllowlist      []string `json:"domain_allowlist,omitempty"`
	MaxAdditionalCIDRs   int      `json:"max_additional_cidrs,omitempty"`
	MaxDynamicCIDRWidth  int      `json:"max_dynamic_cidr_width,omitempty"`
	CanRelaxRateLimit    bool     `json:"can_relax_rate_limit,omitempty"`
}

// MetadataAllowed returns whether metadata access is allowed (defaults to true).
func (p *InternalAccessPolicy) MetadataAllowed() bool {
	if p.AllowMetadata == nil {
		return true
	}
	return *p.AllowMetadata
}

// EffectiveMaxIPSetEntries returns MaxIPSetEntries with a default of 10000.
func (p *NetworkPolicy) EffectiveMaxIPSetEntries() int {
	if p.MaxIPSetEntries <= 0 {
		return 10000
	}
	return p.MaxIPSetEntries
}

// EffectiveMaxIPsPerDomain returns MaxIPsPerDomain with a default of 64.
func (p *NetworkPolicy) EffectiveMaxIPsPerDomain() int {
	if p.MaxIPsPerDomain <= 0 {
		return 64
	}
	return p.MaxIPsPerDomain
}

// Validate checks the policy for internal consistency and returns an error if invalid.
func (p *NetworkPolicy) Validate() error {
	if p.DefaultEgressAction == "" {
		p.DefaultEgressAction = PolicyActionAllow
	}
	if p.DefaultEgressAction != PolicyActionAllow && p.DefaultEgressAction != PolicyActionDeny {
		return fmt.Errorf("invalid default_egress_action: %q (must be \"allow\" or \"deny\")", p.DefaultEgressAction)
	}

	isAllow := p.DefaultEgressAction == PolicyActionAllow

	// Domain rules are only supported in deny-default mode
	if isAllow {
		for i, rule := range p.AllowedEgress {
			if len(rule.Domains) > 0 {
				return fmt.Errorf("allowed_egress[%d]: domain rules are not supported when default_egress_action is \"allow\"", i)
			}
		}
		for i, rule := range p.DeniedEgress {
			if len(rule.Domains) > 0 {
				return fmt.Errorf("denied_egress[%d]: domain rules are not supported when default_egress_action is \"allow\"", i)
			}
		}
		if len(p.DNS.AllowedDomains) > 0 {
			return fmt.Errorf("dns.allowed_domains: not supported when default_egress_action is \"allow\"")
		}
		if len(p.DNS.BlockedDomains) > 0 {
			return fmt.Errorf("dns.blocked_domains: not supported when default_egress_action is \"allow\"")
		}
	}

	// Validate CIDRs in egress rules
	for i, rule := range p.AllowedEgress {
		for j, cidr := range rule.CIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("allowed_egress[%d].cidrs[%d]: invalid CIDR %q: %w", i, j, cidr, err)
			}
		}
		if err := validatePorts(rule.Ports); err != nil {
			return fmt.Errorf("allowed_egress[%d].ports: %w", i, err)
		}
	}
	for i, rule := range p.DeniedEgress {
		for j, cidr := range rule.CIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("denied_egress[%d].cidrs[%d]: invalid CIDR %q: %w", i, j, cidr, err)
			}
		}
		if err := validatePorts(rule.Ports); err != nil {
			return fmt.Errorf("denied_egress[%d].ports: %w", i, err)
		}
	}

	// AllowedInternalCIDRs: each must be /16 or narrower
	for i, cidr := range p.InternalAccess.AllowedInternalCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("internal_access.allowed_internal_cidrs[%d]: invalid CIDR %q: %w", i, cidr, err)
		}
		ones, _ := ipNet.Mask.Size()
		if ones < 16 {
			return fmt.Errorf("internal_access.allowed_internal_cidrs[%d]: CIDR %q is broader than /16 (prefix length %d)", i, cidr, ones)
		}
	}

	// Validate ingress CIDRs
	for i, cidr := range p.Ingress.AllowedSourceCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("ingress.allowed_source_cidrs[%d]: invalid CIDR %q: %w", i, cidr, err)
		}
	}
	if err := validatePorts(p.Ingress.AllowedPorts); err != nil {
		return fmt.Errorf("ingress.allowed_ports: %w", err)
	}

	// MaxIPSetEntries cap
	if p.MaxIPSetEntries > 100000 {
		return fmt.Errorf("max_ipset_entries: %d exceeds maximum of 100000", p.MaxIPSetEntries)
	}

	// MaxIPsPerDomain cap
	if p.MaxIPsPerDomain > 256 {
		return fmt.Errorf("max_ips_per_domain: %d exceeds maximum of 256", p.MaxIPsPerDomain)
	}

	return nil
}

func validatePorts(ports []PortRange) error {
	for i, p := range ports {
		if p.Start < 1 || p.Start > 65535 {
			return fmt.Errorf("[%d]: invalid start port %d", i, p.Start)
		}
		end := p.End
		if end == 0 {
			end = p.Start
		}
		if end < p.Start || end > 65535 {
			return fmt.Errorf("[%d]: invalid end port %d", i, end)
		}
	}
	return nil
}

// Named preset names.
const (
	PresetUnrestricted     = "unrestricted"
	PresetQuarantine       = "quarantine"
	PresetRestrictedEgress = "restricted-egress"
	PresetAgentSandbox     = "agent-sandbox"
)

// GetPreset returns a named preset policy. Returns nil for unknown names.
func GetPreset(name string) *NetworkPolicy {
	switch strings.ToLower(name) {
	case PresetUnrestricted:
		return presetUnrestricted()
	case PresetQuarantine:
		return presetQuarantine()
	case PresetRestrictedEgress:
		return presetRestrictedEgress()
	case PresetAgentSandbox:
		return presetAgentSandbox()
	default:
		return nil
	}
}

func presetUnrestricted() *NetworkPolicy {
	return &NetworkPolicy{
		Name:                "unrestricted",
		DefaultEgressAction: PolicyActionAllow,
	}
}

func presetQuarantine() *NetworkPolicy {
	f := false
	return &NetworkPolicy{
		Name:                "quarantine",
		DefaultEgressAction: PolicyActionDeny,
		InternalAccess: InternalAccessPolicy{
			AllowMetadata:   &f,
			AllowHostAccess: false,
			AllowOtherVMs:   false,
		},
	}
}

func presetRestrictedEgress() *NetworkPolicy {
	return &NetworkPolicy{
		Name:                "restricted-egress",
		DefaultEgressAction: PolicyActionAllow,
		DeniedEgress: []EgressRule{
			{
				Description: "Block RFC1918 (except MMDS handled separately)",
				CIDRs:       []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
			},
		},
	}
}

func presetAgentSandbox() *NetworkPolicy {
	return &NetworkPolicy{
		Name:                "agent-sandbox",
		DefaultEgressAction: PolicyActionDeny,
		DNS: DNSPolicy{
			UsePolicyProxy: true,
		},
		AllowDynamicUpdates: true,
		DynamicUpdateConstraints: &DynamicConstraints{
			MaxAdditionalDomains: 20,
			MaxAdditionalCIDRs:   5,
			MaxDynamicCIDRWidth:  24,
		},
	}
}

// ResolvePolicy resolves a network policy from a preset name and/or explicit policy.
// Request policy overrides workload default. Nil/empty = unrestricted.
func ResolvePolicy(presetName string, explicit *NetworkPolicy) *NetworkPolicy {
	if explicit != nil {
		return explicit
	}
	if presetName != "" {
		if p := GetPreset(presetName); p != nil {
			return p
		}
	}
	return nil // nil means unrestricted (backwards-compatible)
}

// Chain names used by PolicyEnforcer iptables rules.
const (
	policyEgressChain  = "POLICY-EGRESS"
	policyIngressChain = "POLICY-INGRESS"
)

// vethSupernetConst is used by BuildEgressChainRules for the AllowOtherVMs rule.
// Matches vethSupernet in netns.go.
const vethSupernetConst = "10.200.0.0/16"

// BuildEgressChainRules returns the iptables rules that would be added to POLICY-EGRESS
// for the given policy. Pure computation, cross-platform. Used for testing.
func BuildEgressChainRules(policy *NetworkPolicy, id8, vethVM string, hostVethIP net.IP) [][]string {
	var rules [][]string

	// 1. Conntrack
	rules = append(rules, []string{"-A", policyEgressChain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"})

	// 2. MMDS
	if policy.InternalAccess.MetadataAllowed() {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-d", "169.254.169.254", "-j", "ACCEPT"})
	}

	// 3. Host access
	if policy.InternalAccess.AllowHostAccess && hostVethIP != nil {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-d", hostVethIP.String() + "/32", "-j", "ACCEPT"})
	}

	// 4. Allowed internal CIDRs
	for _, cidr := range policy.InternalAccess.AllowedInternalCIDRs {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-d", cidr, "-j", "ACCEPT"})
	}

	// 4b. AllowOtherVMs
	if policy.InternalAccess.AllowOtherVMs {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-d", vethSupernetConst, "-j", "ACCEPT"})
	}

	// 5-7. RFC1918
	rules = append(rules, []string{"-A", policyEgressChain, "-d", "10.0.0.0/8", "-j", "DROP"})
	rules = append(rules, []string{"-A", policyEgressChain, "-d", "172.16.0.0/12", "-j", "DROP"})
	rules = append(rules, []string{"-A", policyEgressChain, "-d", "192.168.0.0/16", "-j", "DROP"})

	// 8. CIDR ipset
	caSet := fmt.Sprintf("POL-%s-CA", id8)
	cdSet := fmt.Sprintf("POL-%s-CD", id8)
	daSet := fmt.Sprintf("POL-%s-DA", id8)

	if policy.DefaultEgressAction == PolicyActionDeny {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-m", "set", "--match-set", caSet, "dst", "-j", "ACCEPT"})
	} else {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-m", "set", "--match-set", cdSet, "dst", "-j", "DROP"})
	}

	// 9. Domain allow set (deny-default only)
	if policy.DefaultEgressAction == PolicyActionDeny {
		rules = append(rules, []string{"-A", policyEgressChain,
			"-m", "set", "--match-set", daSet, "dst", "-j", "ACCEPT"})
	}

	// 10. Default action
	defaultTarget := "ACCEPT"
	if policy.DefaultEgressAction == PolicyActionDeny {
		defaultTarget = "DROP"
	}
	rules = append(rules, []string{"-A", policyEgressChain, "-j", defaultTarget})

	return rules
}

// Clone returns a deep copy of the policy.
func (p *NetworkPolicy) Clone() *NetworkPolicy {
	if p == nil {
		return nil
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	var clone NetworkPolicy
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil
	}
	return &clone
}

// WithAccessPlaneAccess returns a clone of the policy with egress rules added
// to allow traffic to the access plane's HTTP API and CONNECT proxy ports.
// Used when a runner is configured to use an external access plane instead of
// a host-local auth proxy.
func (p *NetworkPolicy) WithAccessPlaneAccess(accessPlaneIP string) *NetworkPolicy {
	clone := p.Clone()
	if clone == nil {
		clone = &NetworkPolicy{DefaultEgressAction: PolicyActionDeny}
	}
	clone.AllowedEgress = append(clone.AllowedEgress, EgressRule{
		Description: "Allow access to project access plane",
		CIDRs:       []string{accessPlaneIP + "/32"},
		Ports:       []PortRange{{Start: 8080, End: 8080}, {Start: 3128, End: 3128}},
	})
	return clone
}
