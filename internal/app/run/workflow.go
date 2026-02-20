// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/actions-oss/act-cli/pkg/model"
	"github.com/actions-oss/act-cli/pkg/schema"
	"gopkg.in/yaml.v3"
)

func generateWorkflow(task *runnerv1.Task) (*model.Workflow, string, error) {
	workflow, err := model.ReadWorkflow(bytes.NewReader(task.WorkflowPayload), model.WorkflowConfig{
		// Schema: schema.GetGiteaWorkflowSchema(),
		// Allow everything
		Schema: &schema.Schema{
			Definitions: map[string]schema.Definition{
				"workflow-root": {
					Context: []string{"github", "gitea", "env", "job", "matrix", "inputs", "vars", "runner", "steps"},
					OneOf:   &[]string{"any"},
				},
			},
		},
	})
	if err != nil {
		return nil, "", err
	}

	jobIDs := workflow.GetJobIDs()
	if len(jobIDs) != 1 {
		return nil, "", fmt.Errorf("multiple jobs found: %v", jobIDs)
	}
	jobID := jobIDs[0]

	needJobIDs := make([]string, 0, len(task.Needs))
	for id, need := range task.Needs {
		needJobIDs = append(needJobIDs, id)
		needJob := &model.Job{
			Outputs: need.Outputs,
			Result:  strings.ToLower(strings.TrimPrefix(need.Result.String(), "RESULT_")),
		}
		workflow.Jobs[id] = needJob
	}
	sort.Strings(needJobIDs)

	rawNeeds := yaml.Node{
		Kind:    yaml.SequenceNode,
		Content: make([]*yaml.Node, 0, len(needJobIDs)),
	}
	for _, id := range needJobIDs {
		rawNeeds.Content = append(rawNeeds.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: id,
		})
	}

	// TODO GITEA
	workflow.Jobs[jobID].RawNeeds = rawNeeds
	workflow.Jobs[jobID].RawRunsOn.Encode("dummy")

	return workflow, jobID, nil
}
