// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"

	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

const (
	defaultMacOSVMBootTimeout = 5 * time.Minute
	macOSVMReadyPollInterval  = 500 * time.Millisecond
	macOSVMStopWaitTimeout    = 10 * time.Second
)

// MacOSVMEnvironment runs workflow steps in an ephemeral macOS VM.
type MacOSVMEnvironment struct {
	Path         string // guest scratch directory
	TmpDir       string // guest temp directory
	ToolCache    string // guest tool cache directory
	Workdir      string // host-side workspace path
	GuestWorkdir string // guest-side workspace path
	ActPath      string // guest act directory

	Image        string
	VMName       string
	ExecutorPath string
	CPU          int
	Memory       int
	BootTimeout  time.Duration

	Stdout io.Writer
	Stderr io.Writer

	executorPath string
	running      bool
	runCmd       *exec.Cmd
	runDone      chan struct{}
	runErr       error
}

// MacOSVMEnvironmentInput is the input for NewMacOSVMEnvironment.
type MacOSVMEnvironmentInput struct {
	Path         string
	TmpDir       string
	ToolCache    string
	Workdir      string
	GuestWorkdir string
	ActPath      string
	Image        string
	VMName       string
	ExecutorPath string
	CPU          int
	Memory       int
	BootTimeout  time.Duration
	Stdout       io.Writer
	Stderr       io.Writer
}

func NewMacOSVMEnvironment(input MacOSVMEnvironmentInput) (*MacOSVMEnvironment, error) {
	if input.Image == "" {
		return nil, errors.New("macOS VM image is required")
	}
	if input.VMName == "" {
		return nil, errors.New("macOS VM name is required")
	}
	if input.BootTimeout <= 0 {
		input.BootTimeout = defaultMacOSVMBootTimeout
	}
	if input.ExecutorPath == "" {
		input.ExecutorPath = "runner-executor-macos"
	}
	return &MacOSVMEnvironment{
		Path:         input.Path,
		TmpDir:       input.TmpDir,
		ToolCache:    input.ToolCache,
		Workdir:      input.Workdir,
		GuestWorkdir: input.GuestWorkdir,
		ActPath:      input.ActPath,
		Image:        input.Image,
		VMName:       input.VMName,
		ExecutorPath: input.ExecutorPath,
		CPU:          input.CPU,
		Memory:       input.Memory,
		BootTimeout:  input.BootTimeout,
		Stdout:       input.Stdout,
		Stderr:       input.Stderr,
		executorPath: input.ExecutorPath,
	}, nil
}

var _ ExecutionsEnvironment = &MacOSVMEnvironment{}

func (e *MacOSVMEnvironment) Create(_, _ []string) common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		// A previous run can leave the deterministic job VM behind. Destroy it first.
		_ = e.runExecutor(ctx, nil, io.Discard, io.Discard, "destroy", e.VMName)

		args := []string{"provision", e.Image, e.VMName}
		if e.CPU > 0 {
			args = append(args, "--cpu", strconv.Itoa(e.CPU))
		}
		if e.Memory > 0 {
			args = append(args, "--memory", strconv.Itoa(e.Memory))
		}
		return e.runExecutor(ctx, nil, e.Stdout, e.Stderr, args...)
	}
}

func (e *MacOSVMEnvironment) Copy(destPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		for _, file := range files {
			dest := path.Join(destPath, file.Name)
			script := `mkdir -p "$1" && cat > "$2" && chmod "$3" "$2"`
			mode := file.Mode
			if mode == 0 {
				mode = 0o644
			}
			args := []string{"exec", "--stdin", e.VMName, "/bin/sh", "-c", script, "sh", path.Dir(dest), dest, strconv.FormatInt(mode, 8)}
			if err := e.runExecutor(ctx, strings.NewReader(file.Body), e.Stdout, e.Stderr, args...); err != nil {
				return err
			}
		}
		return nil
	}
}

