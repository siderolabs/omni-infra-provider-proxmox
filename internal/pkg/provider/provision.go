// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements Proxmox infra provider core.
package provider

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/uuid"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/constants"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	siderocel "github.com/siderolabs/talos/pkg/machinery/cel"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const machineRequestTagPrefix = "machine-request."

// Provisioner implements Talos emulator infra provider.
type Provisioner struct {
	proxmoxClient *proxmox.Client
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(proxmoxClient *proxmox.Client) *Provisioner {
	return &Provisioner{
		proxmoxClient: proxmoxClient,
	}
}

// ProvisionSteps implements infra.Provisioner.
//
//nolint:gocognit,gocyclo,cyclop,maintidx
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("pickNode", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			var data Data

			if err := pctx.UnmarshalProviderData(&data); err != nil {
				return err
			}

			nodes, err := p.proxmoxClient.Nodes(ctx)
			if err != nil {
				return err
			}

			if len(nodes) == 0 {
				return fmt.Errorf("no nodes available")
			}

			// If user specified a node, validate and use it
			if data.Node != "" {
				for _, node := range nodes {
					if node.Node == data.Node {
						if node.Status != "online" {
							return fmt.Errorf("specified node %q is not online (status: %s)", data.Node, node.Status)
						}

						pctx.State.TypedSpec().Value.Node = data.Node

						logger.Info("using configured node for the Proxmox VM", zap.String("node", data.Node))

						return nil
					}
				}

				return fmt.Errorf("specified node %q not found in cluster", data.Node)
			}

			nodeInfoList := make([]nodeStatus, 0, len(nodes))

			for _, node := range nodes {
				var ns nodeStatus

				ns.Name = node.Node
				ns.MemoryFree = float64(node.MaxMem-node.Mem) / float64(node.MaxMem)

				if machineRequestSet, ok := pctx.GetMachineRequestSetID(); ok {
					n, err := p.proxmoxClient.Node(ctx, node.Node)
					if err != nil {
						return fmt.Errorf("failed to get node %q, %w", node.Node, err)
					}

					vms, err := n.VirtualMachines(ctx)
					if err != nil {
						return fmt.Errorf("failed to get vms for now %q, %w", node.Node, err)
					}

					for _, vm := range vms {
						if vm.HasTag(machineRequestTagPrefix + machineRequestSet) {
							ns.SameMachineRequestSetVMs += 1
						}
					}
				}

				nodeInfoList = append(nodeInfoList, ns)
			}

			pickedNode := pickNode(nodeInfoList)

			pctx.State.TypedSpec().Value.Node = pickedNode.Name

			logger.Info("auto-selected node for the Proxmox VM", zap.String("node", pickedNode.Name))

			return nil
		}),
		provision.NewStep("createSchematic", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			// generating schematic with join configs as it's going to be used in the ISO image which doesn't support partial configs
			schematic, err := pctx.GenerateSchematicID(ctx, logger,
				provision.WithExtraExtensions("siderolabs/qemu-guest-agent"),
				provision.WithoutConnectionParams(),
			)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.Schematic = schematic

			return nil
		}),
		provision.NewStep("uploadISO", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.VolumeUploadTask != "" {
				err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VolumeUploadTask)
				if err != nil && err.Error() != "stopped" {
					return err
				}

				if err == nil {
					return nil
				}

				logger.Info("retrying download")
			}

			pctx.State.TypedSpec().Value.TalosVersion = pctx.GetTalosVersion()

			url, err := url.Parse(constants.ImageFactoryBaseURL)
			if err != nil {
				return err
			}

			var data Data

			err = pctx.UnmarshalProviderData(&data)
			if err != nil {
				return err
			}

			url = url.JoinPath("image",
				pctx.State.TypedSpec().Value.Schematic,
				pctx.GetTalosVersion(),
				"nocloud-amd64.iso",
			)

			hash := sha256.New()

			if _, err = hash.Write([]byte(url.String())); err != nil {
				return err
			}

			isoName := hex.EncodeToString(hash.Sum(nil)) + ".iso"

			pctx.State.TypedSpec().Value.VolumeId = isoName

			node, err := p.proxmoxClient.Node(ctx, pctx.State.TypedSpec().Value.Node)
			if err != nil {
				return err
			}

			var storage *proxmox.Storage

			storage, err = node.StorageISO(ctx)
			if err != nil {
				return fmt.Errorf("failed to get storage: %w", err)
			}

			_, err = storage.ISO(ctx, isoName)
			// Already downloaded
			// TODO: figure out a better way to check the errors
			if err == nil {
				return nil
			}

			task, err := storage.DownloadURL(ctx, "iso", isoName, url.String())
			if err != nil {
				return err
			}

			logger.Info("uploading new ISO image", zap.String("volumeID", isoName), zap.String("task", string(task.UPID)))

			pctx.State.TypedSpec().Value.VolumeUploadTask = string(task.UPID)

			return provision.NewRetryInterval(time.Second)
		}),
		provision.NewStep("syncVM", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.VmCreateTask != "" {
				err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VmCreateTask)
				if err != nil {
					return err
				}

				return nil
			}

			if pctx.State.TypedSpec().Value.Uuid == "" {
				pctx.State.TypedSpec().Value.Uuid = uuid.NewString()
				pctx.SetMachineUUID(pctx.State.TypedSpec().Value.Uuid)
			}

			var data Data

			err := pctx.UnmarshalProviderData(&data)
			if err != nil {
				return err
			}

			node, err := p.proxmoxClient.Node(ctx, pctx.State.TypedSpec().Value.Node)
			if err != nil {
				return err
			}

			cluster, err := p.proxmoxClient.Cluster(ctx)
			if err != nil {
				return err
			}

			vmid, err := cluster.NextID(ctx)
			if err != nil {
				return err
			}

			isoStorage, err := node.StorageISO(ctx)
			if err != nil {
				return err
			}

			iso, err := isoStorage.ISO(ctx, pctx.State.TypedSpec().Value.VolumeId)
			if err != nil {
				return err
			}

			if data.NetworkBridge == "" {
				data.NetworkBridge = "vmbr0"
			}

			// Parse out the network config
			var networkString string
			if data.Vlan == 0 {
				networkString = fmt.Sprintf("virtio,bridge=%s,firewall=1", data.NetworkBridge)
			} else {
				networkString = fmt.Sprintf("virtio,bridge=%s,firewall=1,tag=%d", data.NetworkBridge, data.Vlan)
			}

			// Build primary disk options
			selectedStorage, err := p.pickStorage(ctx, node, data.StorageSelector)
			if err != nil {
				return err
			}

			diskOptions := []string{fmt.Sprintf("%s:%d", selectedStorage, data.DiskSize)}
			if data.DiskSSD {
				diskOptions = append(diskOptions, "ssd=1")
			}

			if data.DiskDiscard {
				diskOptions = append(diskOptions, "discard=on")
			}

			if data.DiskIOThread {
				diskOptions = append(diskOptions, "iothread=1")
			}

			if data.DiskCache != "" {
				diskOptions = append(diskOptions, fmt.Sprintf("cache=%s", data.DiskCache))
			}

			if data.DiskAIO != "" {
				diskOptions = append(diskOptions, fmt.Sprintf("aio=%s", data.DiskAIO))
			}

			diskString := strings.Join(diskOptions, ",")

			// Determine CPU type (default to x86-64-v2-AES for compatibility)
			cpuType := "x86-64-v2-AES"
			if data.CPUType != "" {
				cpuType = data.CPUType
			}

			// Build VM options
			vmOptions := []proxmox.VirtualMachineOption{
				{
					Name:  "smbios1",
					Value: "uuid=" + pctx.State.TypedSpec().Value.Uuid,
				},
				{
					Name:  "name",
					Value: pctx.GetRequestID(),
				},
				{
					Name:  "cdrom",
					Value: iso.VolID,
				},
				{
					Name:  "cpu",
					Value: cpuType,
				},
				{
					Name:  "cores",
					Value: data.Cores,
				},
				{
					Name:  "sockets",
					Value: data.Sockets,
				},
				{
					Name:  "memory",
					Value: data.Memory,
				},
				{
					Name:  "scsi0",
					Value: diskString,
				},
				{
					Name:  "scsihw",
					Value: "virtio-scsi-single",
				},
				{
					Name:  "onboot",
					Value: 1,
				},
				{
					Name:  "net0",
					Value: networkString,
				},
				{
					Name:  "agent",
					Value: "enabled=true",
				},
			}

			if machineRequestSet, ok := pctx.GetMachineRequestSetID(); ok {
				vmOptions = append(vmOptions,
					proxmox.VirtualMachineOption{
						Name:  "tags",
						Value: machineRequestTagPrefix + machineRequestSet,
					},
				)
			}

			// Primary disk is always scsi0. Additional disks start from scsi1.
			for i, disk := range data.AdditionalDisks {
				var storage string

				storage, err = p.pickStorage(ctx, node, disk.StorageSelector)
				if err != nil {
					return fmt.Errorf("failed to pick storage for additional disk %d: %w", i+1, err)
				}

				opts := []string{fmt.Sprintf("%s:%d", storage, disk.DiskSize)}
				if disk.DiskSSD {
					opts = append(opts, "ssd=1")
				}

				if disk.DiskDiscard {
					opts = append(opts, "discard=on")
				}

				if disk.DiskIOThread {
					opts = append(opts, "iothread=1")
				}

				if disk.DiskCache != "" {
					opts = append(opts, fmt.Sprintf("cache=%s", disk.DiskCache))
				}

				if disk.DiskAIO != "" {
					opts = append(opts, fmt.Sprintf("aio=%s", disk.DiskAIO))
				}

				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  fmt.Sprintf("scsi%d", i+1),
					Value: strings.Join(opts, ","),
				})
			}

			// Add machine type if specified (q35 for GPU passthrough)
			if data.MachineType != "" {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "machine",
					Value: data.MachineType,
				})
			}

			// Add NUMA if enabled
			if data.NUMA {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "numa",
					Value: 1,
				})
			}

			// Add hugepages if specified (Proxmox expects: "any", "2" for 2MB, "1024" for 1GB)
			if data.Hugepages != "" {
				var hugepagesStr string

				switch data.Hugepages {
				case "2MB", "2":
					hugepagesStr = "2"
				case "1GB", "1024":
					hugepagesStr = "1024"
				case "any":
					hugepagesStr = "any"
				default:
					hugepagesStr = data.Hugepages
				}

				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "hugepages",
					Value: hugepagesStr,
				})
			}

			// Disable balloon if explicitly set to false (for GPU/hugepages)
			if data.Balloon != nil && !*data.Balloon {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "balloon",
					Value: 0,
				})
			}

			// Add additional NICs for storage/backup networks
			for i, nic := range data.AdditionalNICs {
				var nicString string

				firewallVal := 0
				if nic.Firewall {
					firewallVal = 1
				}

				if nic.Vlan == 0 {
					nicString = fmt.Sprintf("virtio,bridge=%s,firewall=%d", nic.Bridge, firewallVal)
				} else {
					nicString = fmt.Sprintf("virtio,bridge=%s,firewall=%d,tag=%d", nic.Bridge, firewallVal, nic.Vlan)
				}

				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  fmt.Sprintf("net%d", i+1), // net1, net2, etc.
					Value: nicString,
				})
			}

			// Add PCI device passthrough using Resource Mappings
			for i, pci := range data.PCIDevices {
				var pciParts []string

				pciParts = append(pciParts, fmt.Sprintf("mapping=%s", pci.Mapping))
				if pci.PCIExpress {
					pciParts = append(pciParts, "pcie=1")
				}

				if pci.PrimaryGPU {
					pciParts = append(pciParts, "x-vga=1")
				}

				if pci.ROMBar {
					pciParts = append(pciParts, "rombar=1")
				}

				pciString := strings.Join(pciParts, ",")
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  fmt.Sprintf("hostpci%d", i), // hostpci0, hostpci1, etc.
					Value: pciString,
				})
			}

			task, err := node.NewVirtualMachine(ctx, vmid, vmOptions...)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.VmCreateTask = string(task.UPID)
			pctx.State.TypedSpec().Value.Vmid = int32(vmid)

			return provision.NewRetryInterval(time.Second * 10)
		}),
		provision.NewStep("startVM", func(ctx context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.VmStartTask != "" {
				if err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VmStartTask); err != nil {
					return err
				}
			} else {
				vm, err := p.getVM(ctx, pctx.State.TypedSpec().Value.Node, pctx.State.TypedSpec().Value.Vmid)
				if err != nil {
					return err
				}

				err = vm.CloudInit(ctx,
					"ide0",
					pctx.ConnectionParams.JoinConfig,
					fmt.Sprintf(`instance-id: %s
local-hostname: %s
hostname: %s`,
						pctx.State.TypedSpec().Value.Uuid,
						pctx.GetRequestID(),
						pctx.GetRequestID(),
					),
					"",
					"version: 1",
				)
				if err != nil {
					return fmt.Errorf("failed to inject nocloud config: %w", err)
				}

				task, err := vm.Start(ctx)
				if err != nil {
					return err
				}

				pctx.State.TypedSpec().Value.VmStartTask = string(task.UPID)

				return provision.NewRetryInterval(time.Second * 1)
			}

			return nil
		}),
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, machineRequest *infra.MachineRequest) error {
	if machine.TypedSpec().Value.Vmid == 0 {
		return nil
	}

	if machine.TypedSpec().Value.Node == "" {
		return errors.New("VM is missing the node information")
	}

	vm, err := p.getVM(ctx, machine.TypedSpec().Value.Node, machine.TypedSpec().Value.Vmid)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil
		}

		return err
	}

	task, err := vm.Stop(ctx)
	if err != nil {
		return err
	}

	if err = p.waitForTaskToFinish(ctx, task); err != nil {
		return err
	}

	task, err = vm.Delete(ctx)
	if err != nil {
		return err
	}

	if err = p.waitForTaskToFinish(ctx, task); err != nil {
		return err
	}

	return nil
}

