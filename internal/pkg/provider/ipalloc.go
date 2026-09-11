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
// ok=false for dhcp. deterministic derives a stable host from the VM id, which
// is unique per VM so addresses never collide.
func allocateIP(mode ipAllocation, subnet netip.Prefix, vmid int) (netip.Addr, bool, error) {
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

	host := uint32(vmid) % maxHosts
	if host == 0 {
		host = maxHosts
	}

	var b [4]byte

	binary.BigEndian.PutUint32(b[:], network+host)

	return netip.AddrFrom4(b), true, nil
}

// buildNetworkConfig renders a cloud-init v1 network-config for a static address,
// consumed by Talos nocloud so the node has its address before it reaches Omni.
func buildNetworkConfig(addr, gateway string, dns []string) string {
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
			fmt.Fprintf(&b, "      - %s\n", ns)
		}
	}

	return b.String()
}
