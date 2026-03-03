//go:build !linux
// +build !linux

package network

import (
	"fmt"
	"net"

	"github.com/sirupsen/logrus"
)

// PolicyEnforcer is a stub on non-Linux platforms.
type PolicyEnforcer struct{}

// PolicyEnforcerConfig holds configuration for creating a PolicyEnforcer.
type PolicyEnforcerConfig struct {
	VMID       string
	NSName     string
	VethVM     string
	HostVethIP net.IP
	Policy     *NetworkPolicy
	Logger     *logrus.Logger
}

func NewPolicyEnforcer(_ PolicyEnforcerConfig) *PolicyEnforcer {
	return &PolicyEnforcer{}
}

func (e *PolicyEnforcer) Apply() error {
	return fmt.Errorf("network policy enforcement is only supported on Linux")
}

func (e *PolicyEnforcer) Update(_ *NetworkPolicy) error {
	return fmt.Errorf("network policy enforcement is only supported on Linux")
}

func (e *PolicyEnforcer) Remove() {}

func (e *PolicyEnforcer) AddIngressPort(_ int) {}

func (e *PolicyEnforcer) SetInitialIngressPorts(_ []int) {}

func (e *PolicyEnforcer) StartDNSProxy(_ func(func() error) error) error {
	return fmt.Errorf("DNS proxy is only supported on Linux")
}

func (e *PolicyEnforcer) GetPolicy() *NetworkPolicy {
	return nil
}
