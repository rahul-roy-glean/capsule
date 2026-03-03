package network

import (
	"net"
	"testing"
)

func TestBuildEgressChainRules_DenyDefault(t *testing.T) {
	policy := &NetworkPolicy{
		DefaultEgressAction: PolicyActionDeny,
		AllowedEgress: []EgressRule{
			{CIDRs: []string{"1.2.3.0/24", "4.5.6.0/24"}},
		},
		InternalAccess: InternalAccessPolicy{
			AllowHostAccess:      true,
			AllowedInternalCIDRs: []string{"10.200.5.0/24"},
		},
	}

	hostIP := net.IPv4(10, 200, 0, 1)
	rules := BuildEgressChainRules(policy, "abcd1234", "veth-abcd1234-v", hostIP)

	if len(rules) == 0 {
		t.Fatal("expected rules to be generated")
	}

	// Check rule ordering
	assertRuleContains(t, rules[0], "ESTABLISHED,RELATED", "first rule should be conntrack")
	assertRuleContains(t, rules[1], "169.254.169.254", "second rule should be MMDS")
	assertRuleContains(t, rules[2], "10.200.0.1/32", "third rule should be host access")
	assertRuleContains(t, rules[3], "10.200.5.0/24", "fourth should be internal CIDR")

	// RFC1918 drops
	assertRuleContains(t, rules[4], "10.0.0.0/8", "should have RFC1918 /8")
	assertRuleContains(t, rules[5], "172.16.0.0/12", "should have RFC1918 /12")
	assertRuleContains(t, rules[6], "192.168.0.0/16", "should have RFC1918 /16")

	// CIDR allow ipset (deny-default)
	assertRuleContains(t, rules[7], "POL-abcd1234-CA", "should reference CIDR allow ipset")
	assertRuleContains(t, rules[7], "ACCEPT", "CIDR allow should ACCEPT")

	// Domain allow ipset
	assertRuleContains(t, rules[8], "POL-abcd1234-DA", "should reference domain allow ipset")

	// Default DROP
	lastRule := rules[len(rules)-1]
	assertRuleContains(t, lastRule, "DROP", "last rule should be DROP for deny-default")
}

func TestBuildEgressChainRules_AllowDefault(t *testing.T) {
	policy := &NetworkPolicy{
		DefaultEgressAction: PolicyActionAllow,
		DeniedEgress: []EgressRule{
			{CIDRs: []string{"10.0.0.0/8"}},
		},
	}

	rules := BuildEgressChainRules(policy, "test1234", "veth-test-v", nil)

	if len(rules) == 0 {
		t.Fatal("expected rules")
	}

	// Find the CIDR deny set rule
	found := false
	for _, rule := range rules {
		for _, arg := range rule {
			if arg == "POL-test1234-CD" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("should reference CIDR deny ipset in allow-default")
	}

	// Last rule should be ACCEPT
	lastRule := rules[len(rules)-1]
	assertRuleContains(t, lastRule, "ACCEPT", "last rule should be ACCEPT for allow-default")
}

func TestBuildEgressChainRules_NoMetadata(t *testing.T) {
	f := false
	policy := &NetworkPolicy{
		DefaultEgressAction: PolicyActionDeny,
		InternalAccess: InternalAccessPolicy{
			AllowMetadata: &f,
		},
	}

	rules := BuildEgressChainRules(policy, "nometa12", "veth-v", nil)

	// MMDS rule should not be present
	for _, rule := range rules {
		for _, arg := range rule {
			if arg == "169.254.169.254" {
				t.Fatal("MMDS rule should not be present when metadata is disabled")
			}
		}
	}
}

func TestBuildEgressChainRules_AllowOtherVMs(t *testing.T) {
	policy := &NetworkPolicy{
		DefaultEgressAction: PolicyActionDeny,
		InternalAccess: InternalAccessPolicy{
			AllowOtherVMs: true,
		},
	}

	rules := BuildEgressChainRules(policy, "vmother1", "veth-v", nil)

	found := false
	for _, rule := range rules {
		for _, arg := range rule {
			if arg == "10.200.0.0/16" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("should include AllowOtherVMs rule for veth supernet")
	}
}

func assertRuleContains(t *testing.T, rule []string, substr, msg string) {
	t.Helper()
	for _, arg := range rule {
		if arg == substr {
			return
		}
	}
	t.Fatalf("%s: rule %v does not contain %q", msg, rule, substr)
}