func (p *Provisioner) pickStorage(ctx context.Context, node *proxmox.Node, selector string) (string, error) {
	storages, err := node.Storages(ctx)
	if err != nil {
		return "", err
	}

	for _, storage := range storages {
		env, err := cel.NewEnv(
			cel.Variable("name", cel.StringType),
			cel.Variable("node", cel.StringType),
			cel.Variable("storageType", cel.StringType),
			cel.Variable("availableSpace", cel.UintType),
		)
		if err != nil {
			return "", err
		}

		expr, err := siderocel.ParseBooleanExpression(selector, env)
		if err != nil {
			return "", err
		}

		matched, err := expr.EvalBool(env, map[string]any{
			"name":           storage.Name,
			"node":           node.Name,
			"storageType":    storage.Type,
			"availableSpace": storage.Avail,
		})
		if err != nil {
			return "", err
		}

		if matched {
			return storage.Name, nil
		}
	}

	return "", fmt.Errorf("failed to pick the disk: no matches for the condition %q", selector)
}

func (p *Provisioner) getVM(ctx context.Context, nodeName string, vmid int32) (*proxmox.VirtualMachine, error) {
	node, err := p.proxmoxClient.Node(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	return node.VirtualMachine(ctx, int(vmid))
}

func (p *Provisioner) checkTaskStatus(ctx context.Context, id string) error {
	t := proxmox.NewTask(proxmox.UPID(id), p.proxmoxClient)

	if err := t.Ping(ctx); err != nil {
		return err
	}

	switch {
	case t.IsRunning:
		return provision.NewRetryInterval(time.Second * 10)
	case t.IsSuccessful:
		return nil
	}

	return errors.New(t.Status)
}

func (p *Provisioner) waitForTaskToFinish(ctx context.Context, t *proxmox.Task) error {
	ticker := time.NewTicker(time.Second * 5)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := t.Ping(ctx); err != nil {
				return err
			}

			switch {
			case t.IsFailed:
				return errors.New(t.Status)
			case t.IsSuccessful:
				return nil
			}
		}
	}
}

type nodeStatus struct {
	Name                     string
	MemoryFree               float64
	SameMachineRequestSetVMs int
}

func pickNode(nodeInfoList []nodeStatus) nodeStatus {
	// Auto-pick node with most free memory and with the least number of machines from the same machine request set
	slices.SortFunc(nodeInfoList, func(a, b nodeStatus) int {
		if c := cmp.Compare(a.SameMachineRequestSetVMs, b.SameMachineRequestSetVMs); c != 0 {
			return c
		}

		return -cmp.Compare(a.MemoryFree, b.MemoryFree)
	})

	return nodeInfoList[0]
}
