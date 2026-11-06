// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/resources"
)

const (
	fakeNode = "pve1"
	fakeVMID = 100

	startFailure = "start failed: QEMU exited with code 1"
	cloneFailure = "clone failed: can't lock file '/var/lock/qemu-server/lock-100.conf' - got timeout"
)

// fakeProxmox is a minimal Proxmox API serving a single VM. Task status payloads mirror
// the real API: a finished task is "stopped" and its result is in "exitstatus".
type fakeProxmox struct {
	t         *testing.T
	tasks     map[string]string
	vmStatus  string // "running", "stopped" or "" once deleted
	startExit string // exit status of every qmstart task

	mu sync.Mutex

	failDestroyPoll atomic.Bool // fail polling the next qmdestroy task, the delete itself still happens

	requests   atomic.Int32
	startCalls atomic.Int32
	stopCalls  atomic.Int32
	deletes    atomic.Int32
	taskSeq    atomic.Int32
}

func newFakeProxmox(t *testing.T, vmStatus, startExit string) (*fakeProxmox, *proxmox.Client) {
	f := &fakeProxmox{t: t, vmStatus: vmStatus, startExit: startExit, tasks: map[string]string{}}

	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	provider.SetTaskPollInterval(t, time.Millisecond)

	return f, proxmox.NewClient(srv.URL)
}

// addTask registers a finished task with the given exit status and returns its UPID.
func (f *fakeProxmox) addTask(kind, exitStatus string) string {
	upid := fmt.Sprintf("UPID:%s:00000000:00000000:00000000:%s:%d:root@pam:", fakeNode, kind, f.taskSeq.Add(1))

	f.mu.Lock()
	f.tasks[upid] = exitStatus
	f.mu.Unlock()

	return upid
}

func (f *fakeProxmox) handler() http.Handler {
	vmPath := fmt.Sprintf("/nodes/%s/qemu/%d", fakeNode, fakeVMID)

	mux := http.NewServeMux()

	mux.HandleFunc(fmt.Sprintf("/nodes/%s/status", fakeNode), func(w http.ResponseWriter, _ *http.Request) {
		f.writeData(w, map[string]any{})
	})
	mux.HandleFunc(vmPath+"/status/current", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		status := f.vmStatus
		f.mu.Unlock()

		if status == "" {
			f.writeProxmoxError(w, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", fakeNode, fakeVMID))

			return
		}

		f.writeData(w, map[string]any{"vmid": fakeVMID, "status": status})
	})
	mux.HandleFunc(vmPath+"/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.writeData(w, map[string]any{})

			return
		}

		f.writeData(w, f.addTask("qmconfig", "OK"))
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/storage", fakeNode), func(w http.ResponseWriter, _ *http.Request) {
		f.writeData(w, []map[string]any{{"storage": "local", "content": "iso", "enabled": 1}})
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/storage/local/upload", fakeNode), func(w http.ResponseWriter, _ *http.Request) {
		f.writeData(w, f.addTask("imgcopy", "OK"))
	})
	mux.HandleFunc(vmPath+"/status/start", func(w http.ResponseWriter, _ *http.Request) {
		f.startCalls.Add(1)
		f.writeData(w, f.addTask("qmstart", f.startExit))
	})
	mux.HandleFunc(vmPath+"/status/stop", func(w http.ResponseWriter, _ *http.Request) {
		f.stopCalls.Add(1)

		f.mu.Lock()
		f.vmStatus = "stopped"
		f.mu.Unlock()

		f.writeData(w, f.addTask("qmstop", "OK"))
	})
	mux.HandleFunc(vmPath, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(f.t, http.MethodDelete, r.Method)

		f.deletes.Add(1)

		f.mu.Lock()
		f.vmStatus = ""
		f.mu.Unlock()

		f.writeData(w, f.addTask("qmdestroy", "OK"))
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/", fakeNode), func(w http.ResponseWriter, r *http.Request) {
		upid, endpoint, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/nodes/%s/tasks/", fakeNode)), "/")

		f.mu.Lock()
		exitStatus, ok := f.tasks[upid]
		f.mu.Unlock()

		require.True(f.t, ok, "unknown task %q", upid)

		if strings.Contains(upid, ":qmdestroy:") && f.failDestroyPoll.CompareAndSwap(true, false) {
			f.writeProxmoxError(w, "proxy loop detected")

			return
		}

		if endpoint == "log" {
			f.writeData(w, []map[string]any{{"n": 1, "t": exitStatus}, {"n": 2, "t": "TASK ERROR: " + exitStatus}})

			return
		}

		// Task.Ping decodes onto the polled task itself, so UPID and node must be echoed back
		f.writeData(w, map[string]any{"upid": upid, "node": fakeNode, "status": "stopped", "exitstatus": exitStatus})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		mux.ServeHTTP(w, r)
	})
}

