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
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/uuid"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/constants"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	siderocel "github.com/siderolabs/talos/pkg/machinery/cel"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/api/specs"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/ha"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const (
	machineRequestTagPrefix = "machine-request."
	poolComment             = "managed by omni-infra-provider-proxmox"
	maxStartAttempts        = 5 // stop retrying vm.Start() after this many "stopped" tasks
)

// Provisioner implements Talos emulator infra provider.
type Provisioner struct {
	ha                    *ha.Manager
	proxmoxClient         *proxmox.Client
	scheduler             *scheduler
	pendingISODownloads   map[string]string
	pendingISODownloadsMu sync.Mutex
	poolMu                sync.Mutex
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(proxmoxClient *proxmox.Client) *Provisioner {
	return &Provisioner{
		proxmoxClient:       proxmoxClient,
		scheduler:           newScheduler(),
		ha:                  ha.NewManager(proxmoxClient),
		pendingISODownloads: make(map[string]string),
	}
}

// ProvisionSteps implements infra.Provisioner.
//
//nolint:gocognit,gocyclo,cyclop,maintidx
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("pickNode", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.Node != "" {
				return nil
			}

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

			strategy, err := parseStrategy(data.PlacementStrategy)
			if err != nil {
				return err
			}

			machineRequestSet, inSet := pctx.GetMachineRequestSetID()

			nodeInfoList := make([]nodeStatus, 0, len(nodes))
			materialized := map[string]struct{}{}

			for _, node := range nodes {
				// Skip nodes that are not online to avoid Proxmox API errors
				if node.Status != "online" {
					logger.Debug("skipping offline node", zap.String("node", node.Node), zap.String("status", node.Status))

					continue
				}

				var ns nodeStatus

				ns.Name = node.Node

				// MaxMem/Mem are unsigned; guard the subtraction in case Proxmox ever
				// reports used memory above the node total, and avoid div-by-zero.
				if node.MaxMem > node.Mem {
					ns.FreeMem = node.MaxMem - node.Mem
				}

				if node.MaxMem > 0 {
					ns.MemoryFree = float64(ns.FreeMem) / float64(node.MaxMem)
				}

				if shouldCountSetVMs(data, inSet) {
					n, err := p.proxmoxClient.Node(ctx, node.Node)
					if err != nil {
						return fmt.Errorf("failed to get node %q, %w", node.Node, err)
					}

					vms, err := n.VirtualMachines(ctx)
					if err != nil {
						return fmt.Errorf("failed to get vms for node %q, %w", node.Node, err)
					}

					ns.TotalVMs = len(vms)

					for _, vm := range vms {
						if vm.HasTag(machineRequestTagPrefix + machineRequestSet) {
							ns.SameMachineRequestSetVMs++
							materialized[vm.Name] = struct{}{}
						}
					}
				}

				nodeInfoList = append(nodeInfoList, ns)
			}

			if len(nodeInfoList) == 0 {
				return fmt.Errorf("no online nodes available for provisioning")
			}

			var pickedNode nodeStatus

			if inSet {
				pickedNode = p.scheduler.pick(nodeInfoList, machineRequestSet, pctx.GetRequestID(), data.Memory*1024*1024, strategy, materialized)
			} else {
				pickedNode = pickNode(nodeInfoList)
			}

			pctx.State.TypedSpec().Value.Node = pickedNode.Name

			logger.Info("auto-selected node for the Proxmox VM", zap.String("node", pickedNode.Name))

			return nil
		}),
		provision.NewStep("createSchematic", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			// generating schematic with join configs as it's going to be used in the ISO image which doesn't support partial configs
			schematic, err := pctx.GenerateSchematicID(
				ctx, logger,
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

			url = url.JoinPath(
				"image",
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

			p.pendingISODownloadsMu.Lock()
			defer p.pendingISODownloadsMu.Unlock()

			isoUploadID := fmt.Sprintf("%s-%s", node.Name, isoName)

			_, err = storage.ISO(ctx, isoName)
			// Already downloaded
			// TODO: figure out a better way to check the errors
			if err == nil {
				delete(p.pendingISODownloads, isoUploadID)

				return nil
			}

			if existing, ok := p.pendingISODownloads[isoUploadID]; ok {
				if p.isTaskLive(ctx, existing) {
					logger.Info("ISO image is already being downloaded, reusing the existing task", zap.String("volumeID", isoName), zap.String("task", existing))

					pctx.State.TypedSpec().Value.VolumeUploadTask = existing

					return provision.NewRetryInterval(time.Second)
				}

				// The previously tracked download is no longer running and the ISO never
				// landed, so drop the stale entry and fall through to start a fresh
				// download instead of pinning every machine to a dead task.
				logger.Info("previous ISO download task is no longer running, restarting download", zap.String("volumeID", isoName), zap.String("task", existing))

				delete(p.pendingISODownloads, isoUploadID)
			}

			task, err := storage.DownloadURL(ctx, "iso", isoName, url.String())
			if err != nil {
				return err
			}

			logger.Info("uploading new ISO image", zap.String("volumeID", isoName), zap.String("task", string(task.UPID)))

			pctx.State.TypedSpec().Value.VolumeUploadTask = string(task.UPID)

			p.pendingISODownloads[isoUploadID] = string(task.UPID)

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

			// network_firewall defaults to true for backward-compatibility
			// with the previous hardcoded behavior. Set false in providerdata
			// to opt out when L2-broadcast services need traffic to bypass
			// the per-VM fwbr bridge.
			primaryFirewall := 1
			if data.NetworkFirewall != nil && !*data.NetworkFirewall {
				primaryFirewall = 0
			}

			// Parse out the network config
			var networkString string
			if data.Vlan == 0 {
				networkString = fmt.Sprintf("virtio,bridge=%s,firewall=%d", data.NetworkBridge, primaryFirewall)
			} else {
				networkString = fmt.Sprintf("virtio,bridge=%s,firewall=%d,tag=%d", data.NetworkBridge, primaryFirewall, data.Vlan)
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

			machineRequestSet, _ := pctx.GetMachineRequestSetID()
			if value, ok := buildTagsOption(data.Tags, machineRequestSet); ok {
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "tags",
					Value: value,
				})
			}

			if value := cmp.Or(data.Pool, machineRequestSet); value != "" {
				p.poolMu.Lock()
				defer p.poolMu.Unlock()

				if err = p.ensurePool(ctx, logger, value, machineRequestSet); err != nil {
					return err
				}

				pctx.State.TypedSpec().Value.Pool = value

				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  "pool",
					Value: value,
				})
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

			vmOptions = append(vmOptions, buildFirmwareOptions(data, selectedStorage)...)

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

				if pci.MDev != "" {
					pciParts = append(pciParts, fmt.Sprintf("mdev=%s", pci.MDev))
				}

				pciString := strings.Join(pciParts, ",")
				vmOptions = append(vmOptions, proxmox.VirtualMachineOption{
					Name:  fmt.Sprintf("hostpci%d", i), // hostpci0, hostpci1, etc.
					Value: pciString,
				})
			}

			vmOptions = append(vmOptions, buildUSBDeviceOptions(data.USBDevices)...)

			task, err := node.NewVirtualMachine(ctx, vmid, vmOptions...)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.VmCreateTask = string(task.UPID)
			pctx.State.TypedSpec().Value.Vmid = int32(vmid)

			return provision.NewRetryInterval(time.Second * 10)
		}),
		provision.NewStep("startVM", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if err := startAttemptsExhausted(pctx.State.TypedSpec().Value); err != nil {
				return err
			}

			if pctx.State.TypedSpec().Value.VmStartTask != "" {
				err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VmStartTask)
				if err == nil {
					return nil
				}

				if err.Error() != "stopped" {
					return err
				}

				if _, giveUpErr := handleStoppedStartTask(pctx.State.TypedSpec().Value, pctx.GetRequestID(), logger); giveUpErr != nil {
					return giveUpErr
				}
			}

			vm, err := p.getVM(ctx, pctx.State.TypedSpec().Value.Node, pctx.State.TypedSpec().Value.Vmid)
			if err != nil {
				return err
			}

			err = vm.CloudInit(
				ctx,
				"ide0",
				pctx.ConnectionParams.JoinConfig,
				fmt.Sprintf(
					`instance-id: %s
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
		}),
		provision.NewStep("registerHA", p.ha.RegisterStep),
		provision.NewStep("syncHARules", p.ha.SyncRulesStep),
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, machineRequest *infra.MachineRequest) error {
	// release up front: a request torn down before its VM materializes returns
	// at the Vmid == 0 check below.
	p.scheduler.release(machineRequest.Metadata().ID())

	if machine.TypedSpec().Value.Vmid == 0 {
		return nil
	}

	if err := p.ha.Deprovision(ctx, logger, machine); err != nil {
		return err
	}

	haRegistered := machine.TypedSpec().Value.HaRegistered

	var (
		node  string
		found bool
	)

	if haRegistered {
		var err error

		node, found, err = p.ha.FindVMNode(ctx, machine.TypedSpec().Value.Vmid)
		if err != nil {
			return err
		}
	} else {
		node = machine.TypedSpec().Value.Node
		found = node != ""
	}

	if !found {
		if pool := machine.TypedSpec().Value.Pool; pool != "" {
			p.cleanupPool(ctx, logger, pool)
		}

		return nil
	}

	vm, err := p.getVM(ctx, node, machine.TypedSpec().Value.Vmid)
	if err != nil {
		// The VM, or the node it was provisioned on, was removed out-of-band
		// (directly in Proxmox). There is nothing left to tear down, so treat it
		// as already deprovisioned and still clean up the pool.
		if isAlreadyGoneErr(err) {
			logger.Info("VM or its node no longer exists, treating as already deprovisioned",
				zap.String("node", node), zap.Int32("vmid", machine.TypedSpec().Value.Vmid), zap.Error(err))

			if pool := machine.TypedSpec().Value.Pool; pool != "" {
				p.cleanupPool(ctx, logger, pool)
			}

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

	task, err = vm.Delete(ctx, nil)
	if err != nil {
		return err
	}

	if err = p.waitForTaskToFinish(ctx, task); err != nil {
		return err
	}

	if pool := machine.TypedSpec().Value.Pool; pool != "" {
		p.cleanupPool(ctx, logger, pool)
	}

	return nil
}

// User tags come first so the tags string is stable across reconciles.
func buildTagsOption(userTags []string, machineRequestSet string) (string, bool) {
	tags := make([]string, 0, len(userTags)+1)
	tags = append(tags, userTags...)

	if machineRequestSet != "" {
		tags = append(tags, machineRequestTagPrefix+machineRequestSet)
	}

	if len(tags) == 0 {
		return "", false
	}

	return strings.Join(tags, ";"), true
}

func buildUSBDeviceOptions(devices []USBDevice) []proxmox.VirtualMachineOption {
	options := make([]proxmox.VirtualMachineOption, 0, len(devices))

	for i, device := range devices {
		parts := []string{fmt.Sprintf("mapping=%s", device.Mapping)}
		if device.USB3 {
			parts = append(parts, "usb3=1")
		}

		options = append(options, proxmox.VirtualMachineOption{
			Name:  fmt.Sprintf("usb%d", i),
			Value: strings.Join(parts, ","),
		})
	}

	return options
}

func buildFirmwareOptions(data Data, selectedStorage string) []proxmox.VirtualMachineOption {
	var options []proxmox.VirtualMachineOption

	if data.Bios != "" {
		options = append(options, proxmox.VirtualMachineOption{
			Name:  "bios",
			Value: data.Bios,
		})

		if data.Bios == "ovmf" {
			options = append(options, proxmox.VirtualMachineOption{
				Name:  "efidisk0",
				Value: fmt.Sprintf("%s:1,efitype=4m,pre-enrolled-keys=0", selectedStorage),
			})
		}
	}

	if data.VGA != "" {
		options = append(options, proxmox.VirtualMachineOption{
			Name:  "vga",
			Value: data.VGA,
		})
	}

	return options
}

func poolCreateDecision(exists bool, poolID, machineRequestSet string) (bool, error) {
	if exists {
		return false, nil
	}

	if poolID == machineRequestSet {
		return true, nil
	}

	return false, fmt.Errorf("proxmox pool %q does not exist; create it in Proxmox or leave \"pool\" empty to use the machine request set id", poolID)
}

// List + search instead of Pool(id): go-proxmox flattens missing-pool 404 into a generic 500.
func (p *Provisioner) findPoolInList(ctx context.Context, poolID string) (comment string, exists bool, err error) {
	pools, err := p.proxmoxClient.Pools(ctx)
	if err != nil {
		return "", false, fmt.Errorf("failed to list proxmox pools: %w", err)
	}

	for _, pool := range pools {
		if pool.PoolID == poolID {
			return pool.Comment, true, nil
		}
	}

	return "", false, nil
}

func (p *Provisioner) ensurePool(ctx context.Context, logger *zap.Logger, poolID, machineRequestSet string) error {
	_, exists, err := p.findPoolInList(ctx, poolID)
	if err != nil {
		return err
	}

	create, err := poolCreateDecision(exists, poolID, machineRequestSet)
	if err != nil {
		return err
	}

	if !create {
		return nil
	}

	logger.Info("creating proxmox pool", zap.String("pool", poolID))

	return p.proxmoxClient.NewPool(ctx, poolID, poolComment)
}

// Errors are swallowed on purpose: pool cleanup must not fail deprovision.
func (p *Provisioner) cleanupPool(ctx context.Context, logger *zap.Logger, poolID string) {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()

	comment, exists, err := p.findPoolInList(ctx, poolID)
	if err != nil {
		logger.Warn("failed to list proxmox pools during cleanup", zap.String("pool", poolID), zap.Error(err))

		return
	}

	if !exists || comment != poolComment {
		return
	}

	// List doesn't return members, so hit the detail endpoint before deciding
	// whether to delete.
	detail, err := p.proxmoxClient.Pool(ctx, poolID)
	if err != nil {
		logger.Warn("failed to load proxmox pool detail during cleanup", zap.String("pool", poolID), zap.Error(err))

		return
	}

	if detail.Comment != poolComment || len(detail.Members) != 0 {
		return
	}

	if err := detail.Delete(ctx); err != nil {
		logger.Warn("failed to delete proxmox pool", zap.String("pool", poolID), zap.Error(err))

		return
	}

	logger.Info("deleted empty proxmox pool", zap.String("pool", poolID))
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

// isTaskLive reports whether the task identified by id is still running or has
// already completed successfully. A failed, stopped or no-longer-known task
// returns false so the caller can re-issue the work instead of waiting on a
// task that will never finish.
func (p *Provisioner) isTaskLive(ctx context.Context, id string) bool {
	t := proxmox.NewTask(proxmox.UPID(id), p.proxmoxClient)

	if err := t.Ping(ctx); err != nil {
		return false
	}

	return t.IsRunning || t.IsSuccessful
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

// startAttemptsExhausted reports whether a prior handleStoppedStartTask call already gave up.
// It leaves VmStartTask pointing at the terminal stopped task instead of clearing it, so without
// this check every later reconcile of the startVM step would re-ping that same dead task and
// increment StartAttempts again instead of staying at its terminal error.
func startAttemptsExhausted(spec *specs.MachineSpec) error {
	if spec.StartAttempts >= maxStartAttempts {
		return fmt.Errorf("giving up starting VM after %d attempts: task ended in stopped state", spec.StartAttempts)
	}

	return nil
}

// handleStoppedStartTask clears VmStartTask so startVM reissues vm.Start(), or gives up
// after maxStartAttempts.
func handleStoppedStartTask(spec *specs.MachineSpec, requestID string, logger *zap.Logger) (restart bool, err error) {
	spec.StartAttempts++

	if spec.StartAttempts >= maxStartAttempts {
		logger.Error("giving up starting VM after too many attempts",
			zap.String("requestID", requestID),
			zap.Int32("attempts", spec.StartAttempts))

		return false, fmt.Errorf("giving up starting VM after %d attempts: task ended in stopped state", spec.StartAttempts)
	}

	spec.VmStartTask = ""

	logger.Info("VM start task stopped, retrying", zap.String("requestID", requestID), zap.Int32("attempts", spec.StartAttempts))

	return true, nil
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
	FreeMem                  uint64
	SameMachineRequestSetVMs int
	TotalVMs                 int
}

// shouldCountSetVMs disables the client-side set spread under HA, where Proxmox owns placement.
func shouldCountSetVMs(data Data, hasSet bool) bool {
	return hasSet && data.HA == nil
}

// isAlreadyGoneErr reports whether err means the VM, or the node it lived on,
// no longer exists in Proxmox — e.g. it was deleted or the node was removed
// out-of-band. On teardown such a machine has nothing left to clean up, so the
// caller treats it as already deprovisioned instead of failing forever.
//
// go-proxmox flattens every 500 into errors.New(res.Status) with no type or
// status code (proxmox.go handleResponse), so the reason phrase is the only
// signal. The phrasings matched here are, respectively: a missing VM config
// file, a node dropped from the cluster, and a node whose hostname no longer
// resolves while Proxmox proxies the per-node request.
func isAlreadyGoneErr(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	switch {
	case strings.Contains(msg, "does not exist"):
		return true
	case strings.Contains(msg, "no such node"):
		return true
	case strings.Contains(msg, "hostname lookup") && strings.Contains(msg, "failed to get address info"):
		return true
	default:
		return false
	}
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
