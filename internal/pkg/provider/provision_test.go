// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/api/specs"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/ha"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

func writeData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
}

const (
	talosWorkers = "talos-workers"

	nodeAName = "node-a"
	nodeBName = "node-b"
	nodeCName = "node-c"

	worker1 = "worker-1"
	worker2 = "worker-2"
	worker3 = "worker-3"
)

func TestPickNode(t *testing.T) {
	const (
		nodeA = "NodeA"
		nodeB = "NodeB"
		nodeC = "NodeC"
		nodeD = "NodeD"
	)

	tests := []struct {
		name     string
		expected string
		input    []provider.NodeStatus
	}{
		{
			name: "Single node should be returned",
			input: []provider.NodeStatus{
				{Name: "node1", MemoryFree: 1, SameMachineRequestSetVMs: 0},
			},
			expected: "node1",
		},
		{
			name: "Primary criteria: Pick node with fewer same-set VMs",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 1.0, SameMachineRequestSetVMs: 10},
				// Node B has less memory, but is free (0 VMs) -> Should win
				{Name: nodeB, MemoryFree: 0.5, SameMachineRequestSetVMs: 0},
			},
			expected: nodeB,
		},
		{
			name: "Secondary criteria: If VMs equal, pick node with MOST free memory",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 0.5, SameMachineRequestSetVMs: 5},
				{Name: nodeB, MemoryFree: 1.0, SameMachineRequestSetVMs: 5}, // More memory
				{Name: nodeC, MemoryFree: 0.1, SameMachineRequestSetVMs: 5},
			},
			expected: nodeB,
		},
		{
			name: "Complex scenario",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 0.1, SameMachineRequestSetVMs: 2},
				{Name: nodeB, MemoryFree: 0.05, SameMachineRequestSetVMs: 1}, // Best VM count
				{Name: nodeC, MemoryFree: 0.04, SameMachineRequestSetVMs: 1}, // Same VM count, less memory than B
				{Name: nodeD, MemoryFree: 1, SameMachineRequestSetVMs: 5},
			},
			expected: nodeB,
		},
		{
			name: "No free memory",
			input: []provider.NodeStatus{
				{Name: nodeA, MemoryFree: 0, SameMachineRequestSetVMs: 0},
				{Name: nodeB, MemoryFree: 1, SameMachineRequestSetVMs: 0},
			},
			expected: nodeB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			result := provider.PickNode(tt.input)

			// Assert
			require.Equal(t, tt.expected, result.Name)
		})
	}
}

func TestShouldCountSetVMs(t *testing.T) {
	tests := []struct {
		name     string
		data     provider.Data
		hasSet   bool
		expected bool
	}{
		{name: "set, no HA: spread", hasSet: true, expected: true},
		{name: "set, HA: no spread", data: provider.Data{HA: &ha.Config{}}, hasSet: true, expected: false},
		{name: "no set, no HA: no spread", hasSet: false, expected: false},
		{name: "no set, HA: no spread", data: provider.Data{HA: &ha.Config{}}, hasSet: false, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, provider.ShouldCountSetVMs(tt.data, tt.hasSet))
		})
	}
}

func TestPoolCreateDecision(t *testing.T) {
	tests := []struct {
		name              string
		poolID            string
		machineRequestSet string
		exists            bool
		expectedCreate    bool
		expectedErr       bool
	}{
		{
			name:              "Pool exists: no-op",
			poolID:            "my-pool",
			machineRequestSet: talosWorkers,
			exists:            true,
			expectedCreate:    false,
		},
		{
			name:              "Pool absent, matches machine request set: create",
			poolID:            talosWorkers,
			machineRequestSet: talosWorkers,
			expectedCreate:    true,
		},
		{
			name:              "Pool absent, user-specified: refuse",
			poolID:            "gpu-pool",
			machineRequestSet: talosWorkers,
			expectedErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			create, err := provider.PoolCreateDecision(tt.exists, tt.poolID, tt.machineRequestSet)

			if tt.expectedErr {
				require.Error(t, err)
				require.False(t, create)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expectedCreate, create)
		})
	}
}

