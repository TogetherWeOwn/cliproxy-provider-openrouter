package main

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPRSizePullRequestTriggers(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/pr-size.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			PullRequest struct {
				Types []string `yaml:"types"`
			} `yaml:"pull_request"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	// A base edit changes the measured diff without changing the head SHA.
	for _, event := range []string{"opened", "synchronize", "reopened", "ready_for_review", "labeled", "unlabeled", "edited"} {
		t.Run(event, func(t *testing.T) {
			if !slices.Contains(workflow.On.PullRequest.Types, event) {
				t.Errorf("pr-size must remeasure on pull_request.%s", event)
			}
		})
	}
}
