// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
)

func TestParseVMIDRange(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		start, end  int
		expectError bool
	}{
		{name: "valid", in: "200-250", start: 200, end: 250},
		{name: "whitespace tolerated", in: " 300 - 310 ", start: 300, end: 310},
		{name: "single-wide", in: "100-100", start: 100, end: 100},
		{name: "missing dash", expectError: true, in: "200"},
		{name: "non-numeric", expectError: true, in: "a-b"},
		{name: "start below 100", expectError: true, in: "50-60"},
		{name: "start after end", expectError: true, in: "300-200"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := provider.ParseVMIDRange(tt.in)
			if tt.expectError {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.start, start)
			require.Equal(t, tt.end, end)
		})
	}
}

func TestLowestFreeVMID(t *testing.T) {
	tests := []struct {
		used        map[int]struct{}
		name        string
		start       int
		end         int
		expected    int
		expectError bool
	}{
		{name: "all free picks start", start: 200, end: 250, expected: 200},
		{
			name:  "skips used ids",
			start: 200, end: 250,
			used:     map[int]struct{}{200: {}, 201: {}},
			expected: 202,
		},
		{
			name:  "exhausted",
			start: 200, end: 201,
			used:        map[int]struct{}{200: {}, 201: {}},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := provider.LowestFreeVMID(tt.start, tt.end, tt.used)
			if tt.expectError {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestResolveIP(t *testing.T) {
	tests := []struct {
		data     *provider.Data
		name     string
		expected string
		vmid     int
	}{
		{
			name:     "explicit",
			expected: "192.168.26.31",
			vmid:     9999,
			data: &provider.Data{Network: &provider.NetworkConfig{
				Subnet:    "192.168.26.0/24",
				IPAddress: "192.168.26.31",
			}},
		},
		{
			name:     "derived offset within range",
			expected: "192.168.26.14",
			vmid:     5304,
			data: &provider.Data{
				VMIDRange: "5300-5350",
				Network:   &provider.NetworkConfig{Subnet: "192.168.26.0/24", BaseIP: "192.168.26.10"},
			},
		},
		{
			name:     "derived octet carry",
			expected: "192.168.1.4",
			vmid:     210, // offset 10 -> .250 + 10 carries into third octet
			data: &provider.Data{
				VMIDRange: "200-400",
				Network:   &provider.NetworkConfig{Subnet: "192.168.0.0/16", BaseIP: "192.168.0.250"},
			},
		},
		{
			name:     "ipv6 explicit",
			expected: "2001:db8::31",
			vmid:     42,
			data: &provider.Data{Network: &provider.NetworkConfig{
				Subnet:    "2001:db8::/64",
				IPAddress: "2001:db8::31",
			}},
		},
		{
			name:     "ipv6 derived with carry",
			expected: "2001:db8::103",
			vmid:     5304, // offset 4 -> ::ff + 4 = ::103
			data: &provider.Data{
				VMIDRange: "5300-5350",
				Network:   &provider.NetworkConfig{Subnet: "2001:db8::/64", BaseIP: "2001:db8::ff"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := provider.ResolveIP(*tt.data, tt.vmid)
			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestValidateNetwork(t *testing.T) {
	valid := func(mut func(*provider.NetworkConfig)) provider.Data {
		n := &provider.NetworkConfig{Subnet: "192.168.26.0/24", Gateway: "192.168.26.1", IPAddress: "192.168.26.31"}
		if mut != nil {
			mut(n)
		}

		return provider.Data{Network: n}
	}

	tests := []struct {
		name        string
		data        provider.Data
		expectError bool
	}{
		{name: "nil network is fine", data: provider.Data{}},
		{name: "valid explicit", data: valid(nil)},
		{
			name: "valid derived",
			data: provider.Data{
				VMIDRange: "5300-5350",
				Network:   &provider.NetworkConfig{Subnet: "192.168.26.0/24", BaseIP: "192.168.26.10"},
			},
		},
		{
			name: "valid ipv6 explicit",
			data: provider.Data{Network: &provider.NetworkConfig{
				Subnet:      "2001:db8::/64",
				Gateway:     "2001:db8::1",
				Nameservers: []string{"2001:4860:4860::8888"},
				IPAddress:   "2001:db8::31",
			}},
		},
		{
			name: "valid ipv6 derived",
			data: provider.Data{
				VMIDRange: "5300-5350",
				Network:   &provider.NetworkConfig{Subnet: "2001:db8::/64", BaseIP: "2001:db8::10"},
			},
		},
		{
			name:        "ipv6 ip is subnet-router anycast",
			expectError: true,
			data:        provider.Data{Network: &provider.NetworkConfig{Subnet: "2001:db8::/64", IPAddress: "2001:db8::"}},
		},
		{
			name:        "ipv6 ip outside subnet",
			expectError: true,
			data:        provider.Data{Network: &provider.NetworkConfig{Subnet: "2001:db8::/64", IPAddress: "2001:dead::5"}},
		},
		{
			name:        "family mismatch: v4 subnet, v6 ip",
			expectError: true,
			data:        valid(func(n *provider.NetworkConfig) { n.IPAddress = "2001:db8::5" }),
		},
		{name: "missing subnet", expectError: true, data: valid(func(n *provider.NetworkConfig) { n.Subnet = "" })},
		{name: "bad subnet", expectError: true, data: valid(func(n *provider.NetworkConfig) { n.Subnet = "192.168.26.0" })},
		{
			name:        "both ip_address and base_ip",
			expectError: true,
			data:        valid(func(n *provider.NetworkConfig) { n.BaseIP = "192.168.26.10" }),
		},
		{
			name:        "neither ip_address nor base_ip",
			expectError: true,
			data:        valid(func(n *provider.NetworkConfig) { n.IPAddress = "" }),
		},
		{
			name:        "base_ip without vmid_range",
			expectError: true,
			data:        provider.Data{Network: &provider.NetworkConfig{Subnet: "192.168.26.0/24", BaseIP: "192.168.26.10"}},
		},
		{
			name:        "explicit ip outside subnet",
			expectError: true,
			data:        valid(func(n *provider.NetworkConfig) { n.IPAddress = "10.0.0.5" }),
		},
		{
			name:        "explicit ip is network address",
			expectError: true,
			data:        valid(func(n *provider.NetworkConfig) { n.IPAddress = "192.168.26.0" }),
		},
		{
			name:        "explicit ip is broadcast address",
			expectError: true,
			data:        valid(func(n *provider.NetworkConfig) { n.IPAddress = "192.168.26.255" }),
		},
		{
			name:        "derived range width leaves subnet",
			expectError: true,
			data: provider.Data{
				VMIDRange: "5300-5350",                                                                  // width 50
				Network:   &provider.NetworkConfig{Subnet: "192.168.26.0/24", BaseIP: "192.168.26.230"}, // 230+50=280
			},
		},
		{name: "bad gateway", expectError: true, data: valid(func(n *provider.NetworkConfig) { n.Gateway = "nope" })},
		{name: "bad nameserver", expectError: true, data: valid(func(n *provider.NetworkConfig) { n.Nameservers = []string{"8.8.8.8", "x"} })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := provider.ValidateNetwork(tt.data)
			if tt.expectError {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestParseMACFromNet(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		expected    string
		expectError bool
	}{
		{
			name:     "virtio with trailing options",
			in:       "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1,tag=2501",
			expected: "BC:24:11:AA:BB:CC",
		},
		{
			name:     "e1000 model",
			in:       "e1000=DE:AD:BE:EF:00:01,bridge=vmbr1",
			expected: "DE:AD:BE:EF:00:01",
		},
		{name: "no mac present", expectError: true, in: "bridge=vmbr0,firewall=1"},
		{name: "empty", expectError: true, in: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := provider.ParseMACFromNet(tt.in)
			if tt.expectError {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildNetworkConfig(t *testing.T) {
	t.Run("explicit with gateway and nameservers", func(t *testing.T) {
		data := provider.Data{Network: &provider.NetworkConfig{
			Subnet:      "192.168.26.0/24",
			Gateway:     "192.168.26.1",
			Nameservers: []string{"8.8.8.8", "8.8.4.4"},
			IPAddress:   "192.168.26.31",
		}}

		got, err := provider.BuildNetworkConfig(data, 0, "BC:24:11:AA:BB:CC")
		require.NoError(t, err)

		expected := `version: 1
config:
  - type: physical
    mac_address: bc:24:11:aa:bb:cc
    subnets:
      - type: static
        address: 192.168.26.31/24
        gateway: 192.168.26.1
  - type: nameserver
    address:
      - 8.8.8.8
      - 8.8.4.4
`
		require.Equal(t, expected, got)
	})

	t.Run("derived without gateway or nameservers", func(t *testing.T) {
		data := provider.Data{
			VMIDRange: "5300-5350",
			Network:   &provider.NetworkConfig{Subnet: "192.168.26.0/24", BaseIP: "192.168.26.10"},
		}

		got, err := provider.BuildNetworkConfig(data, 5304, "bc:24:11:aa:bb:cc")
		require.NoError(t, err)

		expected := `version: 1
config:
  - type: physical
    mac_address: bc:24:11:aa:bb:cc
    subnets:
      - type: static
        address: 192.168.26.14/24
`
		require.Equal(t, expected, got)
	})

	t.Run("ipv6 emits static6", func(t *testing.T) {
		data := provider.Data{
			VMIDRange: "5300-5350",
			Network: &provider.NetworkConfig{
				Subnet:      "2001:db8::/64",
				Gateway:     "2001:db8::1",
				Nameservers: []string{"2001:4860:4860::8888"},
				BaseIP:      "2001:db8::10",
			},
		}

		got, err := provider.BuildNetworkConfig(data, 5304, "bc:24:11:aa:bb:cc")
		require.NoError(t, err)

		expected := `version: 1
config:
  - type: physical
    mac_address: bc:24:11:aa:bb:cc
    subnets:
      - type: static6
        address: 2001:db8::14/64
        gateway: 2001:db8::1
  - type: nameserver
    address:
      - 2001:4860:4860::8888
`
		require.Equal(t, expected, got)
	})
}