func (f *fakeProxmox) writeData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	require.NoError(f.t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
}

// writeProxmoxError mimics Proxmox putting the error message into the HTTP status line,
// which net/http does not allow setting, hence the hijack.
func (f *fakeProxmox) writeProxmoxError(w http.ResponseWriter, msg string) {
	conn, buf, err := w.(http.Hijacker).Hijack() //nolint:forcetypeassert,errcheck
	require.NoError(f.t, err)

	defer conn.Close() //nolint:errcheck

	_, err = fmt.Fprintf(buf, "HTTP/1.1 500 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", msg)
	require.NoError(f.t, err)
	require.NoError(f.t, buf.Flush())
}

// stepRunner runs a provision step the way the omni provision controller does: the step
// mutates a copy of the machine which is persisted only if the step returns nil or a
// requeue error, any other error rolls the mutations back.
type stepRunner struct {
	machine *resources.Machine
	step    provision.Step[*resources.Machine]
}

func newStepRunner(t *testing.T, p *provider.Provisioner, name string, machine *resources.Machine) *stepRunner {
	for _, step := range p.ProvisionSteps() {
		if step.Name() == name {
			return &stepRunner{step: step, machine: machine}
		}
	}

	require.FailNow(t, "step not found", name)

	return nil
}

func (r *stepRunner) run(t *testing.T) error {
	machine := r.machine.DeepCopy().(*resources.Machine) //nolint:forcetypeassert,errcheck

	err := r.step.Run(t.Context(), zap.NewNop(), provision.NewContext(
		infra.NewMachineRequest("req-1"),
		infra.NewMachineRequestStatus("req-1"),
		machine,
		provision.ConnectionParams{JoinConfig: "join-config"},
		nil,
		nil,
	))

	var requeueErr *controller.RequeueError

	if err == nil || errors.As(err, &requeueErr) {
		r.machine = machine
	}

	return err
}

func newMachine() *resources.Machine {
	machine := resources.NewMachine("default", "req-1")
	machine.TypedSpec().Value.Node = fakeNode
	machine.TypedSpec().Value.Vmid = fakeVMID
	machine.TypedSpec().Value.Uuid = "11111111-1111-1111-1111-111111111111"

	return machine
}

func isRequeue(err error) bool {
	var requeueErr *controller.RequeueError

	return errors.As(err, &requeueErr)
}

func TestStartVMGivesUpAndDestroysVM(t *testing.T) {
	fake, client := newFakeProxmox(t, "stopped", startFailure)
	runner := newStepRunner(t, provider.NewProvisioner(client), "startVM", newMachine())

	// every reconcile but the last reissues vm.Start() after the previous task failed
	for range provider.MaxStartAttempts {
		require.True(t, isRequeue(runner.run(t)))
	}

	require.EqualValues(t, provider.MaxStartAttempts, fake.startCalls.Load())
	require.Zero(t, fake.deletes.Load())

	// the last start task failed too: the VM is destroyed and the failure persisted
	err := runner.run(t)
	require.True(t, isRequeue(err), "give up must be a requeue error so that the failure is persisted")
	require.ErrorContains(t, err, startFailure)

	require.EqualValues(t, provider.MaxStartAttempts, fake.startCalls.Load())
	require.EqualValues(t, 1, fake.deletes.Load())
	require.Zero(t, fake.stopCalls.Load(), "a stopped VM must be deleted without stopping it")
	require.Contains(t, runner.machine.TypedSpec().Value.Failure, startFailure)
	require.Empty(t, runner.machine.TypedSpec().Value.VmStartTask)
	require.Zero(t, runner.machine.TypedSpec().Value.Vmid, "the destroyed VM ID must not be kept, it can be reused by another VM")

	// further reconciles fail the request without touching Proxmox
	requests := fake.requests.Load()

	for range 3 {
		err = runner.run(t)
		require.Error(t, err)
		require.False(t, isRequeue(err), "must be a plain error so the request is marked failed")
		require.ErrorContains(t, err, startFailure)
	}

	require.Equal(t, requests, fake.requests.Load())
}

