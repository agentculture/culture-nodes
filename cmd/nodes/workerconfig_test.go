package main

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestRemintProducerActorIDFromEnvDefaultsAndOverrides(t *testing.T) {
	t.Setenv(envRemintProducerActorID, "")
	if got := remintProducerActorIDFromEnv(); got != postgres.RemintSchedulerActorID {
		t.Fatalf("unset producer id = %q, want default %q", got, postgres.RemintSchedulerActorID)
	}
	t.Setenv(envRemintProducerActorID, "engine_remint_custom")
	if got := remintProducerActorIDFromEnv(); got != "engine_remint_custom" {
		t.Fatalf("configured producer id = %q", got)
	}
}

// A code-runner tuple that is absent ENTIRELY says this deployment runs no
// code nodes — the Helm chart's configuration, and every deployment that only
// dispatches agents. Refusing it broke kind-smoke: both worker pods
// CrashLoopBackOff'd on a check meant to catch misattribution, because the
// chart sets none of the three.
func TestWorkerConfigPreflightAcceptsAnEntirelyAbsentCodeRunner(t *testing.T) {
	for _, name := range []string{envCodeRunnerName, envCodeRunnerRevision, envCodeRunnerActorID, envCallbackBaseURL, envCallbackSecret} {
		t.Setenv(name, "")
	}
	if err := workerConfigPreflight(); err != nil {
		t.Fatalf("preflight refused a deployment that declares no code runner at all: %s", err.Message)
	}
}

// The dangerous state is PARTIAL: the worker starts and its first code
// dispatch produces evidence attributed to an identity nobody fully declared.
// Every missing field is named, not just the first, so one restart fixes the
// configuration instead of three.
func TestWorkerConfigPreflightReportsEveryMissingRunnerField(t *testing.T) {
	t.Setenv(envCodeRunnerName, "runner")
	for _, name := range []string{envCodeRunnerRevision, envCodeRunnerActorID, envCallbackBaseURL, envCallbackSecret} {
		t.Setenv(name, "")
	}
	err := workerConfigPreflight()
	if err == nil {
		t.Fatal("preflight accepted a partial code-runner identity")
	}
	for _, name := range []string{envCodeRunnerRevision, envCodeRunnerActorID} {
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
