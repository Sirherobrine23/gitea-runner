// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMacOSVMsToDelete(t *testing.T) {
	cutoff := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	vms := []macOSVM{
		{Name: "GITEA-MACOS-VM-runner-1-stale", AccessedAt: "2026-09-04T10:00:00Z"},
		{Name: "GITEA-MACOS-VM-runner-1-running", AccessedAt: "2026-09-04T10:00:00Z", Running: true},
		{Name: "GITEA-MACOS-VM-runner-1-fresh", AccessedAt: "2026-09-05T11:00:00Z"},
		{Name: "GITEA-MACOS-VM-runner-2-stale", AccessedAt: "2026-09-04T10:00:00Z"},
	}

	assert.Equal(t, []string{"GITEA-MACOS-VM-runner-1-stale"}, macOSVMsToDelete(vms, "runner-1", cutoff))
}

func TestParseMacOSVMTime(t *testing.T) {
	parsed, err := parseMacOSVMTime("2026-09-05T10:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC), parsed)

	parsed, err = parseMacOSVMTime("2026-09-05T10:00:00.123456789Z")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 9, 5, 10, 0, 0, 123456789, time.UTC), parsed)

	_, err = parseMacOSVMTime("")
	require.Error(t, err)
}
