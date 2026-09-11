// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"net/netip"
	"time"
)

type NodeStatus = nodeStatus

func PickNode(nodes []NodeStatus) NodeStatus {
	return pickNode(nodes)
}

func BuildTagsOption(userTags []string, machineRequestSet string) (string, bool) {
	return buildTagsOption(userTags, machineRequestSet)
}

func BuildFirmwareOptions(data Data, selectedStorage string) map[string]any {
	options := buildFirmwareOptions(data, selectedStorage)

	result := map[string]any{}
	for _, option := range options {
		result[option.Name] = option.Value
	}

	return result
}

func BuildUSBDeviceOptions(devices []USBDevice) map[string]any {
	options := buildUSBDeviceOptions(devices)

	result := map[string]any{}
	for _, option := range options {
		result[option.Name] = option.Value
	}

	return result
}

func PoolCreateDecision(exists bool, poolID, machineRequestSet string) (bool, error) {
	return poolCreateDecision(exists, poolID, machineRequestSet)
}

type Scheduler = scheduler

func NewScheduler() *Scheduler {
	return newScheduler()
}

func NewSchedulerWithClock(now func() time.Time, ttl time.Duration) *Scheduler {
	return newSchedulerWithClock(now, ttl)
}

func (s *scheduler) Pick(nodes []NodeStatus, set, requestID string, memory uint64, strategy string, materialized map[string]struct{}) NodeStatus {
	parsed, _ := parseStrategy(strategy) //nolint:errcheck

	return s.pick(nodes, set, requestID, memory, parsed, materialized)
}

func ParseStrategy(s string) (string, error) {
	parsed, err := parseStrategy(s)

	return string(parsed), err
}

func (s *scheduler) Release(requestID string) {
	s.release(requestID)
}

func ShouldCountSetVMs(data Data, hasSet bool) bool {
	return shouldCountSetVMs(data, hasSet)
}

func ParseIPAllocation(s string) (string, error) {
	alloc, err := parseIPAllocation(s)

	return string(alloc), err
}

func AllocateIP(mode string, subnet netip.Prefix, vmid int) (netip.Addr, bool, error) {
	alloc, err := parseIPAllocation(mode)
	if err != nil {
		return netip.Addr{}, false, err
	}

	return allocateIP(alloc, subnet, vmid)
}

func BuildNetworkConfig(addr, gateway string, dns []string) string {
	return buildNetworkConfig(addr, gateway, dns)
}
