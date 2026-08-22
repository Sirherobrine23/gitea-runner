// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gitea.com/gitea/runner/internal/act/common"
	"gitea.com/gitea/runner/internal/act/common/git"

	"gitea.dev/actionslib/pkg/model"
	gogit "github.com/go-git/go-git/v5"
)

type stepActionRemote struct {
	Step                *model.Step
	RunContext          *RunContext
	compositeRunContext *RunContext
	compositeSteps      *compositeSteps
	readAction          readAction
	runAction           runAction
	action              *model.Action
	env                 map[string]string
	remoteAction        *remoteAction
	cacheDir            string
	resolvedSha         string
	downloaded          bool // this step fetched the action repository, rather than reusing a fetch of the job
}

var stepActionRemoteNewCloneExecutor = git.NewGitCloneExecutor

// selfRepoPrefix introduces a self-repository reference: the action lives in the repo holding the file that wrote the `uses:`.
const selfRepoPrefix = "$/"

func (sar *stepActionRemote) prepareActionExecutor() common.Executor {
	return func(ctx context.Context) error {
		if sar.remoteAction != nil && sar.action != nil {
			// we are already good to run
			return nil
		}

		// For gitea:
		// Since actions can specify the download source via a url prefix.
		// The prefix may contain some sensitive information that needs to be stored in secrets,
		// so we need to interpolate the expression value for uses first.
		sar.Step.Uses = sar.RunContext.NewExpressionEvaluator(ctx).Interpolate(ctx, sar.Step.Uses)

		github := sar.getGithubContext(ctx) // read before remoteAction is set, so `$/` resolves against the enclosing action
		if strings.HasPrefix(sar.Step.Uses, selfRepoPrefix) {
			sar.remoteAction = newSelfRepoAction(sar.Step.Uses, github)
		} else {
			sar.remoteAction = newRemoteAction(sar.Step.Uses)
		}
		if sar.remoteAction == nil {
			return fmt.Errorf("Expected format {org}/{repo}[/path]@ref or %s{path}. Actual '%s' Input string was not in a correct format", selfRepoPrefix, sar.Step.Uses)
		}

		if sar.remoteAction.IsCheckout() && isLocalCheckout(github, sar.Step) && !sar.RunContext.Config.NoSkipCheckout {
			common.Logger(ctx).Debugf("Skipping local actions/checkout because workdir was already copied")
			return nil
		}

		for _, action := range sar.RunContext.Config.ReplaceGheActionWithGithubCom {
			if strings.EqualFold(fmt.Sprintf("%s/%s", sar.remoteAction.Org, sar.remoteAction.Repo), action) {
				sar.remoteAction.URL = "https://github.com"
				github.Token = sar.RunContext.Config.ReplaceGheActionTokenWithGithubCom
			}
		}

		downloads := sar.RunContext.downloadedActions()
		downloadKey := sar.downloadKey()

		// Actions served from the action cache are read out of a git object store rather than a
		// directory, so they never reach the bundle patch below and keep to the v1 cache API.
		if sar.RunContext.Config.ActionCache != nil {
			cache := sar.RunContext.Config.ActionCache

			sar.cacheDir = fmt.Sprintf("%s/%s", sar.remoteAction.Org, sar.remoteAction.Repo)
			sha, downloaded := downloads[downloadKey]
			if !downloaded {
				var err error
				repoURL := sar.remoteAction.URL + "/" + sar.cacheDir
				repoRef := sar.remoteAction.Ref
				if sha, err = cache.Fetch(ctx, sar.cacheDir, repoURL, repoRef, github.Token); err != nil {
					return fmt.Errorf("failed to fetch \"%s\" version \"%s\": %w", repoURL, repoRef, err)
				}
				downloads[downloadKey] = sha
				sar.downloaded = true
			}
			sar.resolvedSha = sha

			remoteReader := func(ctx context.Context) actionYamlReader {
				return func(filename string) (io.Reader, io.Closer, error) {
					spath := path.Join(sar.remoteAction.Path, filename)
					for range maxSymlinkDepth {
						tars, err := cache.GetTarArchive(ctx, sar.cacheDir, sar.resolvedSha, spath)
						if err != nil {
							return nil, nil, os.ErrNotExist
						}
						treader := tar.NewReader(tars)
						header, err := treader.Next()
						if err != nil {
							return nil, nil, os.ErrNotExist
						}
						if header.FileInfo().Mode()&os.ModeSymlink == os.ModeSymlink {
							spath, err = symlinkJoin(spath, header.Linkname, ".")
							if err != nil {
								return nil, nil, err
							}
						} else {
							return treader, tars, nil
						}
					}
					return nil, nil, fmt.Errorf("max depth %d of symlinks exceeded while reading %s", maxSymlinkDepth, spath)
				}
			}

			actionModel, err := sar.readAction(ctx, sar.Step, sar.resolvedSha, sar.remoteAction.Path, remoteReader(ctx), os.WriteFile)
			sar.action = actionModel
			return err
		}

		actionDir := sar.actionDir()
		var ntErr common.Executor
		sha, downloaded := downloads[downloadKey]
		if !downloaded {
			// For Gitea
			// A composite RunContext nils Config.Secrets, so getGitCloneToken would yield an
			// empty token and clone the action anonymously (401 against the authenticated
			// instance). github.Token survives the composite config copy and matches the
			// top-level token; keep the shouldCloneURLUseToken host gate to avoid leaking it.
			cloneURL := sar.remoteAction.CloneURL(sar.RunContext.Config.DefaultActionURL())
			token := ""
			if shouldCloneURLUseToken(sar.RunContext.Config.GitHubInstance, sar.RunContext.Config.trustedActionInstance(), cloneURL) {
				token = github.Token
			}
			gitClone := stepActionRemoteNewCloneExecutor(git.NewGitCloneExecutorInput{
				URL:         cloneURL,
				Ref:         sar.remoteAction.Ref,
				Dir:         actionDir,
				Token:       token,
				OfflineMode: sar.RunContext.Config.ActionOfflineMode,
				Depth:       sar.RunContext.Config.ActionCloneDepth,
				// printPrepareActions reports the download with its resolved commit.
				Quiet: true,

				InsecureSkipTLS: sar.cloneSkipTLS(), // For Gitea
			})
			if err := gitClone(ctx); err != nil {
				var refErr *git.Error
				if errors.As(err, &refErr) && errors.Is(err, git.ErrShortRef) {
					return fmt.Errorf("Unable to resolve action `%s`, the provided ref `%s` is the shortened version of a commit SHA, which is not supported. Please use the full commit SHA `%s` instead",
						sar.Step.Uses, sar.remoteAction.Ref, refErr.Commit())
				} else if errors.Is(err, gogit.ErrForceNeeded) { // TODO: figure out if it will be easy to shadow/alias go-git err's
					ntErr = common.NewInfoExecutor("Non-terminating error while running 'git clone': %v", err)
				} else {
					return err
				}
			}

			// Best effort: the download report falls back to the ref alone when the commit is unknown.
			if _, resolved, err := git.FindGitRevision(ctx, actionDir); err != nil {
				common.Logger(ctx).Debugf("unable to resolve the commit of %s: %v", sar.remoteAction.Reference(), err)
			} else {
				sha = resolved
			}
			downloads[downloadKey] = sha
			sar.downloaded = true
		}
		sar.resolvedSha = sha

		remoteReader := func(ctx context.Context) actionYamlReader { //nolint:unparam // pre-existing issue from nektos/act
			return func(filename string) (io.Reader, io.Closer, error) {
				f, err := os.Open(filepath.Join(actionDir, sar.remoteAction.Path, filename))
				return f, f, err
			}
		}

		return common.NewPipelineExecutor(
			ntErr,
			func(ctx context.Context) error {
				defer git.AcquireCloneLock(actionDir)()
				actionModel, err := sar.readAction(ctx, sar.Step, actionDir, sar.remoteAction.Path, remoteReader(ctx), os.WriteFile)
				sar.action = actionModel
				return err
			},
			// A stage of its own: it takes the same clone lock, and it has to land before
			// runAction copies the action into the job container.
			sar.patchActionToolkit,
		)(ctx)
	}
}

