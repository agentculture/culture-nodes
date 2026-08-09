package runners_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// TestDispatchErrorTechStatus checks the one mapping the worker will lean on:
// a dispatch that never happened produced no domain answer, so every kind
// lands on a technical status and none on an outcome.
func TestDispatchErrorTechStatus(t *testing.T) {
	for _, tc := range []struct {
		kind runners.ErrorKind
		want engine.TechStatus
	}{
		{runners.ErrorAuthOrPolicy, engine.StatusPolicyDenied},
		{runners.ErrorRejectedInput, engine.StatusContractRejected},
		{runners.ErrorContractFailure, engine.StatusContractRejected},
		{runners.ErrorTimeout, engine.StatusTimedOut},
		{runners.ErrorCancellation, engine.StatusCancelled},
		{runners.ErrorRateLimited, engine.StatusFailed},
		{runners.ErrorRetryableTransport, engine.StatusFailed},
		{runners.ErrorRunnerUnavailable, engine.StatusFailed},
		{runners.ErrorExecutionFailure, engine.StatusFailed},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			err := &runners.DispatchError{Kind: tc.kind}
			if got := err.TechStatus(); got != tc.want {
				t.Errorf("TechStatus() = %q, want %q", got, tc.want)
			}
			if !err.TechStatus().Valid() {
				t.Errorf("TechStatus() = %q, which is not one of the statuses PRD §3.4 lists", err.TechStatus())
			}
		})
	}
}

// TestOnlyExplicitlyRetryableKindsRetry is PRD §13.5: retries happen for
// declared-retryable categories, and for nothing else. An unknown kind is not
// retryable, because "we do not know what went wrong" is not a reason to do
// it again.
func TestOnlyExplicitlyRetryableKindsRetry(t *testing.T) {
	retryable := map[runners.ErrorKind]bool{
		runners.ErrorRetryableTransport: true,
		runners.ErrorRateLimited:        true,
		runners.ErrorRunnerUnavailable:  true,
	}
	for _, kind := range []runners.ErrorKind{
		runners.ErrorRetryableTransport, runners.ErrorRateLimited, runners.ErrorRunnerUnavailable,
		runners.ErrorRejectedInput, runners.ErrorAuthOrPolicy, runners.ErrorContractFailure,
		runners.ErrorExecutionFailure, runners.ErrorTimeout, runners.ErrorCancellation,
		runners.ErrorKind("something_new"),
	} {
		if got := kind.Retryable(); got != retryable[kind] {
			t.Errorf("%q.Retryable() = %t, want %t", kind, got, retryable[kind])
		}
	}
}

// TestSentinelForNeverLies guards the mistake this mapping is here to
// prevent: a platform denial matching the sentinel that means "this process
// refused", which would make the two boundaries indistinguishable.
func TestSentinelForNeverLies(t *testing.T) {
	if got := runners.SentinelFor(runners.ErrorAuthOrPolicy); got != runners.ErrAccessDenied {
		t.Errorf("auth_or_policy maps to %v, want ErrAccessDenied", got)
	}
	if runners.SentinelFor(runners.ErrorAuthOrPolicy) == runners.ErrUnregisteredFunction {
		t.Error("a platform denial must not be indistinguishable from a local registry refusal")
	}
	if got := runners.SentinelFor(runners.ErrorExecutionFailure); got != nil {
		t.Errorf("a kind with no matching sentinel maps to %v, want nil", got)
	}
}

// TestDispatchErrorMessageNamesWhatItRefused: a refusal that does not say
// which operation and which identity is a refusal an operator cannot act on.
func TestDispatchErrorMessageNamesWhatItRefused(t *testing.T) {
	err := &runners.DispatchError{
		Kind:        runners.ErrorAuthOrPolicy,
		OperationID: "op_01JAV3QK2M0000000000000011",
		Identity:    "deliver-change/run-tests",
		Detail:      "no such identity in the runner registry",
		Err:         runners.ErrUnregisteredFunction,
	}
	message := err.Error()
	for _, want := range []string{"op_01JAV3QK2M0000000000000011", "deliver-change/run-tests", "no such identity"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q omits %q", message, want)
		}
	}
	if !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Error("Unwrap does not expose the sentinel")
	}
}
