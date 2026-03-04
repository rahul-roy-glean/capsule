package main

import (
	"context"

	cigithub "github.com/rahul-roy-glean/bazel-firecracker/pkg/ci/github"
)

// labelsToMap converts a slice of labels to a map.
func labelsToMap(labels []string) map[string]string {
	return cigithub.LabelsToMap(labels)
}

// jobQueueRunnerLookup adapts HostRegistry to cigithub.RunnerLookup.
type jobQueueRunnerLookup struct {
	hr *HostRegistry
}

func (j *jobQueueRunnerLookup) GetRunnerByName(ctx context.Context, name string) (string, error) {
	runner, err := j.hr.GetRunner(name)
	if err != nil {
		return "", err
	}
	return runner.ID, nil
}
