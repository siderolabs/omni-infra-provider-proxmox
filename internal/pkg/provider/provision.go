// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements Proxmox infra provider core.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
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
			nodes, err := p.proxmoxClient.Nodes(ctx)
			if err != nil {
				return err
			}

			var (
				maxFree  uint64
				nodeName string
			)

			if len(nodes) == 0 {
				return fmt.Errorf("no nodes available")
			}

			for _, node := range nodes {
				if node.Status != "online" {
					continue
				}

				freeMem := node.MaxMem - node.Mem
				if freeMem > maxFree {
					maxFree = freeMem
					nodeName = node.Node
				}
			}

			pctx.State.TypedSpec().Value.Node = nodeName

			logger.Info("picked the node for the Proxmox VM", zap.String("node", nodeName))

			return nil
		}),
		provision.NewStep("createSchematic", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			// generating schematic with join configs as it's going to be used in the ISO image which doesn't support partial configs
			schematic, err := pctx.GenerateSchematicID(ctx, logger)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.Schematic = schematic

			return nil
		}),
		provision.NewStep("uploadISO", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.VolumeUploadTask != "" {
				err := p.checkTaskStatus(ctx, pctx.State.TypedSpec().Value.VolumeUploadTask)
				if err != nil {
					return err
				}

				return nil
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
				"metal-amd64.iso",
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

			storages, err := node.Storages(ctx)
			if err != nil {
				return err
			}

			var selectedStorage string

			for _, storage := range storages {
				var env *cel.Env

				env, err = cel.NewEnv(
					cel.Variable("name", cel.StringType),
					cel.Variable("node", cel.StringType),
					cel.Variable("storageType", cel.StringType),
					cel.Variable("availableSpace", cel.UintType),
				)
				if err != nil {
					return err
				}

				var expr siderocel.Expression

				expr, err = siderocel.ParseBooleanExpression(data.StorageSelector, env)
				if err != nil {
					return err
				}

				var matched bool

				matched, err = expr.EvalBool(env, map[string]any{
					"name":           storage.Name,
					"node":           node.Name,
					"storageType":    storage.Type,
					"availableSpace": storage.Avail,
				})
				if err != nil {
					return err
				}

				if matched {
					selectedStorage = storage.Name

					break
				}
			}

			if selectedStorage == "" {
				return fmt.Errorf("failed to pick the disk for the VM volume: no matches for the condition %q", data.StorageSelector)
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

			task, err := node.NewVirtualMachine(
				ctx,
				vmid,
				proxmox.VirtualMachineOption{
					Name:  "smbios1",
					Value: "uuid=" + pctx.State.TypedSpec().Value.Uuid,
				},
				proxmox.VirtualMachineOption{
					Name:  "name",
					Value: pctx.GetRequestID(),
				},
				proxmox.VirtualMachineOption{
					Name:  "cdrom",
					Value: iso.VolID,
				},
				proxmox.VirtualMachineOption{
					Name:  "cpu",
					Value: "x86-64-v2-AES",
				},
				proxmox.VirtualMachineOption{
					Name:  "cores",
					Value: data.Cores,
				},
				proxmox.VirtualMachineOption{
					Name:  "sockets",
					Value: data.Sockets,
				},
				proxmox.VirtualMachineOption{
					Name:  "memory",
					Value: data.Memory,
				},
				proxmox.VirtualMachineOption{
					Name:  "scsi0",
					Value: fmt.Sprintf("%s:%d", selectedStorage, data.DiskSize),
				},
				proxmox.VirtualMachineOption{
					Name:  "scsihw",
					Value: "virtio-scsi-single",
				},
				proxmox.VirtualMachineOption{
					Name:  "onboot",
					Value: 1,
				},
				proxmox.VirtualMachineOption{
					Name:  "net0",
					Value: networkString,
				},
			)
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
