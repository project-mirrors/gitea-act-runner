// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"go.yaml.in/yaml/v4"
)

func generateWorkflow(task *runnerv1.Task) (*model.Workflow, string, error) {
	workflow, err := model.ReadWorkflowPayload(bytes.NewReader(task.WorkflowPayload))
	if err != nil {
		return nil, "", err
	}

	jobIDs := workflow.GetJobIDs()
	if len(jobIDs) != 1 {
		return nil, "", fmt.Errorf("multiple jobs found: %v", jobIDs)
	}
	jobID := jobIDs[0]

	rawNeeds := yaml.Node{
		Kind:    yaml.SequenceNode,
		Content: make([]*yaml.Node, 0, len(task.Needs)),
	}
	for id, need := range task.Needs {
		workflow.Jobs[id] = &model.Job{
			Outputs: need.Outputs,
			Result:  strings.ToLower(strings.TrimPrefix(need.Result.String(), "RESULT_")),
		}
		rawNeeds.Content = append(rawNeeds.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: id,
		})
	}
	slices.SortFunc(rawNeeds.Content, func(left, right *yaml.Node) int { return strings.Compare(left.Value, right.Value) })

	workflow.Jobs[jobID].RawNeeds = rawNeeds

	return workflow, jobID, nil
}
