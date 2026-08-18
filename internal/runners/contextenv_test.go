package runners_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// TestContextEnvironmentCarriesTheRunIdentity: a code node that must write
// derived records about its own run has to be able to name it.
func TestContextEnvironmentCarriesTheRunIdentity(t *testing.T) {
	env := runners.ContextEnvironment(runners.Operation{
		OperationID: "att_01JAV3QK2M0000000000000010",
		Context: &runners.Context{
			RunID:     "run_01JAV3QK2M0000000000000001",
			NodeRunID: "nr_01JAV3QK2M0000000000000008",
			AttemptID: "att_01JAV3QK2M0000000000000010",
		},
	})

	for name, want := range map[string]string{
		runners.EnvRunID:       "run_01JAV3QK2M0000000000000001",
		runners.EnvNodeRunID:   "nr_01JAV3QK2M0000000000000008",
		runners.EnvAttemptID:   "att_01JAV3QK2M0000000000000010",
		runners.EnvOperationID: "att_01JAV3QK2M0000000000000010",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
}

// TestContextEnvironmentOmitsWhatTheControlPlaneDidNotSet: an absent run id is
// a refusal a gate program can act on; an empty one is a value it might use.
func TestContextEnvironmentOmitsWhatTheControlPlaneDidNotSet(t *testing.T) {
	env := runners.ContextEnvironment(runners.Operation{OperationID: "op-1"})
	if _, ok := env[runners.EnvRunID]; ok {
		t.Errorf("%s is present for an operation with no context: %v", runners.EnvRunID, env)
	}
	if _, ok := env[runners.EnvInputJSON]; ok {
		t.Errorf("%s is present for an operation with no input: %v", runners.EnvInputJSON, env)
	}
	if env[runners.EnvOperationID] != "op-1" {
		t.Errorf("%s = %q, want op-1", runners.EnvOperationID, env[runners.EnvOperationID])
	}
}

// TestContextEnvironmentCarriesTheResolvedInput (issue #170): a code node's
// resolved §11.2 input document reaches the executed process the same way
// the run/node-run/attempt ids do — as an environment value, alongside them,
// never blanked to an empty string when Operation.Input is genuinely unset.
func TestContextEnvironmentCarriesTheResolvedInput(t *testing.T) {
	env := runners.ContextEnvironment(runners.Operation{
		OperationID: "op-1",
		Input:       []byte(`{"subject":"widget"}`),
	})
	if got, want := env[runners.EnvInputJSON], `{"subject":"widget"}`; got != want {
		t.Errorf("%s = %q, want %q", runners.EnvInputJSON, got, want)
	}
}
