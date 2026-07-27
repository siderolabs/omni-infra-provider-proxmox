// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"net/netip"
	"strings"
)

// netModels are the Proxmox NIC model keys whose value is the MAC address in a
// net<N> config string (e.g. "virtio=BC:24:11:..."). "macaddr" is the explicit
// override key.
var netModels = map[string]struct{}{
	"virtio": {}, "e1000": {}, "rtl8139": {}, "vmxnet3": {}, "macaddr": {},
}

// parseMACFromNet extracts the MAC address from a Proxmox net<N> config string
// such as "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1,tag=2501".
func parseMACFromNet(net string) (string, error) {
	for part := range strings.SplitSeq(net, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}

		if _, isModel := netModels[strings.ToLower(strings.TrimSpace(key))]; isModel {
			return strings.TrimSpace(value), nil
		}
	}

	return "", fmt.Errorf("no MAC address found in net config %q", net)
}

// addOffset returns addr shifted up by offset (>= 0). It works for both IPv4
// and IPv6 by doing big-endian arithmetic over the address bytes, and errors if
// the result no longer fits the address width.
func addOffset(addr netip.Addr, offset int) (netip.Addr, error) {
	slice := addr.AsSlice() // 4 bytes for IPv4, 16 for IPv6
	if len(slice) == 0 {
		return netip.Addr{}, fmt.Errorf("invalid address %s", addr)
	}

	sum := new(big.Int).Add(new(big.Int).SetBytes(slice), big.NewInt(int64(offset)))

	raw := sum.Bytes()
	if len(raw) > len(slice) {
		return netip.Addr{}, fmt.Errorf("address %s + %d overflows the address space", addr, offset)
	}

	buf := make([]byte, len(slice))
	copy(buf[len(slice)-len(raw):], raw) // left-pad to the original width

	out, ok := netip.AddrFromSlice(buf)
	if !ok {
		return netip.Addr{}, fmt.Errorf("failed to build address from %v", buf)
	}

	return out, nil
}

// resolveIP returns the host IP for a VM given its data and assigned vmid.
// It assumes data.Network != nil (guaranteed by the caller).
func resolveIP(data Data, vmid int) (netip.Addr, error) {
	n := data.Network

	if n.IPAddress != "" {
		addr, err := netip.ParseAddr(n.IPAddress)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("invalid network.ip_address %q: %w", n.IPAddress, err)
		}

		return addr, nil
	}

	base, err := netip.ParseAddr(n.BaseIP)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid network.base_ip %q: %w", n.BaseIP, err)
	}

	r, err := parseVMIDRange(data.VMIDRange)
	if err != nil {
		return netip.Addr{}, err
	}

	return addOffset(base, vmid-r.start)
}

// validateNetwork validates the optional static-networking block. Nil is valid
// (DHCP). It performs a whole-range bounds check up front so a misconfigured
// MachineClass fails immediately rather than only when a high VMID is hit.
func validateNetwork(data Data) error {
	n := data.Network
	if n == nil {
		return nil
	}

	prefix, err := netip.ParsePrefix(n.Subnet)
	if err != nil {
		return fmt.Errorf("invalid network.subnet %q: %w", n.Subnet, err)
	}

	if (n.IPAddress == "") == (n.BaseIP == "") {
		return fmt.Errorf("network requires exactly one of ip_address or base_ip")
	}

	if n.Gateway != "" {
		if _, err = netip.ParseAddr(n.Gateway); err != nil {
			return fmt.Errorf("invalid network.gateway %q: %w", n.Gateway, err)
		}
	}

	for _, ns := range n.Nameservers {
		if _, err = netip.ParseAddr(ns); err != nil {
			return fmt.Errorf("invalid nameserver %q: %w", ns, err)
		}
	}

	if n.IPAddress != "" {
		ip, parseErr := netip.ParseAddr(n.IPAddress)
		if parseErr != nil {
			return fmt.Errorf("invalid network.ip_address %q: %w", n.IPAddress, parseErr)
		}

		return checkHostIP(prefix, ip)
	}

	// derived mode
	if data.VMIDRange == "" {
		return fmt.Errorf("network.base_ip requires vmid_range")
	}

	r, err := parseVMIDRange(data.VMIDRange)
	if err != nil {
		return err
	}

	base, err := netip.ParseAddr(n.BaseIP)
	if err != nil {
		return fmt.Errorf("invalid network.base_ip %q: %w", n.BaseIP, err)
	}

	if err = checkHostIP(prefix, base); err != nil {
		return fmt.Errorf("base_ip: %w", err)
	}

	top, err := addOffset(base, r.end-r.start)
	if err != nil {
		return err
	}

	if err = checkHostIP(prefix, top); err != nil {
		return fmt.Errorf("base_ip + vmid_range width leaves the subnet: %w", err)
	}

	return nil
}

