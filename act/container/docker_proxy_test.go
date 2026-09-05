// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (m *mockDockerClient) VolumeList(ctx context.Context, opts mobyclient.VolumeListOptions) (mobyclient.VolumeListResult, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(mobyclient.VolumeListResult), args.Error(1)
}

func (m *mockDockerClient) VolumeRemove(ctx context.Context, id string, opts mobyclient.VolumeRemoveOptions) (mobyclient.VolumeRemoveResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.VolumeRemoveResult), args.Error(1)
}

// unix socket paths are limited to about a hundred bytes, which the default macOS TMPDIR exceeds
func shortTempDir(t testing.TB) string {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	return t.TempDir()
}

func daemonSocketPath(t testing.TB, cli mobyclient.APIClient) string {
	t.Helper()
	host := cli.DaemonHost()
	if !strings.HasPrefix(host, "unix://") {
		t.Skipf("skipping: daemon at %s is not a unix socket", host)
	}
	return strings.TrimPrefix(host, "unix://")
}

func TestDockerProxy(t *testing.T) {
	bodies := make(chan []byte, 8)
	daemonSocket := filepath.Join(shortTempDir(t), "d.sock")
	listener, err := net.Listen("unix", daemonSocket)
	require.NoError(t, err)
	daemon := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/create"):
			body, _ := io.ReadAll(r.Body)
			bodies <- body
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/start"):
			conn, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n"))
			_, _ = io.Copy(conn, buffered)
		default:
			w.Header().Set("Api-Version", "1.47")
			_, _ = w.Write([]byte("OK " + r.Method + " " + r.URL.Path))
		}
	})}
	go func() { _ = daemon.Serve(listener) }()
	t.Cleanup(func() { _ = daemon.Close() })
	proxy, err := StartDockerProxy(daemonSocket, shortTempDir(t), "job-1")
	require.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", proxy.Socket)
	}}}

	t.Run("labels creates", func(t *testing.T) {
		for path, body := range map[string]string{
			"/v1.47/containers/create": `{"Image":"alpine","Labels":{"own":"1"}}`,
			"/networks/create":         `{"Name":"n"}`,
			"/volumes/create":          `{"Name":"v","labels":null}`,
		} {
			resp, err := client.Post("http://docker"+path, "application/json", strings.NewReader(body))
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusCreated, resp.StatusCode)

			var got map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(<-bodies, &got))
			key := "Labels"
			if _, ok := got["labels"]; ok {
				key = "labels"
			}
			var labels map[string]string
			require.NoError(t, json.Unmarshal(got[key], &labels))
			assert.Equal(t, "job-1", labels[jobLabel], path)
			if strings.Contains(body, "own") {
				assert.Equal(t, "1", labels["own"])
				assert.JSONEq(t, `"alpine"`, string(got["Image"]))
			}
		}
	})

	t.Run("passes other requests through", func(t *testing.T) {
		resp, err := client.Get("http://docker/v1.47/_ping")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "1.47", resp.Header.Get("Api-Version"))
		assert.Equal(t, "OK GET /v1.47/_ping", string(body))
	})

	t.Run("tunnels raw streams", func(t *testing.T) {
		conn, err := net.Dial("unix", proxy.Socket)
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.Write([]byte("POST /v1.47/exec/abc/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}ping\n"))
		require.NoError(t, err)
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, "application/vnd.docker.raw-stream", resp.Header.Get("Content-Type"))
		echoed, err := reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "{}ping\n", echoed)
	})
}

func TestRemoveLabelledRemovesContainersNetworksAndVolumes(t *testing.T) {
	ctx := context.Background()
	filters := make(mobyclient.Filters).Add("label", jobLabel+"=job-1")
	cli := &mockDockerClient{}
	cli.On("ContainerList", ctx, mobyclient.ContainerListOptions{All: true, Filters: filters}).
		Return(mobyclient.ContainerListResult{Items: []container.Summary{{ID: "c1", Names: []string{"/app"}}}}, nil)
	cli.On("ContainerKill", ctx, "c1", mock.Anything).Return(mobyclient.ContainerKillResult{}, nil)
	cli.On("ContainerRemove", ctx, "c1", mobyclient.ContainerRemoveOptions{RemoveVolumes: true, Force: true}).
		Return(mobyclient.ContainerRemoveResult{}, nil)
	cli.On("NetworkList", ctx, mobyclient.NetworkListOptions{Filters: filters}).
		Return(mobyclient.NetworkListResult{Items: []network.Summary{{ID: "n1", Name: "app_default"}}}, nil)
	cli.On("NetworkRemove", ctx, "n1", mock.Anything).Return(mobyclient.NetworkRemoveResult{}, nil)
	cli.On("VolumeList", ctx, mobyclient.VolumeListOptions{Filters: filters}).
		Return(mobyclient.VolumeListResult{Items: []volume.Volume{{Name: "app_data"}}}, nil)
	cli.On("VolumeRemove", ctx, "app_data", mobyclient.VolumeRemoveOptions{}).Return(mobyclient.VolumeRemoveResult{}, nil)

	require.NoError(t, removeLabelled(ctx, cli, "job-1"))
	cli.AssertExpectations(t)
}