func TestBuildTagsOption(t *testing.T) {
	tests := []struct {
		name              string
		machineRequestSet string
		expectedValue     string
		userTags          []string
		expectedOk        bool
	}{
		{
			name:       "No user tags, no request set",
			expectedOk: false,
		},
		{
			name:              "Request set only",
			machineRequestSet: talosWorkers,
			expectedValue:     "machine-request.talos-workers",
			expectedOk:        true,
		},
		{
			name:          "User tags only",
			userTags:      []string{"talos-node", "prod"},
			expectedValue: "talos-node;prod",
			expectedOk:    true,
		},
		{
			name:              "User tags first, internal tag last",
			userTags:          []string{"talos-node", "prod"},
			machineRequestSet: talosWorkers,
			expectedValue:     "talos-node;prod;machine-request.talos-workers",
			expectedOk:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, ok := provider.BuildTagsOption(tt.userTags, tt.machineRequestSet)

			require.Equal(t, tt.expectedOk, ok)
			require.Equal(t, tt.expectedValue, value)
		})
	}
}

func TestSchedulerSpreadsInFlightPlacements(t *testing.T) {
	s := provider.NewScheduler()

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 0.9},
			{Name: nodeBName, MemoryFree: 0.8},
			{Name: nodeCName, MemoryFree: 0.7},
		}
	}

	picked := make([]string, 0, 3)

	for _, requestID := range []string{worker1, worker2, worker3} {
		picked = append(picked, s.Pick(nodes(), talosWorkers, requestID, 0, "spread", nil).Name)
	}

	require.ElementsMatch(t, []string{nodeAName, nodeBName, nodeCName}, picked)
}

func TestSchedulerReleasesMaterializedReservations(t *testing.T) {
	s := provider.NewScheduler()

	twoNodes := func(proxmoxOnA int) []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 1.0, SameMachineRequestSetVMs: proxmoxOnA},
			{Name: nodeBName, MemoryFree: 0.9},
		}
	}

	require.Equal(t, nodeAName, s.Pick(twoNodes(0), talosWorkers, worker1, 0, "spread", nil).Name)
	require.Equal(t, nodeBName, s.Pick(twoNodes(0), talosWorkers, worker2, 0, "spread", nil).Name)

	picked := s.Pick(twoNodes(1), talosWorkers, worker3, 0, "spread", map[string]struct{}{worker1: {}})

	require.Equal(t, nodeAName, picked.Name)
}

func TestSchedulerExpiresStaleReservations(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	s := provider.NewSchedulerWithClock(func() time.Time { return now }, time.Minute)

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 1.0},
			{Name: nodeBName, MemoryFree: 0.9},
		}
	}

	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker1, 0, "spread", nil).Name)

	now = now.Add(2 * time.Minute)

	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker2, 0, "spread", nil).Name)
}

func TestSchedulerReleaseFreesReservedNode(t *testing.T) {
	s := provider.NewScheduler()

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 1.0},
			{Name: nodeBName, MemoryFree: 0.9},
		}
	}

	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker1, 0, "spread", nil).Name)
	require.Equal(t, nodeBName, s.Pick(nodes(), talosWorkers, worker2, 0, "spread", nil).Name)

	s.Release(worker2)

	require.Equal(t, nodeBName, s.Pick(nodes(), talosWorkers, worker3, 0, "spread", nil).Name)
}

func TestSchedulerRoundRobinIgnoresMemory(t *testing.T) {
	s := provider.NewScheduler()

	// Memory order (c>b>a) is the inverse of name order, so a name-ordered
	// result proves round-robin ignores free memory.
	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, MemoryFree: 0.1},
			{Name: nodeBName, MemoryFree: 0.5},
			{Name: nodeCName, MemoryFree: 0.9},
		}
	}

	picked := make([]string, 0, 3)

	for _, requestID := range []string{worker1, worker2, worker3} {
		picked = append(picked, s.Pick(nodes(), talosWorkers, requestID, 0, "round-robin", nil).Name)
	}

	require.Equal(t, []string{nodeAName, nodeBName, nodeCName}, picked)
}

func TestSchedulerFewerVMsBalancesTotalLoad(t *testing.T) {
	s := provider.NewScheduler()

	nodes := []provider.NodeStatus{
		{Name: nodeAName, TotalVMs: 9, SameMachineRequestSetVMs: 0, MemoryFree: 0.5},
		{Name: nodeBName, TotalVMs: 1, SameMachineRequestSetVMs: 3, MemoryFree: 0.5},
	}

	require.Equal(t, nodeBName, s.Pick(nodes, talosWorkers, worker1, 0, "fewer-vms", nil).Name)
}

func TestSchedulerBinpackConsolidatesOntoNodesThatFit(t *testing.T) {
	s := provider.NewScheduler()

	nodes := func() []provider.NodeStatus {
		return []provider.NodeStatus{
			{Name: nodeAName, FreeMem: 100},
			{Name: nodeBName, FreeMem: 50},
		}
	}

	require.Equal(t, nodeBName, s.Pick(nodes(), talosWorkers, worker1, 40, "binpack", nil).Name)
	require.Equal(t, nodeAName, s.Pick(nodes(), talosWorkers, worker2, 60, "binpack", nil).Name)
}

