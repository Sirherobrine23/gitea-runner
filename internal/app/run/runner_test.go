// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"testing"

	"gitea.com/gitea/runner/act/model"
	clientmocks "gitea.com/gitea/runner/internal/pkg/client/mocks"
	"gitea.com/gitea/runner/internal/pkg/config"
	"gitea.com/gitea/runner/internal/pkg/ver"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestRunnerCapabilitiesAndDeclare(t *testing.T) {
	require.Equal(t, []string{CapabilityCancelling}, RunnerCapabilities())

	cli := clientmocks.NewClient(t)
	cli.On("Declare", mock.Anything, mock.MatchedBy(func(req *connect.Request[runnerv1.DeclareRequest]) bool {
		return req.Msg.Version == ver.Version() &&
			len(req.Msg.Labels) == 1 &&
			req.Msg.Labels[0] == "ubuntu" &&
			len(req.Msg.Capabilities) == 1 &&
			req.Msg.Capabilities[0] == CapabilityCancelling
	})).Return(connect.NewResponse(&runnerv1.DeclareResponse{}), nil)

	r := &Runner{client: cli}
	_, err := r.Declare(context.Background(), []string{"ubuntu"})
	require.NoError(t, err)
}

func TestRunnerSetCapabilitiesFromDeclare(t *testing.T) {
	r := &Runner{}
	r.SetCapabilitiesFromDeclare(nil)
	require.Empty(t, r.capabilities)

	resp := connect.NewResponse(&runnerv1.DeclareResponse{})
	resp.Header().Set("X-Gitea-Actions-Capabilities", " cancelling,cache-v2 ")
	r.SetCapabilitiesFromDeclare(resp)
	require.Equal(t, "cancelling,cache-v2", r.capabilities)
}

func TestRunnerDefaultActionsURLUsesMirrorOnlyForGithub(t *testing.T) {
	r := &Runner{cfg: &config.Config{}}
	r.cfg.Runner.GithubMirror = "https://mirror.example"

	task := taskWithDefaultActionsURL("https://github.com")
	require.Equal(t, "https://mirror.example", r.getDefaultActionsURL(task))

	task = taskWithDefaultActionsURL("https://gitea.example")
	require.Equal(t, "https://gitea.example", r.getDefaultActionsURL(task))
}

func TestRunnerRunningCountAndNullLogger(t *testing.T) {
	r := &Runner{}
	require.Equal(t, int64(0), r.RunningCount())
	r.runningCount.Add(2)
	require.Equal(t, int64(2), r.RunningCount())

	logger := NullLogger{}.WithJobLogger()
	require.NotNil(t, logger)
	require.NotNil(t, logger.Out)
}

func TestNewRunnerInitializesLabelsAndEnvironment(t *testing.T) {
	cacheEnabled := false
	cfg := &config.Config{}
	cfg.Cache.Enabled = &cacheEnabled
	cfg.Runner.Envs = map[string]string{"EXISTING": "value"}
	reg := &config.Registration{
		Name:   "runner",
		Labels: []string{"ubuntu:host", "bad:vm:label"},
	}
	cli := clientmocks.NewClient(t)
	cli.On("Address").Return("https://gitea.example/").Maybe()

	r := NewRunner(cfg, reg, cli)

	require.Equal(t, "runner", r.name)
	require.Len(t, r.labels, 1)
	require.Equal(t, "value", r.envs["EXISTING"])
	require.Equal(t, "https://gitea.example/api/actions_pipeline/", r.envs["ACTIONS_RUNTIME_URL"])
	require.Equal(t, "https://gitea.example", r.envs["ACTIONS_RESULTS_URL"])
	require.Equal(t, "true", r.envs["GITEA_ACTIONS"])
	require.NotEmpty(t, r.envs["GITEA_ACTIONS_RUNNER_VERSION"])
	require.Nil(t, r.cacheHandler)
}

func TestApplyPullRequestTargetCheckoutContextUsesHeadSHA(t *testing.T) {
	preset := &model.GithubContext{
		EventName: "pull_request_target",
		Sha:       "base-sha",
		Ref:       "refs/heads/main",
		RefName:   "main",
		HeadRef:   "feature",
		Event: map[string]any{
			"pull_request": map[string]any{
				"head": map[string]any{
					"sha": "head-sha",
					"ref": "contributor-branch",
				},
			},
		},
	}

	applyPullRequestTargetCheckoutContext(preset)

	require.Equal(t, "head-sha", preset.Sha)
	require.Equal(t, "head-sha", preset.Ref)
	require.Equal(t, "contributor-branch", preset.RefName)
	require.Equal(t, "contributor-branch", preset.HeadRef)
}

func TestApplyPullRequestTargetCheckoutContextNoOpsWithoutHeadSHA(t *testing.T) {
	preset := &model.GithubContext{
		EventName: "pull_request_target",
		Sha:       "base-sha",
		Ref:       "refs/heads/main",
		RefName:   "main",
		Event:     map[string]any{},
	}

	applyPullRequestTargetCheckoutContext(preset)

	require.Equal(t, "base-sha", preset.Sha)
	require.Equal(t, "refs/heads/main", preset.Ref)
	require.Equal(t, "main", preset.RefName)
}

func TestApplyPullRequestTargetCheckoutContextNoOpsForOtherEvents(t *testing.T) {
	preset := &model.GithubContext{
		EventName: "pull_request",
		Sha:       "merge-sha",
		Ref:       "refs/pull/1/merge",
		Event: map[string]any{
			"pull_request": map[string]any{
				"head": map[string]any{
					"sha": "head-sha",
				},
			},
		},
	}

	applyPullRequestTargetCheckoutContext(preset)

	require.Equal(t, "merge-sha", preset.Sha)
	require.Equal(t, "refs/pull/1/merge", preset.Ref)
}

func taskWithDefaultActionsURL(url string) *runnerv1.Task {
	return &runnerv1.Task{
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"gitea_default_actions_url": structpb.NewStringValue(url),
			},
		},
	}
}
