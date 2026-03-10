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

// incrementIP increments an IP address by offset
func incrementIP(ip net.IP, offset uint32) net.IP {
	ip = ip.To4()
	if ip == nil {
		return nil
	}

	result := make(net.IP, 4)
	copy(result, ip)

	for i := 3; i >= 0 && offset > 0; i-- {
		sum := uint32(result[i]) + offset
		result[i] = byte(sum & 0xff)
		offset = sum >> 8
	}

	return result
}

// generateMAC generates a MAC address based on IP
func generateMAC(ip net.IP) string {
	ip = ip.To4()
	if ip == nil {
		return "02:00:00:00:00:01"
	}
	return fmt.Sprintf("02:FC:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])
}
