package main

import (
	"strings"
	"testing"
)

func TestWorkerConfigPreflightReportsEveryMissingRunnerField(t *testing.T) {
	for _, name := range []string{envCodeRunnerName, envCodeRunnerRevision, envCodeRunnerActorID, envCallbackBaseURL, envCallbackSecret} {
		t.Setenv(name, "")
	}
	err := workerConfigPreflight()
	if err == nil {
		t.Fatal("preflight accepted an absent code-runner identity")
	}
	for _, name := range []string{envCodeRunnerName, envCodeRunnerRevision, envCodeRunnerActorID} {
		if !strings.Contains(err.Message, name+" is missing") {
			t.Errorf("message %q does not report %s", err.Message, name)
		}
	}
}

func TestWorkerConfigPreflightReportsPartialRunnerAndCallbackTogether(t *testing.T) {
	t.Setenv(envCodeRunnerName, "runner")
	t.Setenv(envCodeRunnerRevision, "")
	t.Setenv(envCodeRunnerActorID, "")
	t.Setenv(envCallbackBaseURL, "https://nodes.example")
	t.Setenv(envCallbackSecret, "")
	err := workerConfigPreflight()
	if err == nil {
		t.Fatal("preflight accepted partial deployment configuration")
	}
	for _, want := range []string{envCodeRunnerRevision, envCodeRunnerActorID, envCallbackSecret} {
		if !strings.Contains(err.Message, want) {
			t.Errorf("message %q does not include %s", err.Message, want)
		}
	}
}

func TestWorkerConfigPreflightAcceptsCompleteConfiguration(t *testing.T) {
	t.Setenv(envCodeRunnerName, "runner")
	t.Setenv(envCodeRunnerRevision, "rev")
	t.Setenv(envCodeRunnerActorID, "actor_register_01")
	t.Setenv(envCallbackBaseURL, "")
	t.Setenv(envCallbackSecret, "")
	if err := workerConfigPreflight(); err != nil {
		t.Fatalf("preflight refused complete configuration: %v", err)
	}
}
