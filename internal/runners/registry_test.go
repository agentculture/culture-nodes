package runners_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

const (
	testARN    = "arn:aws:lambda:us-east-1:123456789012:function:nodes-run-tests"
	testARNTwo = "arn:aws:lambda:us-east-1:123456789012:function:nodes-build"
	testDigest = "sha256:0604fdb7edd7a8eacbcbdebdf7aad00db03650efd7f927f4b16f6cd0e0c3747e"
	otherDiges = "sha256:104c92af170b72888c5be5b64aa08aff686010ae232cd101b8c1c61f7eff0636"
)

func testRegistry(t *testing.T) *runners.FunctionRegistry {
	t.Helper()
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterFunction("deliver-change/run-tests", runners.FunctionIdentity{
		ARN:         testARN,
		ImageDigest: testDigest,
	}); err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}
	return registry
}

// TestResolveRefusesUnregisteredIdentity is spec claim c41's core property at
// the registry level: an identity nobody registered has no answer, and the
// refusal is typed so a caller cannot mistake it for a transient failure.
func TestResolveRefusesUnregisteredIdentity(t *testing.T) {
	registry := testRegistry(t)

	_, err := registry.Resolve("deliver-change/exfiltrate")
	if err == nil {
		t.Fatal("Resolve of an unregistered identity returned no error")
	}
	if !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Errorf("error does not match ErrUnregisteredFunction: %v", err)
	}

	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatchErr.Kind != runners.ErrorAuthOrPolicy {
		t.Errorf("Kind = %q, want %q", dispatchErr.Kind, runners.ErrorAuthOrPolicy)
	}
	if dispatchErr.Retryable() {
		t.Error("a refused identity is not retryable: asking the same forbidden question again cannot succeed")
	}
	if !strings.Contains(dispatchErr.Error(), "deliver-change/exfiltrate") {
		t.Errorf("refusal does not name the requested identity: %v", dispatchErr)
	}
}

// TestEmptyRegistryRefusesEverything states the safe default out loud: a
// worker that has not been told what it may invoke may invoke nothing.
func TestEmptyRegistryRefusesEverything(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	if _, err := registry.Resolve("anything"); !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Errorf("an empty registry must refuse every name, got %v", err)
	}
}

func TestRegisterFunctionValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		identity runners.FunctionIdentity
		wantIn   string
	}{
		{
			name:     "wildcard ARN",
			key:      "wild",
			identity: runners.FunctionIdentity{ARN: "arn:aws:lambda:us-east-1:123456789012:function:*", ImageDigest: testDigest},
			wantIn:   "wildcard",
		},
		{
			name:     "bare function name",
			key:      "bare",
			identity: runners.FunctionIdentity{ARN: "nodes-run-tests", ImageDigest: testDigest},
			wantIn:   "fully-qualified",
		},
		{
			name:     "unpinned image",
			key:      "unpinned",
			identity: runners.FunctionIdentity{ARN: testARN},
			wantIn:   "pinned image digest",
		},
		{
			name:     "digest that is not a digest",
			key:      "bad-digest",
			identity: runners.FunctionIdentity{ARN: testARN, ImageDigest: "latest"},
			wantIn:   "sha256",
		},
		{
			name:     "empty name",
			key:      "",
			identity: runners.FunctionIdentity{ARN: testARN, ImageDigest: testDigest},
			wantIn:   "not a valid identity name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := runners.NewFunctionRegistry()
			err := registry.RegisterFunction(tc.key, tc.identity)
			if err == nil {
				t.Fatalf("RegisterFunction accepted %+v", tc.identity)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not explain the problem (%q)", err, tc.wantIn)
			}
			if registry.Len() != 0 {
				t.Error("a refused registration must not land in the registry")
			}
		})
	}
}

// TestRepointingANameIsRefused keeps the allowlist from widening in place.
// Re-registering the identical identity stays a no-op so idempotent startup
// wiring is not punished.
func TestRepointingANameIsRefused(t *testing.T) {
	registry := testRegistry(t)

	if err := registry.RegisterFunction("deliver-change/run-tests", runners.FunctionIdentity{
		ARN: testARN, ImageDigest: testDigest,
	}); err != nil {
		t.Errorf("re-registering an identical identity should be a no-op, got %v", err)
	}

	err := registry.RegisterFunction("deliver-change/run-tests", runners.FunctionIdentity{
		ARN: testARNTwo, ImageDigest: otherDiges,
	})
	if err == nil {
		t.Fatal("repointing a registered name was accepted")
	}
	identity, resolveErr := registry.Resolve("deliver-change/run-tests")
	if resolveErr != nil {
		t.Fatalf("Resolve: %v", resolveErr)
	}
	if identity.ARN != testARN {
		t.Errorf("the original pin was overwritten: %s", identity.ARN)
	}
}

func TestNodeKeyNamespacesByWorkflow(t *testing.T) {
	if got := runners.NodeKey("deliver-change", "run-tests"); got != "deliver-change/run-tests" {
		t.Errorf("NodeKey = %q", got)
	}
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterFunction(runners.NodeKey("deliver-change", "run-tests"), runners.FunctionIdentity{
		ARN: testARN, ImageDigest: testDigest,
	}); err != nil {
		t.Fatalf("a NodeKey must be a registrable name: %v", err)
	}
}

func TestARNsAreDistinctAndSorted(t *testing.T) {
	registry := testRegistry(t)
	for name, arn := range map[string]string{
		"deliver-change/build": testARNTwo,
		"shared/run-tests":     testARN, // same ARN under a second logical name
	} {
		if err := registry.RegisterFunction(name, runners.FunctionIdentity{ARN: arn, ImageDigest: testDigest}); err != nil {
			t.Fatalf("RegisterFunction(%s): %v", name, err)
		}
	}

	arns := registry.ARNs()
	if len(arns) != 2 {
		t.Fatalf("ARNs() = %v, want the two distinct ARNs", arns)
	}
	if arns[0] > arns[1] {
		t.Errorf("ARNs() is not sorted: %v", arns)
	}
}
