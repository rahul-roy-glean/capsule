//go:build !linux
// +build !linux

package network

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// DNSProxy is a stub on non-Linux platforms.
type DNSProxy struct{}

// DNSProxyConfig configures the DNS proxy.
type DNSProxyConfig struct {
	ListenAddr      string
	UpstreamServers []string
	AllowedDomains  []string
	BlockedDomains  []string
	DomIPSetName    string
	NSName          string
	MaxIPsPerDomain int
	MaxIPSetEntries int
	VMID            string
	Logger          *logrus.Logger
}

func NewDNSProxy(_ DNSProxyConfig) *DNSProxy {
	return &DNSProxy{}
}

func (p *DNSProxy) Start(_ func(func() error) error) error {
	return fmt.Errorf("DNS proxy is only supported on Linux")
}

func (p *DNSProxy) Stop() {}

func (p *DNSProxy) UpdateDomains(_, _ []string) {}