func TestDockerProxyWithDaemon(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	require.NoError(t, NewDockerPullExecutor(NewDockerPullExecutorInput{Image: "alpine"})(ctx))
	direct, err := GetDockerClient(ctx)
	require.NoError(t, err)
	defer direct.Close()
	dir := shortTempDir(t)
	seen, err := daemonSeesDir(ctx, dir)
	require.NoError(t, err)
	t.Logf("daemon sees the runner's filesystem: %v", seen)

	job := "proxy-test-" + t.Name()
	proxy, err := StartDockerProxy(daemonSocketPath(t, direct), dir, job)
	require.NoError(t, err)
	viaProxy, err := mobyclient.New(mobyclient.WithHost("unix://" + proxy.Socket))
	require.NoError(t, err)
	defer viaProxy.Close()

	created, err := viaProxy.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &container.Config{Image: "alpine", Cmd: []string{"sleep", "300"}, Labels: map[string]string{"own": "1"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = direct.ContainerRemove(ctx, created.ID, mobyclient.ContainerRemoveOptions{Force: true}) })
	_, err = viaProxy.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{})
	require.NoError(t, err)
	net, err := viaProxy.NetworkCreate(ctx, job, mobyclient.NetworkCreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = direct.NetworkRemove(ctx, net.ID, mobyclient.NetworkRemoveOptions{}) })
	_, err = viaProxy.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{Name: job})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = direct.VolumeRemove(ctx, job, mobyclient.VolumeRemoveOptions{Force: true}) })

	inspected, err := direct.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"own": "1", jobLabel: job}, inspected.Container.Config.Labels)
	vol, err := direct.VolumeInspect(ctx, job, mobyclient.VolumeInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, job, vol.Volume.Labels[jobLabel])

	exec, err := viaProxy.ExecCreate(ctx, created.ID, mobyclient.ExecCreateOptions{Cmd: []string{"cat"}, AttachStdin: true, AttachStdout: true, TTY: true})
	require.NoError(t, err)
	attached, err := viaProxy.ExecAttach(ctx, exec.ID, mobyclient.ExecAttachOptions{TTY: true})
	require.NoError(t, err)
	_, err = attached.Conn.Write([]byte("hello\n"))
	require.NoError(t, err)
	echoed, err := attached.Reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "hello\r\n", echoed)
	attached.Close()

	require.NoError(t, proxy.Close(ctx))
	_, err = direct.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err))
	_, err = direct.NetworkInspect(ctx, net.ID, mobyclient.NetworkInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err))
	_, err = direct.VolumeInspect(ctx, job, mobyclient.VolumeInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err))
}

func BenchmarkDockerProxy(b *testing.B) {
	ctx := context.Background()
	direct, err := GetDockerClient(ctx)
	require.NoError(b, err)
	defer direct.Close()
	if _, err := direct.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		b.Skipf("docker daemon unreachable: %v", err)
	}
	proxy, err := StartDockerProxy(daemonSocketPath(b, direct), shortTempDir(b), "bench")
	require.NoError(b, err)
	defer func() { _ = proxy.Close(ctx) }()
	viaProxy, err := mobyclient.New(mobyclient.WithHost("unix://" + proxy.Socket))
	require.NoError(b, err)
	defer viaProxy.Close()

	require.NoError(b, NewDockerPullExecutor(NewDockerPullExecutorInput{Image: "alpine"})(ctx))
	created, err := direct.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{Config: &container.Config{Image: "alpine", Cmd: []string{"sleep", "600"}}})
	require.NoError(b, err)
	defer func() { _, _ = direct.ContainerRemove(ctx, created.ID, mobyclient.ContainerRemoveOptions{Force: true}) }()
	_, err = direct.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{})
	require.NoError(b, err)

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	payload := make([]byte, 64<<20)
	require.NoError(b, writer.WriteHeader(&tar.Header{Name: "blob", Mode: 0o600, Size: int64(len(payload))}))
	_, _ = writer.Write(payload)
	require.NoError(b, writer.Close())

	for name, cli := range map[string]mobyclient.APIClient{"direct": direct, "proxy": viaProxy} {
		b.Run("ping/"+name, func(b *testing.B) {
			for b.Loop() {
				if _, err := cli.Ping(ctx, mobyclient.PingOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("copy64MiB/"+name, func(b *testing.B) {
			b.SetBytes(int64(archive.Len()))
			for b.Loop() {
				_, err := cli.CopyToContainer(ctx, created.ID, mobyclient.CopyToContainerOptions{DestinationPath: "/tmp", Content: bytes.NewReader(archive.Bytes())})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
