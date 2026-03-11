package network

import (
	"fmt"
	"net"
)

// TapDevice represents a TAP network device
type TapDevice struct {
	Name       string
	IP         net.IP
	Gateway    net.IP
	Subnet     *net.IPNet
	MAC        string
	BridgeName string
}

// NetworkConfig holds network configuration to inject into microVM
type NetworkConfig struct {
	IP        string `json:"ip"`
	Gateway   string `json:"gateway"`
	Netmask   string `json:"netmask"`
	DNS       string `json:"dns"`
	Interface string `json:"interface"`
	MAC       string `json:"mac"`
}

// GetNetworkConfig returns the network configuration for a TAP device
func (t *TapDevice) GetNetworkConfig() NetworkConfig {
	ones, _ := t.Subnet.Mask.Size()
	return NetworkConfig{
		IP:        fmt.Sprintf("%s/%d", t.IP.String(), ones),
		Gateway:   t.Gateway.String(),
		Netmask:   net.IP(t.Subnet.Mask).String(),
		DNS:       "8.8.8.8",
		Interface: "eth0",
		MAC:       t.MAC,
	}
}