// actionDownloadInfo reports the repository this step downloaded and the commit it resolved to. ok
// is false when nothing was fetched, as for the local checkout of the workflow's own repository or
// for an action another step of the job already downloaded.
func (sar *stepActionRemote) actionDownloadInfo() (reference, sha string, ok bool) {
	if sar.remoteAction == nil || sar.action == nil || !sar.downloaded {
		return "", "", false
	}
	return sar.remoteAction.RepoReference(), sar.resolvedSha, true
}

func (sar *stepActionRemote) pre() common.Executor {
	sar.env = map[string]string{}

	return common.NewPipelineExecutor(
		sar.prepareActionExecutor(),
		runStepExecutor(sar, stepStagePre, sar.revertToolkitOnFailure(runPreStep(sar))).If(hasPreStep(sar)).If(shouldRunPreStep(sar)))
}

func (sar *stepActionRemote) main() common.Executor {
	return common.NewPipelineExecutor(
		sar.prepareActionExecutor(),
		runStepExecutor(sar, stepStageMain, func(ctx context.Context) error {
			printRunActionHeader(ctx, sar.Step, sar.env, sar.RunContext)
			rawLogger := common.Logger(ctx).WithField(rawOutputField, true)
			defer rawLogger.Infof("::endgroup::")

			github := sar.getGithubContext(ctx)
			if sar.remoteAction.IsCheckout() && isLocalCheckout(github, sar.Step) && !sar.RunContext.Config.NoSkipCheckout {
				if sar.RunContext.Config.BindWorkdir {
					common.Logger(ctx).Debugf("Skipping local actions/checkout because you bound your workspace")
					return nil
				}
				eval := sar.RunContext.NewExpressionEvaluator(ctx)
				copyToPath := path.Join(sar.RunContext.JobContainer.ToContainerPath(sar.RunContext.Config.Workdir), eval.Interpolate(ctx, sar.Step.With["path"]))
				return sar.RunContext.JobContainer.CopyDir(copyToPath, sar.RunContext.Config.Workdir+string(filepath.Separator)+".", sar.RunContext.Config.UseGitIgnore)(ctx)
			}

			actionDir := sar.actionDir()

			return sar.revertToolkitOnFailure(sar.runAction(sar, actionDir, sar.remoteAction))(ctx)
		}),
	)
}