func TestParseStrategy(t *testing.T) {
	for _, valid := range []string{"", "spread", "fewer-vms", "round-robin", "binpack"} {
		_, err := provider.ParseStrategy(valid)
		require.NoError(t, err)
	}

	_, err := provider.ParseStrategy("nonsense")
	require.Error(t, err)
}

func TestBuildFirmwareOptions(t *testing.T) {
	const vgaStd = "std"

	tests := []struct {
		expected map[string]any
		name     string
		data     provider.Data
	}{
		{
			name:     "No firmware or display options",
			expected: map[string]any{},
		},
		{
			name: "SeaBIOS with display option",
			data: provider.Data{
				Bios: "seabios",
				VGA:  vgaStd,
			},
			expected: map[string]any{
				"bios": "seabios",
				"vga":  vgaStd,
			},
		},
		{
			name: "OVMF creates EFI disk on selected storage",
			data: provider.Data{
				Bios: "ovmf",
				VGA:  vgaStd,
			},
			expected: map[string]any{
				"bios":     "ovmf",
				"efidisk0": "local-lvm:1,efitype=4m,pre-enrolled-keys=0",
				"vga":      vgaStd,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, provider.BuildFirmwareOptions(tt.data, "local-lvm"))
		})
	}
}

func TestBuildUSBDeviceOptions(t *testing.T) {
	devices := []provider.USBDevice{
		{Mapping: "rtl-sdr", USB3: true},
		{Mapping: "zigbee-controller"},
	}

	require.Equal(t, map[string]any{
		"usb0": "mapping=rtl-sdr,usb3=1",
		"usb1": "mapping=zigbee-controller",
	}, provider.BuildUSBDeviceOptions(devices))
}

func TestHandleStoppedStartTaskRetriesUnderMaxAttempts(t *testing.T) {
	spec := &specs.MachineSpec{
		VmStartTask:   "UPID:node:...:qmstart:",
		StartAttempts: provider.MaxStartAttempts - 2,
	}

	restart, err := provider.HandleStoppedStartTask(spec, "req-1")

	require.NoError(t, err)
	require.True(t, restart)
	require.Empty(t, spec.VmStartTask, "VmStartTask must be cleared so vm.Start() is reissued")
	require.EqualValues(t, provider.MaxStartAttempts-1, spec.StartAttempts)
}

func TestHandleStoppedStartTaskGivesUpAtMaxAttempts(t *testing.T) {
	spec := &specs.MachineSpec{
		VmStartTask:   "UPID:node:...:qmstart:",
		StartAttempts: provider.MaxStartAttempts - 1,
	}

	restart, err := provider.HandleStoppedStartTask(spec, "req-1")

	require.Error(t, err)
	require.False(t, restart)
	require.Contains(t, err.Error(), "giving up starting VM after")
	require.Equal(t, "UPID:node:...:qmstart:", spec.VmStartTask, "VmStartTask must not be cleared once giving up, no vm.Start() reissue")
}

func TestStartAttemptsExhaustedShortCircuitsRepeatedReconciles(t *testing.T) {
	spec := &specs.MachineSpec{
		VmStartTask:   "UPID:node:...:qmstart:",
		StartAttempts: provider.MaxStartAttempts - 1,
	}

	_, err := provider.HandleStoppedStartTask(spec, "req-1")
	require.Error(t, err, "first give-up must error")

	// A repeated reconcile (e.g. the resource gets requeued after the terminal error) must not
	// re-ping the now-stale VmStartTask or bump StartAttempts again - it must short-circuit.
	attemptsBefore := spec.StartAttempts
	err = provider.StartAttemptsExhausted(spec)

	require.Error(t, err)
	require.Contains(t, err.Error(), "giving up starting VM after")
	require.Equal(t, attemptsBefore, spec.StartAttempts, "a repeated reconcile must not increment StartAttempts again")
	require.Equal(t, "UPID:node:...:qmstart:", spec.VmStartTask, "the stale VmStartTask must not be touched by the short-circuit")
}

