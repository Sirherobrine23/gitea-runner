// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/act/common"
	"gitea.com/gitea/runner/internal/act/container"

	_ "embed"

	"gitea.dev/actionslib/pkg/expreval"
	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"go.yaml.in/yaml/v4"
)

// ExpressionEvaluator is the interface for evaluating expressions
type ExpressionEvaluator interface {
	evaluate(context.Context, string, exprparser.DefaultStatusCheck) (any, error)
	interpolate(context.Context, string) (string, error)
	EvaluateYamlNode(context.Context, *yaml.Node) error
	Interpolate(context.Context, string) string
}

// NewExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewExpressionEvaluator(ctx context.Context) ExpressionEvaluator {
	return rc.NewExpressionEvaluatorWithEnv(ctx, rc.GetEnv())
}

func (rc *RunContext) NewExpressionEvaluatorWithEnv(ctx context.Context, env map[string]string) ExpressionEvaluator {
	var workflowCallResult map[string]*model.WorkflowCallResult

	// todo: cleanup EvaluationEnvironment creation
	using := make(map[string]exprparser.Needs)
	strategy := make(map[string]any)
	if rc.Run != nil {
		job := rc.Run.Job()
		if job != nil && job.Strategy != nil {
			strategy["fail-fast"] = job.Strategy.FailFast
			strategy["max-parallel"] = job.Strategy.MaxParallel
		}

		jobs := rc.Run.Workflow.Jobs
		jobNeeds := rc.Run.Job().Needs()

		for _, needs := range jobNeeds {
			using[needs] = exprparser.Needs{
				Outputs: jobs[needs].Outputs,
				Result:  jobs[needs].NeedsResult(),
			}
		}

		// only setup jobs context in case of workflow_call
		// and existing expression evaluator (this means, jobs are at
		// least ready to run)
		if rc.caller != nil && rc.ExprEval != nil {
			workflowCallResult = map[string]*model.WorkflowCallResult{}

			for jobName, job := range jobs {
				result := model.WorkflowCallResult{
					Outputs: map[string]string{},
				}
				maps.Copy(result.Outputs, job.Outputs)
				workflowCallResult[jobName] = &result
			}
		}
	}

	ghc := rc.getGithubContext(ctx)
	inputs := getEvaluatorInputs(ctx, rc, nil, ghc)

	ee := &exprparser.EvaluationEnvironment{
		Github: ghc,
		Env:    env,
		Job:    rc.getJobContext(),
		Jobs:   &workflowCallResult,
		// todo: should be unavailable
		// but required to interpolate/evaluate the step outputs on the job
		Steps:     rc.getStepsContext(),
		Secrets:   getWorkflowSecrets(ctx, rc),
		Vars:      getWorkflowVars(ctx, rc),
		Strategy:  strategy,
		Matrix:    rc.Matrix,
		Needs:     using,
		Inputs:    inputs,
		HashFiles: getHashFilesFunction(ctx, rc),
	}
	ee.Runner = rc.getRunnerContext(ctx)
	return expressionEvaluator{
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    "job",
		}),
	}
}

//go:embed hashfiles/index.js
var hashfiles string

// NewStepExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewStepExpressionEvaluator(ctx context.Context, step step) ExpressionEvaluator {
	// todo: cleanup EvaluationEnvironment creation
	job := rc.Run.Job()
	strategy := make(map[string]any)
	if job.Strategy != nil {
		strategy["fail-fast"] = job.Strategy.FailFast
		strategy["max-parallel"] = job.Strategy.MaxParallel
	}

	jobs := rc.Run.Workflow.Jobs
	jobNeeds := rc.Run.Job().Needs()

	using := make(map[string]exprparser.Needs)
	for _, needs := range jobNeeds {
		using[needs] = exprparser.Needs{
			Outputs: jobs[needs].Outputs,
			Result:  jobs[needs].NeedsResult(),
		}
	}

	ghc := rc.getGithubContext(ctx)
	inputs := getEvaluatorInputs(ctx, rc, step, ghc)

	ee := &exprparser.EvaluationEnvironment{
		Github:   step.getGithubContext(ctx),
		Env:      *step.getEnv(),
		Job:      rc.getJobContext(),
		Steps:    rc.getStepsContext(),
		Secrets:  getWorkflowSecrets(ctx, rc),
		Vars:     getWorkflowVars(ctx, rc),
		Strategy: strategy,
		Matrix:   rc.Matrix,
		Needs:    using,
		// todo: should be unavailable
		// but required to interpolate/evaluate the inputs in actions/composite
		Inputs:    inputs,
		HashFiles: getHashFilesFunction(ctx, rc),
	}
	ee.Runner = rc.getRunnerContext(ctx)
	return expressionEvaluator{
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    "step",
		}),
	}
}