func (sar *stepActionRemote) post() common.Executor {
	return runStepExecutor(sar, stepStagePost, sar.revertToolkitOnFailure(runPostStep(sar))).If(hasPostStep(sar)).If(shouldRunPostStep(sar))
}

// toolkitBundles is the repository's checkout, the action inside it, and the entrypoints the
// toolkit may live in.
func (sar *stepActionRemote) toolkitBundles() (dir, location string, scripts []string) {
	if sar.remoteAction == nil {
		return "", "", nil
	}
	dir = sar.actionDir()
	location = filepath.Join(dir, sar.remoteAction.Path)
	return dir, location, actionScriptPaths(location, sar.action)
}

// patchActionToolkit edits the bundled toolkit so it works against Gitea: the artifact actions
// stop refusing, and the cache client keeps to the cache server whichever API version it picks.
func (sar *stepActionRemote) patchActionToolkit(ctx context.Context) error {
	if sar.RunContext.Config.PatchToolkit {
		dir, location, scripts := sar.toolkitBundles()
		patchToolkit(ctx, dir, location, scripts)
	}
	return nil
}

// revertToolkitOnFailure restores the untouched bundles when the action fails, so a later job
// runs it as shipped rather than repeating a failure the patch may have caused.
func (sar *stepActionRemote) revertToolkitOnFailure(exec common.Executor) common.Executor {
	return func(ctx context.Context) error {
		err := exec(ctx)
		if err != nil {
			dir, location, scripts := sar.toolkitBundles()
			revertToolkit(ctx, dir, location, scripts)
		}
		return err
	}
}

