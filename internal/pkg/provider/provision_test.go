// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"
	"testing"
)

func TestGenerateHostnameConfig(t *testing.T) {
	tests := []struct {
		name         string
		talosVersion string
		hostname     string
		wantFormat   string // "hostnameconfig" or "legacy"
	}{
		{
			name:         "Talos 1.12.0 uses HostnameConfig",
			talosVersion: "v1.12.0",
			hostname:     "worker-01",
			wantFormat:   "hostnameconfig",
		},
		{
			name:         "Talos 1.12.2 uses HostnameConfig",
			talosVersion: "v1.12.2",
			hostname:     "control-plane-01",
			wantFormat:   "hostnameconfig",
		},
		{
			name:         "Talos 1.13.0 uses HostnameConfig",
			talosVersion: "v1.13.0",
			hostname:     "node-1",
			wantFormat:   "hostnameconfig",
		},
		{
			name:         "Talos 1.11.6 uses legacy format",
			talosVersion: "v1.11.6",
			hostname:     "worker-01",
			wantFormat:   "legacy",
		},
		{
			name:         "Talos 1.11.0 uses legacy format",
			talosVersion: "v1.11.0",
			hostname:     "control-plane-01",
			wantFormat:   "legacy",
		},
		{
			name:         "Talos 1.10.0 uses legacy format",
			talosVersion: "v1.10.0",
			hostname:     "node-1",
			wantFormat:   "legacy",
		},
		{
			name:         "version without v prefix works for 1.12+",
			talosVersion: "1.12.0",
			hostname:     "worker-01",
			wantFormat:   "hostnameconfig",
		},
		{
			name:         "version without v prefix works for legacy",
			talosVersion: "1.11.6",
			hostname:     "worker-01",
			wantFormat:   "legacy",
		},
		{
			name:         "invalid version defaults to HostnameConfig",
			talosVersion: "invalid",
			hostname:     "worker-01",
			wantFormat:   "hostnameconfig",
		},
		{
			name:         "empty version defaults to HostnameConfig",
			talosVersion: "",
			hostname:     "worker-01",
			wantFormat:   "hostnameconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(GenerateHostnameConfig(tt.talosVersion, tt.hostname))

			switch tt.wantFormat {
			case "hostnameconfig":
				if !strings.Contains(got, "apiVersion: v1alpha1") {
					t.Errorf("expected HostnameConfig format with apiVersion, got:\n%s", got)
				}
				if !strings.Contains(got, "kind: HostnameConfig") {
					t.Errorf("expected HostnameConfig format with kind, got:\n%s", got)
				}
				if !strings.Contains(got, "hostname: "+tt.hostname) {
					t.Errorf("expected hostname %q in config, got:\n%s", tt.hostname, got)
				}
				if strings.Contains(got, "machine:") {
					t.Errorf("HostnameConfig format should not contain 'machine:', got:\n%s", got)
				}
			case "legacy":
				if !strings.Contains(got, "machine:") {
					t.Errorf("expected legacy format with 'machine:', got:\n%s", got)
				}
				if !strings.Contains(got, "network:") {
					t.Errorf("expected legacy format with 'network:', got:\n%s", got)
				}
				if !strings.Contains(got, "hostname: "+tt.hostname) {
					t.Errorf("expected hostname %q in config, got:\n%s", tt.hostname, got)
				}
				if strings.Contains(got, "HostnameConfig") {
					t.Errorf("legacy format should not contain 'HostnameConfig', got:\n%s", got)
				}
			}
		})
	}
}