func getHashFilesFunction(ctx context.Context, rc *RunContext) func(v []reflect.Value) (any, error) {
	hashFiles := func(v []reflect.Value) (any, error) {
		if rc.JobContainer != nil {
			timeed, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			name := "workflow/hashfiles/index.js"
			hout := &bytes.Buffer{}
			herr := &bytes.Buffer{}
			patterns := []string{}
			followSymlink := false

			for i, p := range v {
				s := p.String()
				if i == 0 {
					if strings.HasPrefix(s, "--") {
						if strings.EqualFold(s, "--follow-symbolic-links") {
							followSymlink = true
							continue
						}
						return "", fmt.Errorf("Invalid glob option %s, available option: '--follow-symbolic-links'", s)
					}
				}
				patterns = append(patterns, s)
			}
			env := map[string]string{}
			maps.Copy(env, rc.Env)
			env["patterns"] = strings.Join(patterns, "\n")
			if followSymlink {
				env["followSymbolicLinks"] = "true"
			}

			stdout, stderr := rc.JobContainer.ReplaceLogWriter(hout, herr)
			_ = rc.JobContainer.Copy(rc.JobContainer.GetActPath(), &container.FileEntry{
				Name: name,
				Mode: 0o644,
				Body: hashfiles,
			}).
				Then(rc.execJobContainer([]string{"node", path.Join(rc.JobContainer.GetActPath(), name)},
					env, "", "")).
				Finally(func(context.Context) error {
					rc.JobContainer.ReplaceLogWriter(stdout, stderr)
					return nil
				})(timeed)
			output := hout.String() + "\n" + herr.String()
			guard := "__OUTPUT__"
			outstart := strings.Index(output, guard)
			if outstart != -1 {
				outstart += len(guard)
				outend := strings.Index(output[outstart:], guard)
				if outend != -1 {
					return output[outstart : outstart+outend], nil
				}
			}
		}
		return "", nil
	}
	return hashFiles
}

type expressionEvaluator struct {
	interpreter exprparser.Interpreter
}

func (ee expressionEvaluator) evaluate(ctx context.Context, in string, defaultStatusCheck exprparser.DefaultStatusCheck) (any, error) {
	logger := common.Logger(ctx)
	logger.Debugf("evaluating expression '%s'", in)
	evaluated, err := ee.interpreter.Evaluate(in, defaultStatusCheck)

	// evaluated is an any: %t renders everything but a bool as "%!t(string=...)"
	printable := regexp.MustCompile(`::add-mask::.*`).ReplaceAllString(fmt.Sprintf("%v", evaluated), "::add-mask::***)")
	logger.Debugf("expression '%s' evaluated to '%s'", in, printable)

	return evaluated, err
}

// shared returns the evaluation layer of the shared library, bound to this context so the
// evaluation of every single expression is still logged and masked here.
func (ee expressionEvaluator) shared(ctx context.Context) expreval.Evaluator {
	return expreval.New(func(in string, defaultStatusCheck exprparser.DefaultStatusCheck) (any, error) {
		return ee.evaluate(ctx, in, defaultStatusCheck)
	})
}

func (ee expressionEvaluator) EvaluateYamlNode(ctx context.Context, node *yaml.Node) error {
	return ee.shared(ctx).EvaluateYamlNode(node)
}

func (ee expressionEvaluator) Interpolate(ctx context.Context, in string) string {
	out, err := ee.interpolate(ctx, in)
	if err != nil {
		common.Logger(ctx).Errorf("Unable to interpolate expression '%s': %s", in, err)
		return ""
	}
	return out
}