// downloadKey is the clone URL and ref a step downloads, shared by every action path inside that
// repository, and distinct per instance serving it.
func (sar *stepActionRemote) downloadKey() string {
	return sar.remoteAction.CloneURL(sar.RunContext.Config.DefaultActionURL()) + "@" + sar.remoteAction.Ref
}

func (sar *stepActionRemote) actionDir() string {
	return fmt.Sprintf("%s/%s", sar.RunContext.ActionCacheDir(), model.UsesHash(sar.downloadKey()))
}

func (sar *stepActionRemote) getRunContext() *RunContext {
	return sar.RunContext
}

func (sar *stepActionRemote) getGithubContext(ctx context.Context) *model.GithubContext {
	ghc := sar.getRunContext().getGithubContext(ctx)

	// extend github context if we already have an initialized remoteAction
	remoteAction := sar.remoteAction
	if remoteAction != nil {
		ghc.ActionRepository = fmt.Sprintf("%s/%s", remoteAction.Org, remoteAction.Repo)
		ghc.ActionRef = remoteAction.Ref
	}

	return ghc
}

func (sar *stepActionRemote) getStepModel() *model.Step {
	return sar.Step
}

func (sar *stepActionRemote) getEnv() *map[string]string {
	return &sar.env
}

func (sar *stepActionRemote) getIfExpression(ctx context.Context, stage stepStage) string {
	switch stage {
	case stepStagePre:
		github := sar.getGithubContext(ctx)
		if sar.remoteAction.IsCheckout() && isLocalCheckout(github, sar.Step) && !sar.RunContext.Config.NoSkipCheckout {
			// skip local checkout pre step
			return "false"
		}
		return sar.action.Runs.PreIf
	case stepStageMain:
		return sar.Step.If.Value
	case stepStagePost:
		return sar.action.Runs.PostIf
	}
	return ""
}

func (sar *stepActionRemote) getActionModel() *model.Action {
	return sar.action
}

func (sar *stepActionRemote) getCompositeRunContext(ctx context.Context) *RunContext {
	if sar.compositeRunContext == nil {
		actionDir := sar.actionDir()
		actionLocation := path.Join(actionDir, sar.remoteAction.Path)
		_, containerActionDir := getContainerActionPaths(sar.getStepModel(), actionLocation, sar.RunContext)

		sar.compositeRunContext = newCompositeRunContext(ctx, sar.RunContext, sar, containerActionDir)
		sar.compositeSteps = sar.compositeRunContext.compositeExecutor(sar.action)
	} else {
		// Re-evaluate environment here. For remote actions the environment
		// need to be re-created for every stage (pre, main, post) as there
		// might be required context changes (inputs/outputs) while the action
		// stages are executed. (e.g. the output of another action is the
		// input for this action during the main stage, but the env
		// was already created during the pre stage)
		env := evaluateCompositeInputAndEnv(ctx, sar.RunContext, sar)
		sar.compositeRunContext.Env = env
		sar.compositeRunContext.ExtraPath = sar.RunContext.ExtraPath
	}
	return sar.compositeRunContext
}

func (sar *stepActionRemote) getCompositeSteps() *compositeSteps {
	return sar.compositeSteps
}

// For Gitea
// cloneSkipTLS returns true if the runner can clone an action from the Gitea instance
func (sar *stepActionRemote) cloneSkipTLS() bool {
	if !sar.RunContext.Config.InsecureSkipTLS {
		// Return false if the Gitea instance is not an insecure instance
		return false
	}
	if sar.remoteAction.URL == "" {
		// Empty URL means the default action instance should be used
		// Return true if the URL of the Gitea instance is the same as the URL of the default action instance
		return sar.RunContext.Config.DefaultActionURL() == sar.RunContext.Config.GitHubInstance
	}
	// Return true if the URL of the remote action is the same as the URL of the Gitea instance
	return sar.remoteAction.URL == sar.RunContext.Config.GitHubInstance
}

