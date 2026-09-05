// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type macOSVM struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Running    bool   `json:"running"`
	AccessedAt string `json:"accessed_at"`
}

// CleanupOrphanMacOSVMs deletes local macOS VMs left behind by this runner that
// have not been accessed since cutoff.
func CleanupOrphanMacOSVMs(ctx context.Context, executorPath, uuid string, cutoff time.Time) error {
	if uuid == "" {
		return nil
	}

	output, err := exec.CommandContext(ctx, executorPath, "list", "--format", "json").Output()
	if err != nil {
		return fmt.Errorf("list macOS VMs: %w", err)
	}

	var vms []macOSVM
	if err := json.Unmarshal(output, &vms); err != nil {
		return fmt.Errorf("parse macOS VM list: %w", err)
	}

	var errs []error
	for _, name := range macOSVMsToDelete(vms, uuid, cutoff) {
		if err := exec.CommandContext(ctx, executorPath, "destroy", name).Run(); err != nil {
			errs = append(errs, fmt.Errorf("destroy macOS VM %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func macOSVMsToDelete(vms []macOSVM, uuid string, cutoff time.Time) []string {
	prefix := "GITEA-MACOS-VM-" + uuid + "-"
	var stale []string
	for _, vm := range vms {
		if vm.Running || !strings.HasPrefix(vm.Name, prefix) {
			continue
		}
		accessed, err := parseMacOSVMTime(vm.AccessedAt)
		if err != nil || accessed.After(cutoff) {
			continue
		}
		stale = append(stale, vm.Name)
	}
	return stale
}

func parseMacOSVMTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, errors.New("empty access time")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, raw)
}
