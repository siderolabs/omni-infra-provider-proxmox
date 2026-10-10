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
// It assumes static networking is configured (guaranteed by the caller).
func resolveIP(data Data, vmid int) (netip.Addr, error) {
	if data.NetworkIP != "" {
		addr, err := netip.ParseAddr(data.NetworkIP)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("invalid network_ip %q: %w", data.NetworkIP, err)
		}

		return addr, nil
	}

	base, err := netip.ParseAddr(data.NetworkBaseIP)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid network_base_ip %q: %w", data.NetworkBaseIP, err)
	}

	r, err := parseVMIDRange(data.VMIDRange)
	if err != nil {
		return netip.Addr{}, err
	}

	return addOffset(base, vmid-r.start)
}

// validateNetwork validates the optional static-networking fields. An empty
// network_subnet is valid (DHCP). It performs a whole-range bounds check up
// front so a misconfigured MachineClass fails immediately rather than only when
// a high VMID is hit.
func validateNetwork(data Data) error {
	if data.NetworkSubnet == "" {
		if data.NetworkIP != "" || data.NetworkBaseIP != "" || data.NetworkGateway != "" ||
			len(data.NetworkNameservers) > 0 || data.NetworkMTU != 0 {
			return fmt.Errorf("network_subnet is required when any other network_* field is set")
		}

		return nil
	}

	prefix, err := netip.ParsePrefix(data.NetworkSubnet)
	if err != nil {
		return fmt.Errorf("invalid network_subnet %q: %w", data.NetworkSubnet, err)
	}

	if (data.NetworkIP == "") == (data.NetworkBaseIP == "") {
		return fmt.Errorf("static networking requires exactly one of network_ip or network_base_ip")
	}

	if data.NetworkGateway != "" {
		if _, err = netip.ParseAddr(data.NetworkGateway); err != nil {
			return fmt.Errorf("invalid network_gateway %q: %w", data.NetworkGateway, err)
		}
	}

	for _, ns := range data.NetworkNameservers {
		if _, err = netip.ParseAddr(ns); err != nil {
			return fmt.Errorf("invalid nameserver %q: %w", ns, err)
		}
	}

	if err = validateMTU(data.NetworkMTU, prefix.Addr().Is4()); err != nil {
		return err
	}

	if data.NetworkIP != "" {
		ip, parseErr := netip.ParseAddr(data.NetworkIP)
		if parseErr != nil {
			return fmt.Errorf("invalid network_ip %q: %w", data.NetworkIP, parseErr)
		}

		return checkHostIP(prefix, ip)
	}

	// derived mode
	if data.VMIDRange == "" {
		return fmt.Errorf("network_base_ip requires vmid_range")
	}

	r, err := parseVMIDRange(data.VMIDRange)
	if err != nil {
		return err
	}

	base, err := netip.ParseAddr(data.NetworkBaseIP)
	if err != nil {
		return fmt.Errorf("invalid network_base_ip %q: %w", data.NetworkBaseIP, err)
	}

	if err = checkHostIP(prefix, base); err != nil {
		return fmt.Errorf("network_base_ip: %w", err)
	}

	top, err := addOffset(base, r.end-r.start)
	if err != nil {
		return err
	}

	if err = checkHostIP(prefix, top); err != nil {
		return fmt.Errorf("network_base_ip + vmid_range width leaves the subnet: %w", err)
	}

	return nil
}

// validateMTU bounds network_mtu. Zero means "unset": the link keeps whatever
// the hypervisor hands it. The lower bound is the minimum MTU each IP family
// requires; the upper bound is the largest value an Ethernet link spec accepts.
func validateMTU(mtu int, isIPv4 bool) error {
	if mtu == 0 {
		return nil
	}

	minMTU := 1280 // RFC 8200 minimum link MTU for IPv6
	if isIPv4 {
		minMTU = 576 // RFC 791 minimum reassembly buffer
	}

	if mtu < minMTU || mtu > 65535 {
		return fmt.Errorf("network_mtu %d is out of range [%d, 65535]", mtu, minMTU)
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
// parses via netip.ParsePrefix. Assumes static networking is configured.
func buildNetworkConfig(data Data, vmid int, mac string) (string, error) {
	ip, err := resolveIP(data, vmid)
	if err != nil {
		return "", err
	}

	prefix, err := netip.ParsePrefix(data.NetworkSubnet)
	if err != nil {
		return "", fmt.Errorf("invalid network_subnet %q: %w", data.NetworkSubnet, err)
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

	if data.NetworkMTU != 0 {
		fmt.Fprintf(&b, "    mtu: %d\n", data.NetworkMTU)
	}

	b.WriteString("    subnets:\n")
	fmt.Fprintf(&b, "      - type: %s\n", subnetType)
	fmt.Fprintf(&b, "        address: %s/%d\n", ip, prefix.Bits())

	if data.NetworkGateway != "" {
		fmt.Fprintf(&b, "        gateway: %s\n", data.NetworkGateway)
	}

	if len(data.NetworkNameservers) > 0 {
		b.WriteString("  - type: nameserver\n")
		b.WriteString("    address:\n")

		for _, ns := range data.NetworkNameservers {
			fmt.Fprintf(&b, "      - %s\n", ns)
		}
	}

	return b.String(), nil
}
