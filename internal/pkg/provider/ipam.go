// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/luthermonson/go-proxmox"
	"go.yaml.in/yaml/v4"
)

const (
	staticIPMarker   = "omni-static-ip:"
	vmManagedComment = "managed by omni-infra-provider-proxmox"
	maxPoolAddresses = 4096
)

var primaryMACPattern = regexp.MustCompile(`virtio=([0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5})`)

func expandNetworkConfig(n *Network) ([]netip.Prefix, error) {
	if n == nil || (len(n.Addresses) == 0) == (n.Range == nil) {
		return nil, fmt.Errorf("exactly one of addresses or range is required")
	}

	var candidates []netip.Prefix

	if n.Range != nil {
		start, err := netip.ParseAddr(n.Range.Start)
		if err != nil {
			return nil, fmt.Errorf("invalid range start: %w", err)
		}

		end, err := netip.ParseAddr(n.Range.End)
		if err != nil {
			return nil, fmt.Errorf("invalid range end: %w", err)
		}

		if start.Is4() != end.Is4() || start.Compare(end) > 0 {
			return nil, fmt.Errorf("range bounds must have the same family and start must not follow end")
		}

		if _, err = start.Prefix(n.Range.Prefix); err != nil {
			return nil, fmt.Errorf("invalid range prefix: %w", err)
		}

		for addr := start; addr.IsValid() && addr.Compare(end) <= 0; addr = addr.Next() {
			if len(candidates) == maxPoolAddresses {
				return nil, fmt.Errorf("static IP pool exceeds %d addresses", maxPoolAddresses)
			}

			candidates = append(candidates, netip.PrefixFrom(addr, n.Range.Prefix))
		}
	} else {
		if len(n.Addresses) > maxPoolAddresses {
			return nil, fmt.Errorf("static IP pool exceeds %d addresses", maxPoolAddresses)
		}

		for _, address := range n.Addresses {
			prefix, err := netip.ParsePrefix(address)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %w", address, err)
			}

			candidates = append(candidates, prefix)
		}
	}

	slices.SortFunc(candidates, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })

	seen := make(map[netip.Addr]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate.Addr()]; ok {
			return nil, fmt.Errorf("duplicate static IP %s", candidate.Addr())
		}

		seen[candidate.Addr()] = struct{}{}
	}

	return candidates, nil
}

func buildVMDescription(ip netip.Prefix) string {
	return vmManagedComment + "\n" + staticIPMarker + " " + ip.String()
}

func parseStaticIPMarker(description string) (netip.Prefix, bool) {
	for line := range strings.SplitSeq(description, "\n") {
		if address, ok := strings.CutPrefix(strings.TrimSpace(line), staticIPMarker); ok {
			ip, err := netip.ParsePrefix(strings.TrimSpace(address))

			return ip, err == nil
		}
	}

	return netip.Prefix{}, false
}

func parsePrimaryMAC(net0 string) string {
	match := primaryMACPattern.FindStringSubmatch(net0)
	if len(match) < 2 {
		return ""
	}

	return match[1]
}

