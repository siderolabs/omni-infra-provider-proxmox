// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

// Data is the provider custom machine config.
type Data struct {
	Balloon         *bool            `yaml:"balloon,omitempty"`
	Node            string           `yaml:"node,omitempty"`
	StorageSelector string           `yaml:"storage_selector,omitempty"`
	NetworkBridge   string           `yaml:"network_bridge"`
	Hugepages       string           `yaml:"hugepages,omitempty"`
	MachineType     string           `yaml:"machine_type,omitempty"`
	CPUType         string           `yaml:"cpu_type,omitempty"`
	DiskAIO         string           `yaml:"disk_aio,omitempty"`
	DiskCache       string           `yaml:"disk_cache,omitempty"`
	AdditionalDisks []AdditionalDisk `yaml:"additional_disks,omitempty"`
	AdditionalNICs  []AdditionalNIC  `yaml:"additional_nics,omitempty"`
	PCIDevices      []PCIDevice      `yaml:"pci_devices,omitempty"`
	Vlan            uint64           `yaml:"vlan"`
	Memory          uint64           `yaml:"memory"`
	Sockets         int              `yaml:"sockets"`
	DiskSize        int              `yaml:"disk_size"`
	Cores           int              `yaml:"cores"`
	DiskIOThread    bool             `yaml:"disk_iothread,omitempty"`
	NUMA            bool             `yaml:"numa,omitempty"`
	DiskDiscard     bool             `yaml:"disk_discard,omitempty"`
	DiskSSD         bool             `yaml:"disk_ssd,omitempty"`
}

// AdditionalDisk represents an additional disk configuration.
type AdditionalDisk struct {
	StorageSelector string `yaml:"storage_selector"`
	DiskCache       string `yaml:"disk_cache,omitempty"`
	DiskAIO         string `yaml:"disk_aio,omitempty"`
	DiskSize        int    `yaml:"disk_size"`
	DiskSSD         bool   `yaml:"disk_ssd,omitempty"`
	DiskDiscard     bool   `yaml:"disk_discard,omitempty"`
	DiskIOThread    bool   `yaml:"disk_iothread,omitempty"`
}

// PCIDevice represents a PCI device passthrough configuration using Proxmox Resource Mappings.
type PCIDevice struct {
	Mapping    string `yaml:"mapping"`               // Resource mapping name (e.g., nvidia-gpu-1)
	PCIExpress bool   `yaml:"pcie,omitempty"`        // Use PCIe instead of PCI (recommended for GPUs)
	PrimaryGPU bool   `yaml:"primary_gpu,omitempty"` // Set as primary GPU (x-vga=1)
	ROMBar     bool   `yaml:"rombar,omitempty"`      // Enable ROM BAR (default true, set false to disable)
}

// AdditionalNIC represents an additional network interface configuration.
type AdditionalNIC struct {
	Bridge   string `yaml:"bridge"`             // Network bridge (e.g., vmbr1)
	Vlan     uint64 `yaml:"vlan,omitempty"`     // Optional VLAN tag
	Firewall bool   `yaml:"firewall,omitempty"` // Enable firewall (default: false for storage networks)
}