func TestStartVMGiveUpDeleteTaskPollFails(t *testing.T) {
	fake, client := newFakeProxmox(t, "stopped", startFailure)
	fake.failDestroyPoll.Store(true)

	machine := newMachine()
	machine.TypedSpec().Value.StartAttempts = provider.MaxStartAttempts - 1
	machine.TypedSpec().Value.VmStartTask = fake.addTask("qmstart", startFailure)

	runner := newStepRunner(t, provider.NewProvisioner(client), "startVM", machine)

	// the VM is deleted, but waiting for the delete task fails: nothing is persisted
	err := runner.run(t)
	require.Error(t, err)
	require.False(t, isRequeue(err))
	require.EqualValues(t, 1, fake.deletes.Load())
	require.Empty(t, runner.machine.TypedSpec().Value.Failure)

	// the next reconcile finds the VM gone and records the original start failure
	err = runner.run(t)
	require.True(t, isRequeue(err))
	require.ErrorContains(t, err, startFailure)
	require.EqualValues(t, 1, fake.deletes.Load())
	require.Zero(t, fake.startCalls.Load())
	require.Contains(t, runner.machine.TypedSpec().Value.Failure, fmt.Sprintf("after %d attempts", provider.MaxStartAttempts))
	require.Contains(t, runner.machine.TypedSpec().Value.Failure, startFailure)

	// and then stays idle
	requests := fake.requests.Load()

	err = runner.run(t)
	require.Error(t, err)
	require.False(t, isRequeue(err))
	require.Equal(t, requests, fake.requests.Load())
}

func TestStartVMFailedTaskButVMRunning(t *testing.T) {
	fake, client := newFakeProxmox(t, "running", "")
	machine := newMachine()
	machine.TypedSpec().Value.VmStartTask = fake.addTask("qmstart", "VM 100 already running")

	runner := newStepRunner(t, provider.NewProvisioner(client), "startVM", machine)

	require.NoError(t, runner.run(t))
	require.Zero(t, fake.startCalls.Load())
	require.Zero(t, fake.deletes.Load())
	require.Zero(t, runner.machine.TypedSpec().Value.StartAttempts)
}

func TestSyncVMFailedCloneDestroysVM(t *testing.T) {
	fake, client := newFakeProxmox(t, "stopped", "")
	machine := newMachine()
	machine.TypedSpec().Value.VmCreateTask = fake.addTask("qmclone", cloneFailure)

	runner := newStepRunner(t, provider.NewProvisioner(client), "syncVM", machine)

	err := runner.run(t)
	require.True(t, isRequeue(err))
	require.ErrorContains(t, err, cloneFailure)
	require.EqualValues(t, 1, fake.deletes.Load())
	require.Contains(t, runner.machine.TypedSpec().Value.Failure, cloneFailure)

	requests := fake.requests.Load()

	err = runner.run(t)
	require.Error(t, err)
	require.False(t, isRequeue(err))
	require.Equal(t, requests, fake.requests.Load())
}

func TestSyncVMGiveUpWithVMAlreadyGone(t *testing.T) {
	fake, client := newFakeProxmox(t, "", "")
	machine := newMachine()
	machine.TypedSpec().Value.VmCreateTask = fake.addTask("qmclone", cloneFailure)

	runner := newStepRunner(t, provider.NewProvisioner(client), "syncVM", machine)

	err := runner.run(t)
	require.True(t, isRequeue(err))
	require.ErrorContains(t, err, cloneFailure)
	require.Zero(t, fake.deletes.Load())
	require.Contains(t, runner.machine.TypedSpec().Value.Failure, cloneFailure)
}

func TestDeprovisionVM(t *testing.T) {
	for _, tc := range []struct {
		vmStatus  string
		wantStops int32
	}{
		{vmStatus: "stopped", wantStops: 0},
		{vmStatus: "running", wantStops: 1},
	} {
		t.Run(tc.vmStatus, func(t *testing.T) {
			fake, client := newFakeProxmox(t, tc.vmStatus, "")

			require.NoError(t, provider.NewProvisioner(client).Deprovision(context.Background(), zap.NewNop(), newMachine(), infra.NewMachineRequest("req-1")))
			require.Equal(t, tc.wantStops, fake.stopCalls.Load())
			require.EqualValues(t, 1, fake.deletes.Load())
		})
	}
}
