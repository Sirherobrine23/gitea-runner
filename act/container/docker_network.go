// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2023 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/moby/moby/client"
)

var (
	dockerNetworkRemoveRetryInterval = 200 * time.Millisecond
	dockerNetworkRemoveTimeout       = 10 * time.Second
)

type dockerNetworkClient interface {
	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
}

func NewDockerNetworkCreateExecutor(name string) common.Executor {
	return func(ctx context.Context) error {
		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}
		defer cli.Close()

		// Only create the network if it doesn't exist
		networks, err := cli.NetworkList(ctx, client.NetworkListOptions{})
		if err != nil {
			return err
		}
		// For Gitea, reduce log noise
		// common.Logger(ctx).Debugf("%v", networks)
		for _, n := range networks.Items {
			if n.Name == name {
				common.Logger(ctx).Debugf("Network %v exists", name)
				return nil
			}
		}

		_, err = cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
			Driver: "bridge",
			Scope:  "local",
		})
		if err != nil {
			return err
		}

		return nil
	}
}

func NewDockerNetworkRemoveExecutor(name string) common.Executor {
	return func(ctx context.Context) error {
		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}
		defer cli.Close()

		return removeDockerNetworks(ctx, cli, name)
	}
}

func removeDockerNetworks(ctx context.Context, cli dockerNetworkClient, name string) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, dockerNetworkRemoveTimeout)
	defer cancel()

	for {
		pendingRemoval, err := removeDockerNetworksOnce(cleanupCtx, cli, name)
		if err != nil {
			return err
		}
		if !pendingRemoval {
			return nil
		}

		select {
		case <-cleanupCtx.Done():
			common.Logger(ctx).Warnf("Timed out waiting for Docker network %v endpoints to detach; leaving network behind", name)
			return nil
		case <-time.After(dockerNetworkRemoveRetryInterval):
		}
	}
}

func removeDockerNetworksOnce(ctx context.Context, cli dockerNetworkClient, name string) (bool, error) {
	// Make sure that all network of the specified name are removed.
	// cli.NetworkRemove refuses to remove a network if there are duplicates.
	networks, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return false, err
	}
	// For Gitea, reduce log noise
	// common.Logger(ctx).Debugf("%v", networks)

	pendingRemoval := false
	for _, n := range networks.Items {
		if n.Name != name {
			continue
		}

		result, err := cli.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
		if err != nil {
			return false, err
		}

		if len(result.Network.Containers) != 0 {
			pendingRemoval = true
			common.Logger(ctx).Debugf("Waiting to remove network %v because it still has active endpoints", name)
			continue
		}

		if _, err = cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil {
			pendingRemoval = true
			common.Logger(ctx).Debugf("Retrying Docker network removal for %v: %v", name, err)
		}
	}

	return pendingRemoval, nil
}
