// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

type ipAllocation string

const (
	ipDHCP          ipAllocation = "dhcp"
	ipDeterministic ipAllocation = "deterministic"
)

func parseIPAllocation(s string) (ipAllocation, error) {
	switch ipAllocation(s) {
	case "", ipDHCP:
		return ipDHCP, nil
	case ipDeterministic:
		return ipDeterministic, nil
	default:
		return "", fmt.Errorf("unknown ip allocation %q", s)
	}
}

// allocateIP returns the address a VM should use under the given mode, or
// ok=false for dhcp. deterministic derives a stable host from the VM id modulo
// the subnet's host count, excluding the gateway's own address.
//
// This is collision-free only while the fleet stays within the subnet's host
// capacity: VMIDs that differ by a multiple of that capacity map to the same
// host. A full fix needs a used-address set the provider can't cheaply read
// back yet (see the placement-strategy TODO for incremental allocation).
// Size the subnet well above the expected VM count for this mode to be safe.
func allocateIP(mode ipAllocation, subnet netip.Prefix, gateway netip.Addr, vmid int) (netip.Addr, bool, error) {
	if mode == ipDHCP {
		return netip.Addr{}, false, nil
	}

	if !subnet.Addr().Is4() {
		return netip.Addr{}, false, fmt.Errorf("ip allocation requires an IPv4 subnet, got %q", subnet)
	}

	if subnet.Bits() > 30 {
		return netip.Addr{}, false, fmt.Errorf("subnet %q is too small for host allocation", subnet)
	}

	base := subnet.Masked().Addr().As4()
	network := binary.BigEndian.Uint32(base[:])
	maxHosts := (uint32(1) << (32 - subnet.Bits())) - 2

	var gatewayHost uint32

	if gateway.IsValid() {
		if !gateway.Is4() {
			return netip.Addr{}, false, fmt.Errorf("gateway %q must be IPv4", gateway)
		}

		gb := gateway.As4()
		gatewayAddr := binary.BigEndian.Uint32(gb[:])

		// Subtracting first and range-checking after would underflow (wrap to a huge
		// uint32) for a gateway below the subnet, silently defeating the host-offset
		// check below instead of erroring. Range-check on the real addresses first.
		if gatewayAddr < network+1 || gatewayAddr > network+maxHosts {
			return netip.Addr{}, false, fmt.Errorf("gateway %q is not a usable host in subnet %q", gateway, subnet)
		}

		gatewayHost = gatewayAddr - network
	}

	host := uint32(vmid) % maxHosts
	if host == 0 {
		host = maxHosts
	}

	// Never hand out the gateway's own address.
	if gateway.IsValid() && host == gatewayHost {
		host = host%maxHosts + 1
	}

	var b [4]byte

	binary.BigEndian.PutUint32(b[:], network+host)

	return netip.AddrFrom4(b), true, nil
}

// buildNetworkConfig renders cloud-init network-config v1 YAML. dns entries are parsed and
// re-serialized as addresses (not pasted in as-is) so a malformed value - including one
// containing a newline - can't produce invalid or structurally altered YAML.
func buildNetworkConfig(addr, gateway string, dns []string) (string, error) {
	var b strings.Builder

	b.WriteString("version: 1\n")
	b.WriteString("config:\n")
	b.WriteString("  - type: physical\n")
	b.WriteString("    name: eth0\n")
	b.WriteString("    subnets:\n")
	b.WriteString("      - type: static\n")
	fmt.Fprintf(&b, "        address: %s\n", addr)
	fmt.Fprintf(&b, "        gateway: %s\n", gateway)

	if len(dns) > 0 {
		b.WriteString("  - type: nameserver\n")
		b.WriteString("    address:\n")

		for _, ns := range dns {
			parsed, err := netip.ParseAddr(strings.TrimSpace(ns))
			if err != nil {
				return "", fmt.Errorf("invalid dns server %q: %w", ns, err)
			}

			fmt.Fprintf(&b, "      - %s\n", parsed)
		}
	}

	return b.String(), nil
}
