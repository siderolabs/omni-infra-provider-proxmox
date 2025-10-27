// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

// Data is the provider custom machine config.
type Data struct {
	Node            string `yaml:"node,omitempty"`
	StorageSelector string `yaml:"storage_selector,omitempty"`
	NetworkBridge   string `yaml:"network_bridge"`
	Cores           int    `yaml:"cores"`
	DiskSize        int    `yaml:"disk_size"`
	Sockets         int    `yaml:"sockets"`
	Memory          uint64 `yaml:"memory"`
	Vlan            uint64 `yaml:"vlan"`
}
