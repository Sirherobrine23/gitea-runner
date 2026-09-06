//go:build macos_vm_integration

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMacOSVMEnvironmentIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS VMs run on macOS")
	}
	if _, err := exec.LookPath("runner-executor-macos"); err != nil {
		t.Skip("runner-executor-macos executable not found")
	}
	image := os.Getenv("MACOS_VM_TEST_IMAGE")
	if image == "" {
		t.Skip("set MACOS_VM_TEST_IMAGE to a macOS VM image to run this test")
	}

	ctx := context.Background()
	vmName := fmt.Sprintf("gitea-runner-test-%d", time.Now().UnixNano())
	base := "/Users/admin/runner"
	if os.Getenv("RUNNER_EXECUTOR_MACOS_DEV_MODE") == "1" {
		base = t.TempDir()
	}
	env, err := NewMacOSVMEnvironment(MacOSVMEnvironmentInput{
		Path:         base + "/scratch/" + vmName,
		TmpDir:       base + "/scratch/" + vmName + "/tmp",
		ToolCache:    base + "/scratch/" + vmName + "/tool_cache",
		Workdir:      t.TempDir(),
		GuestWorkdir: base + "/workspace/" + vmName,
		ActPath:      base + "/scratch/" + vmName + "/act",
		Image:        image,
		VMName:       vmName,
		ExecutorPath: "runner-executor-macos",
		BootTimeout:  10 * time.Minute,
		Stdout:       io.Discard,
		Stderr:       io.Discard,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = env.Remove()(context.Background())
	})

	require.NoError(t, env.Create(nil, nil)(ctx))
	require.NoError(t, env.Start(false)(ctx))
	require.NoError(t, os.MkdirAll(env.GuestWorkdir, 0o755))

	require.NoError(t, env.Copy("/tmp", &FileEntry{
		Name: "gitea-runner-macos.txt",
		Mode: 0o644,
		Body: "hello from macos-vm",
	})(ctx))

	archive, err := env.GetContainerArchive(ctx, "/tmp/gitea-runner-macos.txt")
	require.NoError(t, err)
	body := readFirstTarFile(t, archive)
	require.NoError(t, archive.Close())
	require.Equal(t, "hello from macos-vm", body)

	var output bytes.Buffer
	oldOut, oldErr := env.ReplaceLogWriter(&output, &output)
	defer env.ReplaceLogWriter(oldOut, oldErr)
	require.NoError(t, env.Exec([]string{"/usr/bin/uname", "-s"}, nil, "", "")(ctx))
	require.Contains(t, output.String(), "Darwin")

	require.NoError(t, env.Remove()(ctx))
}

func readFirstTarFile(t *testing.T, archive io.Reader) string {
	t.Helper()
	reader := tar.NewReader(archive)
	header, err := reader.Next()
	require.NoError(t, err)
	require.Contains(t, header.Name, "gitea-runner-macos.txt")
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	return string(body)
}
