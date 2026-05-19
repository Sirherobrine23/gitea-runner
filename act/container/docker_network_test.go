// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2026 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"testing"
	"time"

	containernetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDockerNetworkClient struct {
	listResult   client.NetworkListResult
	inspectByID  map[string][]client.NetworkInspectResult
	inspectCalls map[string]int
	removeCalls  []string
	removeErrs   map[string][]error
	removeIdx    map[string]int
}

func (f *fakeDockerNetworkClient) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return f.listResult, nil
}

func (f *fakeDockerNetworkClient) NetworkInspect(_ context.Context, networkID string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	idx := f.inspectCalls[networkID]
	f.inspectCalls[networkID] = idx + 1
	results := f.inspectByID[networkID]
	if len(results) == 0 {
		return client.NetworkInspectResult{}, nil
	}
	if idx >= len(results) {
		return results[len(results)-1], nil
	}
	return results[idx], nil
}

func (f *fakeDockerNetworkClient) NetworkRemove(_ context.Context, networkID string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.removeCalls = append(f.removeCalls, networkID)
	idx := f.removeIdx[networkID]
	f.removeIdx[networkID] = idx + 1
	if errs := f.removeErrs[networkID]; idx < len(errs) {
		return client.NetworkRemoveResult{}, errs[idx]
	}
	return client.NetworkRemoveResult{}, nil
}

func TestRemoveDockerNetworksRetriesUntilEndpointsDetach(t *testing.T) {
	originalInterval := dockerNetworkRemoveRetryInterval
	originalTimeout := dockerNetworkRemoveTimeout
	dockerNetworkRemoveRetryInterval = time.Millisecond
	dockerNetworkRemoveTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		dockerNetworkRemoveRetryInterval = originalInterval
		dockerNetworkRemoveTimeout = originalTimeout
	})

	cli := &fakeDockerNetworkClient{
		listResult: client.NetworkListResult{
			Items: []containernetwork.Summary{{Network: containernetwork.Network{ID: "n1", Name: "test"}}},
		},
		inspectByID: map[string][]client.NetworkInspectResult{
			"n1": {
				{Network: containernetwork.Inspect{Containers: map[string]containernetwork.EndpointResource{"c1": {}}}},
				{Network: containernetwork.Inspect{Containers: map[string]containernetwork.EndpointResource{}}},
			},
		},
		inspectCalls: map[string]int{},
		removeErrs:   map[string][]error{},
		removeIdx:    map[string]int{},
	}

	err := removeDockerNetworks(context.Background(), cli, "test")
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, cli.removeCalls)
	assert.GreaterOrEqual(t, cli.inspectCalls["n1"], 2)
}

func TestRemoveDockerNetworksStopsRetryingAfterTimeout(t *testing.T) {
	originalInterval := dockerNetworkRemoveRetryInterval
	originalTimeout := dockerNetworkRemoveTimeout
	dockerNetworkRemoveRetryInterval = time.Millisecond
	dockerNetworkRemoveTimeout = 5 * time.Millisecond
	t.Cleanup(func() {
		dockerNetworkRemoveRetryInterval = originalInterval
		dockerNetworkRemoveTimeout = originalTimeout
	})

	cli := &fakeDockerNetworkClient{
		listResult: client.NetworkListResult{
			Items: []containernetwork.Summary{{Network: containernetwork.Network{ID: "n1", Name: "test"}}},
		},
		inspectByID: map[string][]client.NetworkInspectResult{
			"n1": {
				{Network: containernetwork.Inspect{Containers: map[string]containernetwork.EndpointResource{"c1": {}}}},
			},
		},
		inspectCalls: map[string]int{},
		removeErrs:   map[string][]error{},
		removeIdx:    map[string]int{},
	}

	err := removeDockerNetworks(context.Background(), cli, "test")
	require.NoError(t, err)
	assert.Empty(t, cli.removeCalls)
	assert.Positive(t, cli.inspectCalls["n1"])
}
