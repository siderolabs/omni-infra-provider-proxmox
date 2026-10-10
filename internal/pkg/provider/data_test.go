// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
)

// schemaPath is the machine-class schema embedded into the provider binary; it
// is what renders the fields in the Omni UI.
const schemaPath = "../../../cmd/omni-infra-provider-proxmox/data/schema.json"

// TestProviderDataYAML parses providerdata exactly as the Omni SDK does
// (go.yaml.in/yaml/v4), so a wrong or stale `yaml:` tag fails here rather than
// silently leaving a field at its zero value on a live provision.
func TestProviderDataYAML(t *testing.T) {
	const providerData = `
storage_selector: name == "diskfs"
network_bridge: prodapp
vlan: 0
cores: 4
memory: 8192
disk_size: 40
vmid_range: 102101-102110
network_subnet: 10.80.2.0/24
network_gateway: 10.80.2.1
network_nameservers:
  - 10.84.3.10
  - 10.84.3.20
network_base_ip: 10.80.2.101
network_mtu: 1450
`

	var data provider.Data

	require.NoError(t, yaml.Unmarshal([]byte(providerData), &data))

	require.Equal(t, "10.80.2.0/24", data.NetworkSubnet)
	require.Equal(t, "10.80.2.1", data.NetworkGateway)
	require.Equal(t, []string{"10.84.3.10", "10.84.3.20"}, data.NetworkNameservers)
	require.Equal(t, "10.80.2.101", data.NetworkBaseIP)
	require.Equal(t, 1450, data.NetworkMTU)
	require.Equal(t, "102101-102110", data.VMIDRange)
	require.Empty(t, data.NetworkIP)

	require.NoError(t, provider.ValidateNetwork(data))

	// vmid 102105 is 4 past the start of the range, so .101 + 4.
	got, err := provider.BuildNetworkConfig(data, 102105, "BC:24:11:AA:BB:CC")
	require.NoError(t, err)

	require.Equal(t, `version: 1
config:
  - type: physical
    mac_address: bc:24:11:aa:bb:cc
    mtu: 1450
    subnets:
      - type: static
        address: 10.80.2.105/24
        gateway: 10.80.2.1
  - type: nameserver
    address:
      - 10.84.3.10
      - 10.84.3.20
`, got)
}

// TestProviderDataYAMLDefaultsToDHCP guards the one behavioral promise of the
// static-networking feature: providerdata without any network_* field must
// leave the VM on DHCP.
func TestProviderDataYAMLDefaultsToDHCP(t *testing.T) {
	var data provider.Data

	require.NoError(t, yaml.Unmarshal([]byte("cores: 4\nmemory: 8192\n"), &data))

	require.Empty(t, data.NetworkSubnet)
	require.NoError(t, provider.ValidateNetwork(data))
}

// TestSchemaMatchesData keeps the embedded machine-class schema and the Data
// struct from drifting: a field the UI offers but the provider never reads
// (or the reverse) is invisible until someone sets it on a live machine class.
func TestSchemaMatchesData(t *testing.T) {
	raw, err := os.ReadFile(schemaPath)
	require.NoError(t, err)

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}

	require.NoError(t, json.Unmarshal(raw, &schema))

	tags := map[string]struct{}{}

	dataType := reflect.TypeOf(provider.Data{})
	for i := range dataType.NumField() {
		name, _, _ := strings.Cut(dataType.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			tags[name] = struct{}{}
		}
	}

	for name := range schema.Properties {
		_, ok := tags[name]
		require.True(t, ok, "schema.json offers %q but Data has no matching yaml tag", name)
	}

	for name := range tags {
		_, ok := schema.Properties[name]
		require.True(t, ok, "Data accepts %q but schema.json does not declare it", name)
	}
}
