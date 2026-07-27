// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import "time"

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

func ParseVMIDRange(s string) (start, end int, err error) {
	r, err := parseVMIDRange(s)

	return r.start, r.end, err
}

func LowestFreeVMID(start, end int, used map[int]struct{}) (int, error) {
	return lowestFreeVMID(vmidRange{start: start, end: end}, used)
}

func ResolveIP(data Data, vmid int) (string, error) {
	ip, err := resolveIP(data, vmid)
	if err != nil {
		return "", err
	}

	return ip.String(), nil
}

func ValidateNetwork(data Data) error {
	return validateNetwork(data)
}

func ParseMACFromNet(net string) (string, error) {
	return parseMACFromNet(net)
}

func BuildNetworkConfig(data Data, vmid int, mac string) (string, error) {
	return buildNetworkConfig(data, vmid, mac)
}