func (e *MacOSVMEnvironment) CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		pipeReader, pipeWriter := io.Pipe()
		writeDone := make(chan error, 1)
		go func() {
			err := e.writeTar(ctx, pipeWriter, srcPath, useGitIgnore)
			_ = pipeWriter.CloseWithError(err)
			writeDone <- err
		}()

		script := `export COPYFILE_DISABLE=1; mkdir -p "$1" && /usr/bin/tar -xf - -C "$1"`
		cmdErr := e.runExecutor(ctx, pipeReader, e.Stdout, e.Stderr, "exec", "--stdin", e.VMName, "/bin/sh", "-c", script, "sh", destPath)
		_ = pipeReader.Close()
		writeErr := <-writeDone
		return errors.Join(cmdErr, writeErr)
	}
}

func (e *MacOSVMEnvironment) writeTar(ctx context.Context, writer io.Writer, srcPath string, useGitIgnore bool) error {
	tarWriter := tar.NewWriter(writer)
	defer tarWriter.Close()

	srcPrefix := filepath.Dir(srcPath)
	if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
		srcPrefix += string(filepath.Separator)
	}

	var ignorer gitignore.Matcher
	if useGitIgnore {
		patterns, err := gitignore.ReadPatterns(polyfill.New(osfs.New(srcPath)), nil)
		if err != nil {
			common.Logger(ctx).Debugf("Error loading .gitignore: %v", err)
		}
		ignorer = gitignore.NewMatcher(patterns)
	}

	collector := &filecollector.FileCollector{
		Ignorer:   ignorer,
		SrcPath:   srcPath,
		SrcPrefix: srcPrefix,
		Handler:   &filecollector.TarCollector{TarWriter: tarWriter},
	}
	return filepath.Walk(srcPath, collector.CollectFiles(ctx, []string{}))
}

func (e *MacOSVMEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	if common.Dryrun(ctx) {
		return nil, errors.New("DRYRUN is not supported in GetContainerArchive")
	}
	cmd := exec.CommandContext(ctx, e.executorPath, "exec", e.VMName, "/usr/bin/env", "COPYFILE_DISABLE=1", "/usr/bin/tar", "-cf", "-", srcPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = e.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &macOSVMArchiveReader{ReadCloser: stdout, cmd: cmd}, nil
}

type macOSVMArchiveReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (r *macOSVMArchiveReader) Close() error {
	closeErr := r.ReadCloser.Close()
	return errors.Join(closeErr, r.cmd.Wait())
}

func (e *MacOSVMEnvironment) Inspect(ctx context.Context) (*Info, error) {
	if common.Dryrun(ctx) {
		return &Info{Health: HealthNone, Ports: map[string]string{}}, nil
	}
	state := "exited"
	if e.running {
		state = StateRunning
	}
	return &Info{ID: e.VMName, State: state, Health: HealthNone, Ports: map[string]string{}}, nil
}

func (e *MacOSVMEnvironment) DumpLogs(context.Context) error {
	return nil
}

func (e *MacOSVMEnvironment) Pull(forcePull bool) common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		return nil
	}
}

func (e *MacOSVMEnvironment) Start(bool) common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		cmd := exec.Command(e.executorPath, "start", "--no-graphics", e.VMName)
		cmd.Stdout = e.Stdout
		cmd.Stderr = e.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		e.runCmd = cmd
		e.runDone = make(chan struct{})
		go func() {
			e.runErr = cmd.Wait()
			close(e.runDone)
		}()
		e.running = true
		return e.waitReady(ctx)
	}
}

func (e *MacOSVMEnvironment) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, e.BootTimeout)
	defer cancel()

	var lastErr error
	for {
		if err := e.runExecutor(ctx, nil, io.Discard, io.Discard, "exec", e.VMName, "/usr/bin/true"); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("macOS VM %s did not become ready: %w", e.VMName, lastErr)
		case <-e.runDone:
			if e.runErr != nil {
				return fmt.Errorf("macOS VM start exited before the VM became ready: %w", e.runErr)
			}
			return errors.New("macOS VM start exited before the VM became ready")
		case <-time.After(macOSVMReadyPollInterval):
		}
	}
}

