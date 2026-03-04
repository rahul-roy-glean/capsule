package network

import (
	"encoding/json"
	"testing"
)

func TestValidate_DefaultEgressAction(t *testing.T) {
	// Empty defaults to allow
	p := &NetworkPolicy{}
	if err := p.Validate(); err != nil {
		t.Fatalf("empty policy should be valid: %v", err)
	}
	if p.DefaultEgressAction != PolicyActionAllow {
		t.Fatalf("expected default action 'allow', got %q", p.DefaultEgressAction)
	}

	// Invalid action
	p = &NetworkPolicy{DefaultEgressAction: "maybe"}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for invalid default_egress_action")
	}
}

func TestValidate_DomainRulesRejectedInAllowDefault(t *testing.T) {
	tests := []struct {
		name   string
		policy NetworkPolicy
	}{
		{
			name: "allowed_egress domains",
			policy: NetworkPolicy{
				DefaultEgressAction: PolicyActionAllow,
				AllowedEgress: []EgressRule{
					{Domains: []string{"example.com"}},
				},
			},
		},
		{
			name: "denied_egress domains",
			policy: NetworkPolicy{
				DefaultEgressAction: PolicyActionAllow,
				DeniedEgress: []EgressRule{
					{Domains: []string{"evil.com"}},
				},
			},
		},
		{
			name: "dns allowed_domains",
			policy: NetworkPolicy{
				DefaultEgressAction: PolicyActionAllow,
				DNS:                 DNSPolicy{AllowedDomains: []string{"ok.com"}},
			},
		},
		{
			name: "dns blocked_domains",
			policy: NetworkPolicy{
				DefaultEgressAction: PolicyActionAllow,
				DNS:                 DNSPolicy{BlockedDomains: []string{"bad.com"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.policy.Validate(); err == nil {
				t.Fatal("expected validation error for domain rules in allow-default")
			}
		})
	}
}

func TestValidate_DomainRulesAcceptedInDenyDefault(t *testing.T) {
	p := &NetworkPolicy{
		DefaultEgressAction: PolicyActionDeny,
		AllowedEgress: []EgressRule{
			{Domains: []string{"github.com"}},
		},
		DNS: DNSPolicy{
			AllowedDomains: []string{"github.com"},
			BlockedDomains: []string{"evil.com"},
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("domain rules should be valid in deny-default: %v", err)
	}
}

func TestValidate_InvalidCIDR(t *testing.T) {
	p := &NetworkPolicy{
		DefaultEgressAction: PolicyActionDeny,
		AllowedEgress: []EgressRule{
			{CIDRs: []string{"not-a-cidr"}},
		},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

func TestValidate_InternalCIDRBroaderThan16(t *testing.T) {
	p := &NetworkPolicy{
		DefaultEgressAction: PolicyActionAllow,
		InternalAccess: InternalAccessPolicy{
			AllowedInternalCIDRs: []string{"10.0.0.0/8"},
		},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for /8 internal CIDR")
	}

	// /16 should be fine
	p.InternalAccess.AllowedInternalCIDRs = []string{"10.200.0.0/16"}
	if err := p.Validate(); err != nil {
		t.Fatalf("/16 should be valid: %v", err)
	}

	// /24 should be fine
	p.InternalAccess.AllowedInternalCIDRs = []string{"10.200.1.0/24"}
	if err := p.Validate(); err != nil {
		t.Fatalf("/24 should be valid: %v", err)
	}
}

func TestValidate_PortRange(t *testing.T) {
	p := &NetworkPolicy{
		DefaultEgressAction: PolicyActionDeny,
		AllowedEgress: []EgressRule{
			{CIDRs: []string{"0.0.0.0/0"}, Ports: []PortRange{{Start: 0}}},
		},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for port 0")
	}

	p.AllowedEgress[0].Ports = []PortRange{{Start: 80, End: 79}}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for end < start")
	}

	p.AllowedEgress[0].Ports = []PortRange{{Start: 80, End: 443}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid port range should pass: %v", err)
	}
}

func TestValidate_MaxIPSetEntries(t *testing.T) {
	p := &NetworkPolicy{
		DefaultEgressAction: PolicyActionAllow,
		MaxIPSetEntries:     100001,
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for MaxIPSetEntries > 100000")
	}
}

func TestValidate_MaxIPsPerDomain(t *testing.T) {
	p := &NetworkPolicy{
		DefaultEgressAction: PolicyActionAllow,
		MaxIPsPerDomain:     257,
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for MaxIPsPerDomain > 256")
	}
}

func TestMetadataAllowed_Default(t *testing.T) {
	ia := InternalAccessPolicy{}
	if !ia.MetadataAllowed() {
		t.Fatal("default should allow metadata")
	}

	f := false
	ia.AllowMetadata = &f
	if ia.MetadataAllowed() {
		t.Fatal("explicit false should deny metadata")
	}

	tr := true
	ia.AllowMetadata = &tr
	if !ia.MetadataAllowed() {
		t.Fatal("explicit true should allow metadata")
	}
}

func TestEffectiveDefaults(t *testing.T) {
	p := &NetworkPolicy{}
	if p.EffectiveMaxIPSetEntries() != 10000 {
		t.Fatalf("expected 10000, got %d", p.EffectiveMaxIPSetEntries())
	}
	if p.EffectiveMaxIPsPerDomain() != 64 {
		t.Fatalf("expected 64, got %d", p.EffectiveMaxIPsPerDomain())
	}

	p.MaxIPSetEntries = 50000
	p.MaxIPsPerDomain = 128
	if p.EffectiveMaxIPSetEntries() != 50000 {
		t.Fatalf("expected 50000, got %d", p.EffectiveMaxIPSetEntries())
	}
	if p.EffectiveMaxIPsPerDomain() != 128 {
		t.Fatalf("expected 128, got %d", p.EffectiveMaxIPsPerDomain())
	}
}

func TestPresets(t *testing.T) {
	tests := []struct {
		name           string
		defaultAction  PolicyAction
		hasDeniedRules bool
		hasAllowRules  bool
	}{
		{PresetUnrestricted, PolicyActionAllow, false, false},
		{PresetQuarantine, PolicyActionDeny, false, false},
		{PresetCIStandard, PolicyActionAllow, true, false},
		{PresetAgentSandbox, PolicyActionDeny, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := GetPreset(tt.name)
			if p == nil {
				t.Fatal("preset returned nil")
			}
			if p.DefaultEgressAction != tt.defaultAction {
				t.Fatalf("expected action %q, got %q", tt.defaultAction, p.DefaultEgressAction)
			}
			if (len(p.DeniedEgress) > 0) != tt.hasDeniedRules {
				t.Fatalf("denied rules mismatch")
			}
			if (len(p.AllowedEgress) > 0) != tt.hasAllowRules {
				t.Fatalf("allowed rules mismatch")
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("preset should be valid: %v", err)
			}
		})
	}
}

func TestPreset_Unknown(t *testing.T) {
	if p := GetPreset("nonexistent"); p != nil {
		t.Fatal("expected nil for unknown preset")
	}
}

func TestPreset_Quarantine(t *testing.T) {
	p := GetPreset(PresetQuarantine)
	if p.InternalAccess.MetadataAllowed() {
		t.Fatal("quarantine should block metadata")
	}
	if p.InternalAccess.AllowHostAccess {
		t.Fatal("quarantine should block host access")
	}
}

func TestPreset_AgentSandbox(t *testing.T) {
	p := GetPreset(PresetAgentSandbox)
	if !p.AllowDynamicUpdates {
		t.Fatal("agent-sandbox should allow dynamic updates")
	}
	if p.DynamicUpdateConstraints == nil {
		t.Fatal("agent-sandbox should have dynamic constraints")
	}
	if p.DynamicUpdateConstraints.MaxAdditionalDomains != 20 {
		t.Fatalf("expected max 20 domains, got %d", p.DynamicUpdateConstraints.MaxAdditionalDomains)
	}
	if !p.DNS.UsePolicyProxy {
		t.Fatal("agent-sandbox should use DNS policy proxy")
	}
}

func TestResolvePolicy(t *testing.T) {
	// nil/empty = nil (unrestricted)
	if p := ResolvePolicy("", nil); p != nil {
		t.Fatal("expected nil for empty inputs")
	}

	// Preset name
	p := ResolvePolicy(PresetCIStandard, nil)
	if p == nil || p.Name != "restricted-egress" {
		t.Fatal("expected restricted-egress preset")
	}

	// Explicit overrides preset
	explicit := &NetworkPolicy{Name: "custom", DefaultEgressAction: PolicyActionDeny}
	p = ResolvePolicy(PresetCIStandard, explicit)
	if p.Name != "custom" {
		t.Fatal("explicit should override preset")
	}
}

func TestClone(t *testing.T) {
	p := GetPreset(PresetAgentSandbox)
	clone := p.Clone()
	if clone == nil {
		t.Fatal("clone should not be nil")
	}
	if clone == p {
		t.Fatal("clone should be a different pointer")
	}

	// Modify clone, original should be unaffected
	clone.Name = "modified"
	if p.Name == "modified" {
		t.Fatal("modifying clone should not affect original")
	}
}

func TestClone_Nil(t *testing.T) {
	var p *NetworkPolicy
	if p.Clone() != nil {
		t.Fatal("clone of nil should be nil")
	}
}

func TestNetworkPolicy_JSONRoundTrip(t *testing.T) {
	p := GetPreset(PresetAgentSandbox)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var p2 NetworkPolicy
	if err := json.Unmarshal(data, &p2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if p2.Name != p.Name || p2.DefaultEgressAction != p.DefaultEgressAction {
		t.Fatal("JSON round-trip mismatch")
	}
	if !p2.DNS.UsePolicyProxy {
		t.Fatal("DNS proxy flag lost in round-trip")
	}
}
