// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
)

func TestExpandNetworkConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		net  provider.Network
		want []string
	}{
		{"addresses sorted", provider.Network{Addresses: []string{"10.0.0.3/24", "10.0.0.1/24"}}, []string{"10.0.0.1/24", "10.0.0.3/24"}},
		{"range", provider.Network{Range: &provider.IPRange{Start: "10.0.0.1", End: "10.0.0.3", Prefix: 24}}, []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}},
		{"single-address range", provider.Network{Range: &provider.IPRange{Start: "10.0.0.1", End: "10.0.0.1", Prefix: 24}}, []string{"10.0.0.1/24"}},
		{"mixed families", provider.Network{Addresses: []string{"2001:db8::1/64", "10.0.0.1/24"}}, []string{"10.0.0.1/24", "2001:db8::1/64"}},
		{"both", provider.Network{Addresses: []string{"10.0.0.1/24"}, Range: &provider.IPRange{Start: "10.0.0.1", End: "10.0.0.2", Prefix: 24}}, nil},
		{"neither", provider.Network{}, nil},
		{"bare IP", provider.Network{Addresses: []string{"10.0.0.1"}}, nil},
		{"garbage CIDR", provider.Network{Addresses: []string{"garbage/24"}}, nil},
		{"duplicate", provider.Network{Addresses: []string{"10.0.0.1/24", "10.0.0.1/25"}}, nil},
		{"reversed range", provider.Network{Range: &provider.IPRange{Start: "10.0.0.3", End: "10.0.0.1", Prefix: 24}}, nil},
		{"mixed range", provider.Network{Range: &provider.IPRange{Start: "10.0.0.1", End: "2001:db8::1", Prefix: 24}}, nil},
		{"bad prefix", provider.Network{Range: &provider.IPRange{Start: "10.0.0.1", End: "10.0.0.3", Prefix: 64}}, nil},
		{"range over cap", provider.Network{Range: &provider.IPRange{Start: "10.0.0.1", End: "10.0.20.1", Prefix: 16}}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			actual, err := provider.ExpandNetworkConfig(&tt.net)
			if tt.want == nil {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			addresses := make([]string, 0, len(actual))
			for _, ip := range actual {
				addresses = append(addresses, ip.String())
			}

			assert.Equal(t, tt.want, addresses)
		})
	}
}

func TestStaticIPMarker(t *testing.T) {
	t.Parallel()

	ip := netip.MustParsePrefix("10.0.0.1/24")
	description := provider.BuildVMDescription(ip)
	assert.Equal(t, "managed by omni-infra-provider-proxmox\nomni-static-ip: 10.0.0.1/24", description)
	parsed, ok := provider.ParseStaticIPMarker(description)
	require.True(t, ok)
	assert.Equal(t, ip, parsed)

	for _, description := range []string{"", "unrelated", "omni-static-ip: garbage"} {
		_, ok := provider.ParseStaticIPMarker(description)
		assert.False(t, ok)
	}
}

func TestParsePrimaryMAC(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "BC:24:11:AA:BB:CC", provider.ParsePrimaryMAC("virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1"))
	assert.Empty(t, provider.ParsePrimaryMAC(""))
	assert.Empty(t, provider.ParsePrimaryMAC("bridge=vmbr0"))
}

func TestBuildNocloudNetworkConfig(t *testing.T) {
	t.Parallel()

	const noGatewayYAML = "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: static\n        address: 10.0.0.1/24\n"

	tests := []struct {
		want        string
		name        string
		ip          string
		gateway     string
		mac         string
		nameservers []string
		invalid     bool
	}{
		{`version: 1
config:
  - type: physical
    name: eth0
    mac_address: BC:24:11:AA:BB:CC
    subnets:
      - type: static
        address: 10.0.0.1/24
        gateway: 10.0.0.254
  - type: nameserver
    address:
      - 10.0.0.53
`, "v4", "10.0.0.1/24", "10.0.0.254", "BC:24:11:AA:BB:CC", []string{"10.0.0.53"}, false},
		{`version: 1
config:
  - type: physical
    name: eth0
    subnets:
      - type: static6
        address: 2001:db8::1/64
        gateway: 2001:db8::ff
`, "v6", "2001:db8::1/64", "2001:db8::ff", "", nil, false},
		{noGatewayYAML, "no gateway or DNS", "10.0.0.1/24", "", "", nil, false},
		{"", "mismatched gateway", "10.0.0.1/24", "2001:db8::ff", "", nil, true},
		{"", "invalid gateway", "10.0.0.1/24", "garbage", "", nil, true},
		{"", "invalid nameserver", "10.0.0.1/24", "", "", []string{"garbage"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			actual, err := provider.BuildNocloudNetworkConfig(netip.MustParsePrefix(tt.ip), tt.gateway, tt.nameservers, tt.mac)
			if tt.invalid {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, actual)
		})
	}
}

