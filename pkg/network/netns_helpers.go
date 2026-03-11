//go:build linux
// +build linux

package network

import (
	"fmt"
	"net"
)

// incrementIP increments an IP address by offset.
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

// generateMAC generates a MAC address based on IP.
func generateMAC(ip net.IP) string {
	ip = ip.To4()
	if ip == nil {
		return "02:00:00:00:00:01"
	}
	return fmt.Sprintf("02:FC:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])
}