// checkHostIP ensures ip is inside prefix and is a usable host address. It
// rejects the network address (IPv4 network / IPv6 subnet-router anycast) and,
// for IPv4, the broadcast address. A family mismatch between ip and prefix is
// caught by the containment check (Contains is false across families).
func checkHostIP(prefix netip.Prefix, ip netip.Addr) error {
	if !prefix.Contains(ip) {
		return fmt.Errorf("%s is outside subnet %s", ip, prefix)
	}

	if prefix.Bits() < ip.BitLen() && ip == prefix.Masked().Addr() {
		return fmt.Errorf("%s is the network address of %s", ip, prefix)
	}

	if ip.Is4() && prefix.Bits() <= 30 && ip == broadcastAddr(prefix) {
		return fmt.Errorf("%s is the broadcast address of %s", ip, prefix)
	}

	return nil
}

// broadcastAddr returns the IPv4 broadcast address for prefix.
func broadcastAddr(prefix netip.Prefix) netip.Addr {
	b := prefix.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | (uint32(0xffffffff) >> uint(prefix.Bits()))

	var out [4]byte

	binary.BigEndian.PutUint32(out[:], v)

	return netip.AddrFrom4(out)
}

// buildNetworkConfig renders a cloud-init network-config v1 document for the
// primary NIC. It matches the interface by MAC (robust against Talos link-name
// variability) and encodes the address as <ip>/<prefix>, which Talos nocloud
// parses via netip.ParsePrefix. Assumes data.Network != nil.
func buildNetworkConfig(data Data, vmid int, mac string) (string, error) {
	ip, err := resolveIP(data, vmid)
	if err != nil {
		return "", err
	}

	prefix, err := netip.ParsePrefix(data.Network.Subnet)
	if err != nil {
		return "", fmt.Errorf("invalid network.subnet %q: %w", data.Network.Subnet, err)
	}

	// Talos nocloud selects the address family from the subnet type.
	subnetType := "static"
	if !prefix.Addr().Is4() {
		subnetType = "static6"
	}

	var b strings.Builder

	b.WriteString("version: 1\n")
	b.WriteString("config:\n")
	b.WriteString("  - type: physical\n")
	fmt.Fprintf(&b, "    mac_address: %s\n", strings.ToLower(mac))
	b.WriteString("    subnets:\n")
	fmt.Fprintf(&b, "      - type: %s\n", subnetType)
	fmt.Fprintf(&b, "        address: %s/%d\n", ip, prefix.Bits())

	if data.Network.Gateway != "" {
		fmt.Fprintf(&b, "        gateway: %s\n", data.Network.Gateway)
	}

	if len(data.Network.Nameservers) > 0 {
		b.WriteString("  - type: nameserver\n")
		b.WriteString("    address:\n")

		for _, ns := range data.Network.Nameservers {
			fmt.Fprintf(&b, "      - %s\n", ns)
		}
	}

	return b.String(), nil
}