//nolint:gocognit
func TestIPAllocator(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		resource    map[string]any
		name        string
		markedIP    string
		want        string
		configError bool
	}{
		{nil, "empty", "10.0.0.1/24", "10.0.0.1/24", false},
		{map[string]any{"type": "qemu", "node": "pve", "vmid": 100, "tags": "machine-request.set-1"}, "used", "10.0.0.1/24", "10.0.0.2/24", false},
		{map[string]any{"type": "qemu", "node": "pve", "vmid": 100, "tags": "machine-request.set-1"}, "used with different prefix", "10.0.0.1/25", "10.0.0.2/24", false},
		{map[string]any{"type": "qemu", "node": "pve", "vmid": 100, "tags": "machine-request.set-1"}, "used with v4-mapped IPv6", "::ffff:10.0.0.1/24", "10.0.0.2/24", false},
		{map[string]any{"type": "qemu", "node": "pve", "vmid": 100}, "foreign VM marker is ignored", "10.0.0.1/24", "10.0.0.1/24", false},
		{map[string]any{"type": "qemu", "node": "pve", "vmid": 100}, "foreign VM config failure is ignored", "10.0.0.1/24", "10.0.0.1/24", true},
		{map[string]any{"type": "qemu", "node": "pve", "vmid": 100, "tags": "machine-request.set-1", "template": 1}, "template", "10.0.0.1/24", "10.0.0.1/24", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resources := []map[string]any{}
			if tt.resource != nil {
				resources = append(resources, tt.resource)
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any

				switch r.URL.Path {
				case "/cluster/status":
					data = []map[string]any{}
				case "/nodes/pve/status":
					data = map[string]any{}
				case "/cluster/resources":
					data = resources
				case "/nodes/pve/qemu/100/status/current":
					data = map[string]any{"vmid": 100}
				case "/nodes/pve/qemu/100/config":
					if tt.configError {
						http.Error(w, "unavailable", http.StatusInternalServerError)

						return
					}

					data = map[string]any{"description": provider.BuildVMDescription(netip.MustParsePrefix(tt.markedIP))}
				default:
					http.NotFound(w, r)

					return
				}

				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			client := proxmox.NewClient(srv.URL)
			allocator := provider.NewIPAllocator()
			candidates, err := provider.ExpandNetworkConfig(&provider.Network{Range: &provider.IPRange{Start: "10.0.0.1", End: "10.0.0.10", Prefix: 24}})
			require.NoError(t, err)

			first, err := allocator.Allocate(t.Context(), client, candidates, "first")
			require.NoError(t, err)
			assert.Equal(t, tt.want, first.String())

			retry, err := allocator.Allocate(t.Context(), client, candidates, "first")
			require.NoError(t, err)
			assert.Equal(t, first, retry)

			if tt.name != "empty" {
				return
			}

			var wg sync.WaitGroup

			results := make(chan string, 9)

			for i := 1; i < 10; i++ {
				wg.Go(func() {
					ip, allocErr := allocator.Allocate(t.Context(), client, candidates, fmt.Sprintf("request-%d", i))
					if allocErr != nil {
						t.Errorf("allocate: %v", allocErr)

						return
					}

					results <- ip.String()
				})
			}

			wg.Wait()
			close(results)

			seen := map[string]bool{first.String(): true}
			for address := range results {
				assert.False(t, seen[address], "duplicate allocation: %s", address)
				seen[address] = true
			}

			assert.Len(t, seen, 10)
			_, err = allocator.Allocate(t.Context(), client, candidates, "exhausted")
			require.ErrorContains(t, err, "static IP pool exhausted")
			allocator.Release("first")
			again, err := allocator.Allocate(t.Context(), client, candidates, "new")
			require.NoError(t, err)
			assert.Equal(t, first, again)
		})
	}
}

func TestIPAllocatorPingFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)

			return
		}

		data := any(map[string]any{"vmid": 100})
		if r.URL.Path == "/cluster/status" {
			data = []map[string]any{}
		}

		if r.URL.Path == "/nodes/pve/status" {
			data = map[string]any{}
		}

		if r.URL.Path == "/cluster/resources" {
			data = []map[string]any{{"type": "qemu", "node": "pve", "vmid": 100, "tags": "machine-request.set-1"}}
		}

		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := provider.NewIPAllocator().Allocate(t.Context(), proxmox.NewClient(srv.URL), []netip.Prefix{netip.MustParsePrefix("10.0.0.1/24")}, "req")
	require.ErrorContains(t, err, "VM 100")
}
