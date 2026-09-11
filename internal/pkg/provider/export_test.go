// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"time"

	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/api/specs"
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

const MaxStartAttempts = maxStartAttempts

func HandleStoppedStartTask(spec *specs.MachineSpec, requestID string) (bool, error) {
	return handleStoppedStartTask(spec, requestID, zap.NewNop())
}

func StartAttemptsExhausted(spec *specs.MachineSpec) error {
	return startAttemptsExhausted(spec)
}