// TestStartVMStepEnforcesRetryCapAgainstRealProxmoxAPI drives the actual "startVM" provision
// step (not the internal helpers directly) against a fake Proxmox HTTP server whose start task
// always ends "stopped", repeating the step call the way the controller repeats a reconcile.
// This is the higher-level check requested on PR #83: it exercises the real getVM/CloudInit/
// vm.Start/checkTaskStatus HTTP round-trips together with StartAttempts persistence on the
// Machine resource, instead of trusting that the unit-level helpers reflect what the step does.
func TestStartVMStepEnforcesRetryCapAgainstRealProxmoxAPI(t *testing.T) {
	const (
		node = "pve1"
		vmid = 100
	)

	var (
		startCalls atomic.Int32
		taskSeq    atomic.Int32
	)

	newUPID := func(kind string) string {
		return fmt.Sprintf("UPID:%s:00000000:00000000:00000000:%s:%d:root@pam:", node, kind, taskSeq.Add(1))
	}

	mux := http.NewServeMux()

	mux.HandleFunc(fmt.Sprintf("/nodes/%s/status", node), func(w http.ResponseWriter, _ *http.Request) {
		writeData(t, w, map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		writeData(t, w, map[string]any{"vmid": vmid})
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeData(t, w, map[string]any{})

			return
		}

		writeData(t, w, newUPID("qmconfig")) // AddTag / device+boot patch, always succeeds
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/storage", node), func(w http.ResponseWriter, _ *http.Request) {
		writeData(t, w, []map[string]any{{"storage": "local", "content": "iso", "enabled": 1}})
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/storage/local/upload", node), func(w http.ResponseWriter, _ *http.Request) {
		writeData(t, w, newUPID("imgcopy")) // cloud-init ISO upload, always succeeds
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/status/start", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		startCalls.Add(1)
		writeData(t, w, newUPID("qmstart"))
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/", node), func(w http.ResponseWriter, r *http.Request) {
		// Real Proxmox echoes UPID/Node back on every status poll; the client's Task.Ping
		// re-decodes the response onto the same struct it polled with, so a stub omitting
		// them wipes the task's identity (Node included) after the first poll, sending the
		// next poll to a malformed "/nodes//tasks/..." URL. Echo both back here too.
		upid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/nodes/%s/tasks/", node)), "/status")

		if strings.Contains(upid, ":qmstart:") {
			writeData(t, w, map[string]any{"UPID": upid, "Node": node, "Status": "stopped", "IsRunning": false, "IsSuccessful": false, "IsFailed": true})

			return
		}

		writeData(t, w, map[string]any{"UPID": upid, "Node": node, "Status": "OK", "IsRunning": false, "IsSuccessful": true})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := provider.NewProvisioner(proxmox.NewClient(srv.URL))

	var startVMStep provision.Step[*resources.Machine]

	for _, step := range p.ProvisionSteps() {
		if step.Name() == "startVM" {
			startVMStep = step
		}
	}

	require.NotZero(t, startVMStep.Name(), "startVM step must exist")

	machineRequest := infra.NewMachineRequest("req-1")
	machine := resources.NewMachine("default", "req-1")
	machine.TypedSpec().Value.Node = node
	machine.TypedSpec().Value.Vmid = vmid
	machine.TypedSpec().Value.Uuid = "11111111-1111-1111-1111-111111111111"

	pctx := provision.NewContext[*resources.Machine](
		machineRequest,
		infra.NewMachineRequestStatus("req-1"),
		machine,
		provision.ConnectionParams{JoinConfig: "join-config"},
		nil,
		nil,
	)

	// Every reconcile until the cap must reissue vm.Start() after seeing the prior task
	// end "stopped"; the retry interval error from a fresh start is expected, not a failure.
	var lastErr error

	for i := 0; i < int(provider.MaxStartAttempts)+1; i++ {
		lastErr = startVMStep.Run(context.Background(), zap.NewNop(), pctx)
	}

	require.Error(t, lastErr, "must give up once the cap is hit")
	require.Contains(t, lastErr.Error(), "giving up starting VM after")
	require.EqualValues(t, provider.MaxStartAttempts, startCalls.Load(),
		"vm.Start() must be issued exactly MaxStartAttempts times, never once the cap is persisted")
	require.EqualValues(t, provider.MaxStartAttempts, machine.TypedSpec().Value.StartAttempts,
		"StartAttempts must be persisted on the resource, not just held in a local variable")

	// A further reconcile after the terminal error must short-circuit: no additional
	// vm.Start() call and no re-poll of the dead task incrementing StartAttempts again.
	callsBefore := startCalls.Load()
	err := startVMStep.Run(context.Background(), zap.NewNop(), pctx)

	require.Error(t, err)
	require.Contains(t, err.Error(), "giving up starting VM after")
	require.Equal(t, callsBefore, startCalls.Load())
	require.EqualValues(t, provider.MaxStartAttempts, machine.TypedSpec().Value.StartAttempts)
}
