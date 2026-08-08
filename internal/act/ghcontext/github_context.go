// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package ghcontext fills a model.GithubContext from the local git checkout.
// The pure data parts of the context live in the shared
// gitea.dev/actionslib/pkg/model package, only the helpers that need a
// git repository on disk are kept here.
package ghcontext

import (
	"context"
	"fmt"
	"strings"

	"gitea.com/gitea/runner/internal/act/common"
	"gitea.com/gitea/runner/internal/act/common/git"

	"gitea.dev/actionslib/pkg/model"
)

var (
	findGitRef      = git.FindGitRef
	findGitRevision = git.FindGitRevision
	findGithubRepo  = git.FindGithubRepo
)

func withDefaultBranch(ctx context.Context, b string, event map[string]any) map[string]any {
	repoI, ok := event["repository"]
	if !ok {
		repoI = make(map[string]any)
	}

	repo, ok := repoI.(map[string]any)
	if !ok {
		common.Logger(ctx).Warnf("unable to set default branch to %v", b)
		return event
	}

	// if the branch is already there return with no changes
	if _, ok = repo["default_branch"]; ok {
		return event
	}

	repo["default_branch"] = b
	event["repository"] = repo

	return event
}

// SetRef resolves the ref of the context from its event payload, falling back
// to the ref checked out in repoPath.
func SetRef(ctx context.Context, ghc *model.GithubContext, defaultBranch, repoPath string) {
	logger := common.Logger(ctx)

	// https://docs.github.com/en/actions/learn-github-actions/events-that-trigger-workflows
	// https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads
	switch ghc.EventName {
	case "pull_request_target":
		ghc.Ref = "refs/heads/" + ghc.BaseRef
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		ghc.Ref = fmt.Sprintf("refs/pull/%.0f/merge", ghc.Event["number"])
	case "deployment", "deployment_status":
		ghc.Ref = model.AsString(model.NestedMapLookup(ghc.Event, "deployment", "ref"))
	case "release":
		ghc.Ref = "refs/tags/" + model.AsString(model.NestedMapLookup(ghc.Event, "release", "tag_name"))
	case "push", "create", "workflow_dispatch":
		ghc.Ref = model.AsString(ghc.Event["ref"])
	default:
		defaultBranch := model.AsString(model.NestedMapLookup(ghc.Event, "repository", "default_branch"))
		if defaultBranch != "" {
			ghc.Ref = "refs/heads/" + defaultBranch
		}
	}

	if ghc.Ref == "" {
		ref, err := findGitRef(ctx, repoPath)
		if err != nil {
			logger.Warningf("unable to get git ref: %v", err)
		} else {
			logger.Debugf("using github ref: %s", ref)
			ghc.Ref = ref
		}

		// set the branch in the event data
		if defaultBranch != "" {
			ghc.Event = withDefaultBranch(ctx, defaultBranch, ghc.Event)
		} else {
			ghc.Event = withDefaultBranch(ctx, "master", ghc.Event)
		}

		if ghc.Ref == "" {
			ghc.Ref = "refs/heads/" + model.AsString(model.NestedMapLookup(ghc.Event, "repository", "default_branch"))
		}
	}
}

// SetSha resolves the commit of the context from its event payload, falling
// back to the revision checked out in repoPath.
func SetSha(ctx context.Context, ghc *model.GithubContext, repoPath string) {
	logger := common.Logger(ctx)

	// https://docs.github.com/en/actions/learn-github-actions/events-that-trigger-workflows
	// https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads
	switch ghc.EventName {
	case "pull_request_target":
		ghc.Sha = model.AsString(model.NestedMapLookup(ghc.Event, "pull_request", "base", "sha"))
	case "deployment", "deployment_status":
		ghc.Sha = model.AsString(model.NestedMapLookup(ghc.Event, "deployment", "sha"))
	case "push", "create", "workflow_dispatch":
		if deleted, ok := ghc.Event["deleted"].(bool); ok && !deleted {
			ghc.Sha = model.AsString(ghc.Event["after"])
		}
	}

	if ghc.Sha == "" {
		_, sha, err := findGitRevision(ctx, repoPath)
		if err != nil {
			logger.Warningf("unable to get git revision: %v", err)
		} else {
			ghc.Sha = sha
		}
	}
}

// SetRepositoryAndOwner resolves the repository of the context from the git
// remote in repoPath when it is not set yet, and derives its owner.
func SetRepositoryAndOwner(ctx context.Context, ghc *model.GithubContext, githubInstance, remoteName, repoPath string) {
	if ghc.Repository == "" {
		repo, err := findGithubRepo(ctx, repoPath, githubInstance, remoteName)
		if err != nil {
			common.Logger(ctx).Warningf("unable to get git repo (githubInstance: %v; remoteName: %v, repoPath: %v): %v", githubInstance, remoteName, repoPath, err)
			return
		}
		ghc.Repository = repo
	}
	ghc.RepositoryOwner = strings.Split(ghc.Repository, "/")[0]
}
