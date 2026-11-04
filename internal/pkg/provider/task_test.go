// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
)

func TestTaskFailure(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve-1:00001234:00005678:6A000000:qmstart:100:root@pam:"

	newTask := func(t *testing.T, logLines []string) *proxmox.Task {
		t.Helper()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/log") {
				http.NotFound(w, r)

				return
			}

			rows := make([]map[string]any, 0, len(logLines))

			// reversed, the order must come from the line numbers
			for i, logLine := range slices.Backward(logLines) {
				rows = append(rows, map[string]any{"n": i + 1, "t": logLine})
			}

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(map[string]any{"data": rows}); err != nil {
				t.Errorf("failed to encode the task log: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		task := proxmox.NewTask(upid, proxmox.NewClient(srv.URL))
		task.Status = "stopped"
		task.ExitStatus = "start failed: QEMU exited with code 1"

		return task
	}

	t.Run("exit status and log tail", func(t *testing.T) {
		t.Parallel()

		task := newTask(t, []string{
			"kvm: -drive file=/dev/pve/vm-100-disk-0: Could not open '/dev/pve/vm-100-disk-0': No such file or directory",
			"",
			"TASK ERROR: start failed: QEMU exited with code 1",
		})

		err := provider.TaskFailure(t.Context(), task)

		require.ErrorIs(t, err, provider.ErrTaskFailed)
		assert.EqualError(t, err, `proxmox task failed: qmstart 100 on node "pve-1": start failed: QEMU exited with code 1, `+
			`task log: kvm: -drive file=/dev/pve/vm-100-disk-0: Could not open '/dev/pve/vm-100-disk-0': No such file or directory`)
	})

	t.Run("only the last lines of a long log", func(t *testing.T) {
		t.Parallel()

		lines := make([]string, 0, 15)
		for i := range 15 {
			lines = append(lines, fmt.Sprintf("line %d", i))
		}

		err := provider.TaskFailure(t.Context(), newTask(t, lines))

		require.ErrorIs(t, err, provider.ErrTaskFailed)
		assert.True(t, strings.HasSuffix(err.Error(), "task log: line 5 | line 6 | line 7 | line 8 | line 9 | line 10 | line 11 | line 12 | line 13 | line 14"), err.Error())
	})

	t.Run("empty log", func(t *testing.T) {
		t.Parallel()

		err := provider.TaskFailure(t.Context(), newTask(t, nil))

		require.ErrorIs(t, err, provider.ErrTaskFailed)
		assert.EqualError(t, err, `proxmox task failed: qmstart 100 on node "pve-1": start failed: QEMU exited with code 1`)
	})
}
