// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
)

func TestPickNode(t *testing.T) {
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
				{Name: "NodeA", MemoryFree: 1.0, SameMachineRequestSetVMs: 10},
				// Node B has less memory, but is free (0 VMs) -> Should win
				{Name: "NodeB", MemoryFree: 0.5, SameMachineRequestSetVMs: 0},
			},
			expected: "NodeB",
		},
		{
			name: "Secondary criteria: If VMs equal, pick node with MOST free memory",
			input: []provider.NodeStatus{
				{Name: "NodeA", MemoryFree: 0.5, SameMachineRequestSetVMs: 5},
				{Name: "NodeB", MemoryFree: 1.0, SameMachineRequestSetVMs: 5}, // More memory
				{Name: "NodeC", MemoryFree: 0.1, SameMachineRequestSetVMs: 5},
			},
			expected: "NodeB",
		},
		{
			name: "Complex scenario",
			input: []provider.NodeStatus{
				{Name: "NodeA", MemoryFree: 0.1, SameMachineRequestSetVMs: 2},
				{Name: "NodeB", MemoryFree: 0.05, SameMachineRequestSetVMs: 1}, // Best VM count
				{Name: "NodeC", MemoryFree: 0.04, SameMachineRequestSetVMs: 1}, // Same VM count, less memory than B
				{Name: "NodeD", MemoryFree: 1, SameMachineRequestSetVMs: 5},
			},
			expected: "NodeB",
		},
		{
			name: "No free memory",
			input: []provider.NodeStatus{
				{Name: "NodeA", MemoryFree: 0, SameMachineRequestSetVMs: 0},
				{Name: "NodeB", MemoryFree: 1, SameMachineRequestSetVMs: 0},
			},
			expected: "NodeB",
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