func (ee expressionEvaluator) interpolate(ctx context.Context, in string) (string, error) {
	return ee.shared(ctx).Interpolate(in)
}

// EvalBool evaluates an expression against given evaluator. An `if:` is an expression even without
// `${{ }}`, while literal text around one makes the whole value a string.
func EvalBool(ctx context.Context, evaluator ExpressionEvaluator, expr string, defaultStatusCheck exprparser.DefaultStatusCheck) (bool, error) {
	return expreval.New(func(in string, dsc exprparser.DefaultStatusCheck) (any, error) {
		return evaluator.evaluate(ctx, in, dsc)
	}).EvalBool(expr, defaultStatusCheck)
}

func getEvaluatorInputs(ctx context.Context, rc *RunContext, step step, ghc *model.GithubContext) map[string]any {
	inputs := map[string]any{}

	setupWorkflowInputs(ctx, &inputs, rc)

	var env map[string]string
	if step != nil {
		env = *step.getEnv()
	} else {
		env = rc.GetEnv()
	}

	for k, v := range env {
		if after, ok := strings.CutPrefix(k, "INPUT_"); ok {
			inputs[strings.ToLower(after)] = v
		}
	}

	if ghc.EventName == "workflow_dispatch" {
		config := rc.Run.Workflow.WorkflowDispatchConfig()
		if config != nil && config.Inputs != nil {
			for k, v := range config.Inputs {
				value := nestedMapLookup(ghc.Event, "inputs", k)
				if value == nil {
					value = v.Default
				}
				inputs[k] = coerceInputValue(value, v.Type)
			}
		}
	}

	if ghc.EventName == "workflow_call" {
		config := rc.Run.Workflow.WorkflowCallConfig()
		if config != nil && config.Inputs != nil {
			for k, v := range config.Inputs {
				value := nestedMapLookup(ghc.Event, "inputs", k)
				if value == nil {
					value = v.Default
				}
				inputs[k] = coerceInputValue(value, v.Type)
			}
		}
	}
	return inputs
}

// coerceInputValue converts an input value to the type declared by the workflow.
// The event payload carries natively typed JSON values on newer Gitea versions,
// while defaults and older servers provide strings.
func coerceInputValue(value any, inputType string) any {
	if inputType != "boolean" {
		return value
	}
	if b, ok := value.(bool); ok {
		return b
	}
	return value == "true"
}

func setupWorkflowInputs(ctx context.Context, inputs *map[string]any, rc *RunContext) {
	if rc.caller != nil {
		config := rc.Run.Workflow.WorkflowCallConfig()

		for name, input := range config.Inputs {
			value := rc.caller.runContext.Run.Job().With[name]
			if value != nil {
				if str, ok := value.(string); ok {
					// evaluate using the calling RunContext (outside)
					value = rc.caller.runContext.ExprEval.Interpolate(ctx, str)
				}
			}

			if value == nil && config != nil && config.Inputs != nil {
				value = input.Default
				if rc.ExprEval != nil {
					if str, ok := value.(string); ok {
						// evaluate using the called RunContext (inside)
						value = rc.ExprEval.Interpolate(ctx, str)
					}
				}
			}

			(*inputs)[name] = coerceInputValue(value, input.Type)
		}
	}
}

func getWorkflowSecrets(ctx context.Context, rc *RunContext) map[string]string {
	if rc.caller != nil {
		job := rc.caller.runContext.Run.Job()
		secrets := job.Secrets()

		if secrets == nil && job.InheritSecrets() {
			secrets = rc.caller.runContext.Config.Secrets
		}

		// Interpolate into a new map. secrets may be the shared Config.Secrets (or the job's
		// map), which other parallel jobs read concurrently (e.g. log masking), so mutating it
		// in place is a data race.
		interpolated := make(map[string]string, len(secrets))
		for k, v := range secrets {
			interpolated[k] = rc.caller.runContext.ExprEval.Interpolate(ctx, v)
		}

		return interpolated
	}

	return rc.Config.Secrets
}

func getWorkflowVars(_ context.Context, rc *RunContext) map[string]string {
	return rc.Config.Vars
}
