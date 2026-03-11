package main

import "testing"

func makeMMDSNetwork(ip, mac, dns, gateway string) *MMDSData {
	data := &MMDSData{}
	data.Latest.Network.IP = ip
	data.Latest.Network.MAC = mac
	data.Latest.Network.DNS = dns
	data.Latest.Network.Gateway = gateway
	data.Latest.Network.Interface = "eth0"
	return data
}

func TestShouldReconfigureNetworkAfterRestore(t *testing.T) {
	tests := []struct {
		name       string
		current    *guestNetworkState
		data       *MMDSData
		want       bool
		wantReason string
	}{
		{
			name: "same network skips reconfigure",
			current: &guestNetworkState{
				IP:              "10.200.0.2",
				MAC:             "02:fc:00:00:00:01",
				DNS:             "8.8.8.8",
				HasDefaultRoute: true,
			},
			data:       makeMMDSNetwork("10.200.0.2/24", "02:fc:00:00:00:01", "8.8.8.8", "10.200.0.1"),
			want:       false,
			wantReason: "guest network already matches MMDS",
		},
		{
			name: "ip change requires reconfigure",
			current: &guestNetworkState{
				IP:              "10.200.0.2",
				MAC:             "02:fc:00:00:00:01",
				DNS:             "8.8.8.8",
				HasDefaultRoute: true,
			},
			data:       makeMMDSNetwork("10.200.1.2/24", "02:fc:00:00:00:02", "8.8.8.8", "10.200.1.1"),
			want:       true,
			wantReason: "guest IP differs from MMDS",
		},
		{
			name: "mac change requires reconfigure",
			current: &guestNetworkState{
				IP:              "10.200.0.2",
				MAC:             "02:fc:00:00:00:01",
				DNS:             "8.8.8.8",
				HasDefaultRoute: true,
			},
			data:       makeMMDSNetwork("10.200.0.2/24", "02:fc:00:00:00:03", "8.8.8.8", "10.200.0.1"),
			want:       true,
			wantReason: "guest MAC differs from MMDS",
		},
		{
			name: "dns change requires reconfigure",
			current: &guestNetworkState{
				IP:              "10.200.0.2",
				MAC:             "02:fc:00:00:00:01",
				DNS:             "1.1.1.1",
				HasDefaultRoute: true,
			},
			data:       makeMMDSNetwork("10.200.0.2/24", "02:fc:00:00:00:01", "8.8.8.8", "10.200.0.1"),
			want:       true,
			wantReason: "guest DNS differs from MMDS",
		},
		{
			name: "missing route requires reconfigure",
			current: &guestNetworkState{
				IP:              "10.200.0.2",
				MAC:             "02:fc:00:00:00:01",
				DNS:             "8.8.8.8",
				HasDefaultRoute: false,
			},
			data:       makeMMDSNetwork("10.200.0.2/24", "02:fc:00:00:00:01", "8.8.8.8", "10.200.0.1"),
			want:       true,
			wantReason: "guest missing default route",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := shouldReconfigureNetworkAfterRestore(tt.current, tt.data)
			if got != tt.want {
				t.Fatalf("shouldReconfigureNetworkAfterRestore() = %v, want %v (reason=%q)", got, tt.want, reason)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