func buildNocloudNetworkConfig(ip netip.Prefix, gateway string, nameservers []string, mac string) (string, error) {
	if !ip.IsValid() {
		return "", fmt.Errorf("invalid static IP")
	}

	type subnet struct {
		Type    string `yaml:"type"`
		Address string `yaml:"address"`
		Gateway string `yaml:"gateway,omitempty"`
	}

	type configEntry struct {
		Type       string   `yaml:"type"`
		Name       string   `yaml:"name,omitempty"`
		MACAddress string   `yaml:"mac_address,omitempty"`
		Subnets    []subnet `yaml:"subnets,omitempty"`
		Address    []string `yaml:"address,omitempty"`
	}

	config := struct {
		Config []configEntry `yaml:"config"`
	}{}

	sub := subnet{Type: "static", Address: ip.String()}
	if ip.Addr().Is6() {
		sub.Type = "static6"
	}

	if gateway != "" {
		addr, err := netip.ParseAddr(gateway)
		if err != nil {
			return "", fmt.Errorf("invalid gateway %q: %w", gateway, err)
		}

		if addr.Is4() != ip.Addr().Is4() {
			return "", fmt.Errorf("gateway %s does not match the address family of the allocated address %s", addr, ip)
		}

		sub.Gateway = addr.String()
	}

	for _, ns := range nameservers {
		if _, err := netip.ParseAddr(ns); err != nil {
			return "", fmt.Errorf("invalid nameserver %q: %w", ns, err)
		}
	}

	config.Config = append(config.Config, configEntry{Type: "physical", Name: "eth0", MACAddress: mac, Subnets: []subnet{sub}})
	if len(nameservers) > 0 {
		config.Config = append(config.Config, configEntry{Type: "nameserver", Address: nameservers})
	}

	var output strings.Builder

	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)

	if err := encoder.Encode(config); err != nil {
		return "", fmt.Errorf("marshal nocloud network config: %w", err)
	}

	return "version: 1\n" + output.String(), nil
}

type ipAllocator struct {
	reserved map[string]netip.Prefix
	mu       sync.Mutex
}

// occupancyKey normalizes an address for pool occupancy comparisons, so the
// same IP always maps to one key regardless of prefix length or v4-mapped
// IPv6 encoding.
func occupancyKey(a netip.Addr) netip.Addr {
	return a.Unmap()
}

func newIPAllocator() *ipAllocator {
	return &ipAllocator{reserved: make(map[string]netip.Prefix)}
}

func (a *ipAllocator) allocate(ctx context.Context, client *proxmox.Client, candidates []netip.Prefix, requestID string) (netip.Prefix, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if address, ok := a.reserved[requestID]; ok {
		return address, nil
	}

	used, err := a.scanUsedStaticIPs(ctx, client)
	if err != nil {
		return netip.Prefix{}, err
	}

	for _, ip := range a.reserved {
		used[occupancyKey(ip.Addr())] = struct{}{}
	}

	for _, candidate := range candidates {
		if _, ok := used[occupancyKey(candidate.Addr())]; !ok {
			a.reserved[requestID] = candidate

			return candidate, nil
		}
	}

	return netip.Prefix{}, fmt.Errorf("static IP pool exhausted: all %d addresses are in use", len(candidates))
}

func (a *ipAllocator) scanUsedStaticIPs(ctx context.Context, client *proxmox.Client) (map[netip.Addr]struct{}, error) {
	cluster, err := client.Cluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("get Proxmox cluster: %w", err)
	}

	resources, err := cluster.Resources(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Proxmox cluster resources: %w", err)
	}

	used := make(map[netip.Addr]struct{})

	for _, res := range resources {
		if res.Type != "qemu" || res.Template == 1 {
			continue
		}

		// Auto-provisioned VMs always carry a machine-request tag. VMs without it
		// are not ours, so their descriptions are not scanned.
		if !strings.Contains(res.Tags, machineRequestTagPrefix) {
			continue
		}

		node, err := client.Node(ctx, res.Node)
		if err != nil {
			return nil, fmt.Errorf("get node for VM %d: %w", res.VMID, err)
		}

		vm, err := node.VirtualMachine(ctx, int(res.VMID))
		if err != nil {
			return nil, fmt.Errorf("get VM %d: %w", res.VMID, err)
		}

		if err = vm.Ping(ctx); err != nil {
			return nil, fmt.Errorf("read VM %d config: %w", res.VMID, err)
		}

		if ip, ok := parseStaticIPMarker(vm.VirtualMachineConfig.Description); ok {
			used[occupancyKey(ip.Addr())] = struct{}{}
		}
	}

	return used, nil
}

func (a *ipAllocator) release(requestID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.reserved, requestID)
}

func (a *ipAllocator) reserve(requestID string, ip netip.Prefix) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.reserved[requestID] = ip
}
