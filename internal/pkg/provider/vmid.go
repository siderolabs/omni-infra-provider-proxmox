// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"fmt"
	"strconv"
	"strings"
)

// vmidRange is an inclusive [start, end] range of Proxmox VMIDs.
type vmidRange struct {
	start int
	end   int
}

// parseVMIDRange parses a "start-end" string (e.g. "200-250").
func parseVMIDRange(s string) (vmidRange, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return vmidRange{}, fmt.Errorf("invalid vmid_range %q: expected \"start-end\"", s)
	}

	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return vmidRange{}, fmt.Errorf("invalid vmid_range start %q: %w", parts[0], err)
	}

	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return vmidRange{}, fmt.Errorf("invalid vmid_range end %q: %w", parts[1], err)
	}

	if start < 100 {
		return vmidRange{}, fmt.Errorf("invalid vmid_range %q: start must be >= 100", s)
	}

	if start > end {
		return vmidRange{}, fmt.Errorf("invalid vmid_range %q: start must be <= end", s)
	}

	return vmidRange{start: start, end: end}, nil
}

// lowestFreeVMID returns the lowest id in the range that is not present in used.
func lowestFreeVMID(r vmidRange, used map[int]struct{}) (int, error) {
	for id := r.start; id <= r.end; id++ {
		if _, taken := used[id]; !taken {
			return id, nil
		}
	}

	return 0, fmt.Errorf("vmid_range %d-%d exhausted: no free VMID available", r.start, r.end)
}
