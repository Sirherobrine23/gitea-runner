// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMacOSVMEnvironment(t *testing.T) {
	env, err := NewMacOSVMEnvironment(MacOSVMEnvironmentInput{
		Image:  "ghcr.io/cirruslabs/macos-sonoma-base:latest",
		VMName: "job-vm",
	})
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/cirruslabs/macos-sonoma-base:latest", env.Image)
	assert.Equal(t, "job-vm", env.VMName)
	assert.Equal(t, defaultMacOSVMBootTimeout, env.BootTimeout)
	assert.Equal(t, "runner-executor-macos", env.executorPath)

	_, err = NewMacOSVMEnvironment(MacOSVMEnvironmentInput{VMName: "job-vm"})
	require.Error(t, err)
	_, err = NewMacOSVMEnvironment(MacOSVMEnvironmentInput{Image: "macos"})
	require.Error(t, err)
}

func TestMacOSVMExecArgs(t *testing.T) {
	args := macOSVMExecArgs("vm", []string{"bash", "-c", "echo hi"}, map[string]string{"B": "two", "A": "one"}, "/Users/admin/work")
	assert.Equal(t, []string{
		"exec", "vm", "/usr/bin/env",
		"A=one", "B=two",
		"/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", "/Users/admin/work",
		"bash", "-c", "echo hi",
	}, args)

	args = macOSVMExecArgs("vm", []string{"true"}, nil, "")
	assert.Equal(t, []string{
		"exec", "vm", "/usr/bin/env",
		"/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", "",
		"true",
	}, args)
}

func TestMacOSVMEnvironmentResolveWorkdir(t *testing.T) {
	env := &MacOSVMEnvironment{GuestWorkdir: "/Users/admin/work"}
	assert.Equal(t, "/Users/admin/work", env.resolveWorkdir(""))
	assert.Equal(t, "/Users/admin/work/sub", env.resolveWorkdir("sub"))
	assert.Equal(t, "/tmp", env.resolveWorkdir("/tmp"))
}

func TestMacOSVMEnvironmentToContainerPath(t *testing.T) {
	env := &MacOSVMEnvironment{Workdir: "/workspace/owner/repo", GuestWorkdir: "/Users/admin/work/owner/repo"}
	assert.Equal(t, "/Users/admin/work/owner/repo", env.ToContainerPath("/workspace/owner/repo"))
	assert.Equal(t, "/Users/admin/work/owner/repo/sub", env.ToContainerPath("/workspace/owner/repo/sub"))
	assert.Equal(t, "/elsewhere", env.ToContainerPath("/elsewhere"))
}

func TestMacOSVMEnvironmentGetRunnerContext(t *testing.T) {
	env := &MacOSVMEnvironment{
		TmpDir:    "/Users/admin/scratch/tmp",
		ToolCache: "/Users/admin/scratch/tool_cache",
	}
	ctx := env.GetRunnerContext(context.Background())
	assert.Equal(t, "macOS", ctx["os"])
	assert.Equal(t, goArchToActionArch(runtime.GOARCH), ctx["arch"])
	assert.Equal(t, "/Users/admin/scratch/tmp", ctx["temp"])
	assert.Equal(t, "/Users/admin/scratch/tool_cache", ctx["tool_cache"])
}

func TestMacOSVMEnvironmentReplaceLogWriter(t *testing.T) {
	oldOut := io.Discard
	oldErr := io.Discard
	env := &MacOSVMEnvironment{Stdout: oldOut, Stderr: oldErr}
	newOut, newErr := env.ReplaceLogWriter(io.Discard, io.Discard)
	assert.Equal(t, oldOut, newOut)
	assert.Equal(t, oldErr, newErr)
}

func TestMacOSVMEnvironmentDefaultPathVariable(t *testing.T) {
	env := &MacOSVMEnvironment{}
	assert.Equal(t, "PATH", env.GetPathVariableName())
	assert.Equal(t, "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin", env.DefaultPathVariable())
	assert.Equal(t, "/a:/b", env.JoinPathVariable("/a", "/b"))
	assert.False(t, env.IsEnvironmentCaseInsensitive())
}