func (e *MacOSVMEnvironment) Exec(command []string, env map[string]string, _, workdir string) common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		err := e.runExecutor(ctx, nil, e.Stdout, e.Stderr, macOSVMExecArgs(e.VMName, command, env, e.resolveWorkdir(workdir))...)
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return ExitCodeError(exitErr.ExitCode())
		}
		return err
	}
}

func macOSVMExecArgs(vmName string, command []string, env map[string]string, workdir string) []string {
	args := []string{"exec", vmName, "/usr/bin/env"}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		args = append(args, key+"="+env[key])
	}
	args = append(args, "/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", workdir)
	return append(args, command...)
}

func (e *MacOSVMEnvironment) resolveWorkdir(workdir string) string {
	switch {
	case workdir == "":
		return e.GuestWorkdir
	case strings.HasPrefix(workdir, "/"):
		return workdir
	default:
		return path.Join(e.GuestWorkdir, workdir)
	}
}

func (e *MacOSVMEnvironment) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return parseEnvFile(e, srcPath, env)
}

func (e *MacOSVMEnvironment) UpdateFromImageEnv(*map[string]string) common.Executor {
	return func(context.Context) error {
		return nil
	}
}

func (e *MacOSVMEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()

		if e.runCmd != nil && e.runDone != nil {
			select {
			case <-e.runDone:
			default:
				if err := e.runExecutor(cleanCtx, nil, e.Stdout, e.Stderr, "stop", e.VMName, "--timeout", "20"); err != nil {
					common.Logger(ctx).Debugf("macOS VM stop failed for VM %s: %v", e.VMName, err)
				}
				select {
				case <-e.runDone:
				case <-time.After(macOSVMStopWaitTimeout):
					if e.runCmd.Process != nil {
						_ = e.runCmd.Process.Kill()
					}
					common.Logger(ctx).Warnf("timed out waiting for macOS VM start to exit for VM %s", e.VMName)
				}
			}
		}
		e.running = false
		if err := e.runExecutor(cleanCtx, nil, e.Stdout, e.Stderr, "destroy", e.VMName); err != nil {
			common.Logger(ctx).Warnf("failed to destroy macOS VM %s: %v", e.VMName, err)
		}
		return nil
	}
}

func (e *MacOSVMEnvironment) Close() common.Executor {
	return func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}
		if e.runCmd != nil && e.runCmd.Process != nil {
			_ = e.runCmd.Process.Kill()
		}
		e.runCmd = nil
		return nil
	}
}

func (e *MacOSVMEnvironment) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	oldOut := e.Stdout
	oldErr := e.Stderr
	e.Stdout = stdout
	e.Stderr = stderr
	return oldOut, oldErr
}

func (e *MacOSVMEnvironment) ToContainerPath(hostPath string) string {
	if filepath.Clean(hostPath) == filepath.Clean(e.Workdir) {
		return e.GuestWorkdir
	}
	if rel, err := filepath.Rel(e.Workdir, hostPath); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path.Join(e.GuestWorkdir, filepath.ToSlash(rel))
	}
	return filepath.ToSlash(hostPath)
}

func (e *MacOSVMEnvironment) GetActPath() string {
	return e.ActPath
}

func (*MacOSVMEnvironment) GetPathVariableName() string {
	return "PATH"
}

func (*MacOSVMEnvironment) DefaultPathVariable() string {
	return "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

func (*MacOSVMEnvironment) JoinPathVariable(paths ...string) string {
	return strings.Join(paths, ":")
}

func (e *MacOSVMEnvironment) GetRunnerContext(context.Context) map[string]any {
	return map[string]any{
		"os":         "macOS",
		"arch":       goArchToActionArch(runtime.GOARCH),
		"temp":       e.TmpDir,
		"tool_cache": e.ToolCache,
	}
}

func (*MacOSVMEnvironment) IsEnvironmentCaseInsensitive() bool {
	return false
}

func (e *MacOSVMEnvironment) runExecutor(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	if stdout == nil {
		stdout = e.Stdout
	}
	if stderr == nil {
		stderr = e.Stderr
	}
	cmd := exec.CommandContext(ctx, e.executorPath, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("macOS VM executor %s: %w", args[0], err)
	}
	return nil
}