type remoteAction struct {
	URL  string
	Org  string
	Repo string
	Path string
	Ref  string
}

func (ra *remoteAction) CloneURL(u string) string {
	if ra.URL == "" {
		// keep an absolute local path as-is (used by tests to resolve actions from a local
		// repo); only bare host names get the https:// scheme prepended
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") && !filepath.IsAbs(u) {
			u = "https://" + u
		}
	} else {
		u = ra.URL
	}

	return fmt.Sprintf("%s/%s/%s", u, ra.Org, ra.Repo)
}

// Reference renders the action as {org}/{repo}[/path]@{ref}, omitting the download source, which
// can be interpolated from a secret.
func (ra *remoteAction) Reference() string {
	if ra.Path == "" {
		return ra.RepoReference()
	}
	return fmt.Sprintf("%s/%s/%s@%s", ra.Org, ra.Repo, ra.Path, ra.Ref)
}

// RepoReference renders the downloaded repository as {org}/{repo}@{ref}, the unit actions/runner
// downloads and reports, which carries no path inside the repository.
func (ra *remoteAction) RepoReference() string {
	return fmt.Sprintf("%s/%s@%s", ra.Org, ra.Repo, ra.Ref)
}

func (ra *remoteAction) IsCheckout() bool {
	if ra.Org == "actions" && ra.Repo == "checkout" {
		return true
	}
	return false
}

// newSelfRepoAction resolves `$/{path}` against the enclosing composite action, falling back to the workflow's own repo and commit.
func newSelfRepoAction(action string, github *model.GithubContext) *remoteAction {
	subPath := strings.TrimLeft(strings.TrimPrefix(action, selfRepoPrefix), "/")
	if subPath == "" || strings.Contains(subPath, "@") || path.Clean("/"+subPath) != "/"+subPath { // rooted, so a leading ".." is rejected too
		return nil
	}
	repo, ref := github.ActionRepository, github.ActionRef
	if repo == "" || ref == "" {
		repo, ref = github.Repository, github.Sha
	}
	org, name, _ := strings.Cut(repo, "/")
	if org == "" || name == "" || ref == "" {
		return nil
	}
	return &remoteAction{URL: github.ServerURL, Org: org, Repo: name, Path: subPath, Ref: ref}
}

func newRemoteAction(action string) *remoteAction {
	// support http(s)://host/owner/repo@v3
	for _, schema := range []string{"https://", "http://", "ssh://"} {
		if after, ok := strings.CutPrefix(action, schema); ok {
			splits := strings.SplitN(after, "/", 2)
			if len(splits) != 2 {
				return nil
			}
			ret := parseAction(splits[1])
			if ret == nil {
				return nil
			}
			ret.URL = schema + splits[0]
			return ret
		}
	}

	return parseAction(action)
}

func parseAction(action string) *remoteAction {
	// GitHub's document[^] describes:
	// > We strongly recommend that you include the version of
	// > the action you are using by specifying a Git ref, SHA, or Docker tag number.
	// Actually, the workflow stops if there is the uses directive that hasn't @ref.
	// [^]: https://docs.github.com/en/actions/reference/workflow-syntax-for-github-actions
	r := regexp.MustCompile(`^([^/@]+)/([^/@]+)(/([^@]*))?(@(.*))?$`)
	matches := r.FindStringSubmatch(action)
	if len(matches) < 7 || matches[6] == "" {
		return nil
	}
	return &remoteAction{
		Org:  matches[1],
		Repo: matches[2],
		Path: matches[4],
		Ref:  matches[6],
		URL:  "",
	}
}

func safeFilename(s string) string {
	return strings.NewReplacer(
		`<`, "-",
		`>`, "-",
		`:`, "-",
		`"`, "-",
		`/`, "-",
		`\`, "-",
		`|`, "-",
		`?`, "-",
		`*`, "-",
	).Replace(s)
}
