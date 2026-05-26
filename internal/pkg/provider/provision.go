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
	"os"
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

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const (
	machineRequestTagPrefix = "machine-request."
	poolComment             = "managed by omni-infra-provider-proxmox"
)

// Provisioner implements Talos emulator infra provider.
type Provisioner struct {
	proxmoxClient         *proxmox.Client
	pendingISODownloads   map[string]string
	pendingISODownloadsMu sync.Mutex
	poolMu                sync.Mutex
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(proxmoxClient *proxmox.Client) *Provisioner {
	return &Provisioner{
		proxmoxClient:       proxmoxClient,
		pendingISODownloads: make(map[string]string),
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
				// Skip nodes that are not online to avoid Proxmox API errors
				if node.Status != "online" {
					logger.Debug("skipping offline node", zap.String("node", node.Node), zap.String("status", node.Status))

					continue
				}

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

			if len(nodeInfoList) == 0 {
				return fmt.Errorf("no online nodes available for provisioning")
			}

			pickedNode := pickNode(nodeInfoList)

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
				logger.Info("ISO image is already being downloaded, reusing the existing task", zap.String("volumeID", isoName), zap.String("task", existing))

				pctx.State.TypedSpec().Value.VolumeUploadTask = existing

				return provision.NewRetryInterval(time.Second)
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

				joinConfig := pctx.ConnectionParams.JoinConfig

				if caPath := os.Getenv("TRUSTED_ROOTS_CONFIG_PATH"); caPath != "" {
					caConfig, readErr := os.ReadFile(caPath)
					if readErr != nil {
						return fmt.Errorf("failed to read trusted roots config from %q: %w", caPath, readErr)
					}

					joinConfig = joinConfig + "\n---\n" + string(caConfig)
				}

				err = vm.CloudInit(
					ctx,
					"ide0",
					joinConfig,
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
