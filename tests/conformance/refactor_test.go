package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/tests/conformance"
)

// The kit, run against the in-repo reference actor.
//
// This file is the answer to "how do I know the conformance kit is not
// asserting something impossible?" — it stands up a correct PRD §13
// implementation in-process and requires the whole suite to pass against it,
// on every `go test ./...`, with no external service and no flags.
//
// It is also the worked example. An adapter author whose endpoint fails a
// check can read tests/conformance/reference.go and see the seventy lines
// that make that check pass.
//
// (The file name is the one this task's brief specified. What it contains is
// the reference-actor run of the kit.)

func TestReferenceActorPassesTheConformanceKit(t *testing.T) {
	actor := conformance.NewReferenceActor("reference-workload-token")
	defer actor.Close()

	conformance.Run(t, conformance.Config{
		Endpoint:  actor.URL(),
		AuthToken: "reference-workload-token",

		WorkflowName:   "conformance-reference",
		WorkflowDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		NodeID:         "reference",
		ContractDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",

		Input:      json.RawMessage(`{"subject":"synchronous"}`),
		AsyncInput: json.RawMessage(`{"async":true,"delay_ms":50}`),
		BadInput:   json.RawMessage(`{"reject":true}`),

		// The reference actor redelivers a refused terminal callback with the
		// same event id, so the strict form of §13.4's idempotency
		// requirement is exercised rather than merely assumed.
		ExpectCallbackRetry: true,
		// It also declares supports_cancellation, so the cancellation check
		// must actually run rather than skip.
		RequireCancellation: true,

		Timeout:      10 * time.Second,
		CallbackWait: 15 * time.Second,
	})
}

// negativeEnv makes the inner half of the negative self-check runnable.
const negativeEnv = "CULTURE_NODES_CONFORMANCE_NEGATIVE"

// TestKitDetectsANonConformingActor proves the kit is not vacuous: an actor
// that refuses every invocation must FAIL it.
//
// The check runs in a subprocess because a failing subtest marks its parent
// failed, and there is no supported way to observe a *testing.T failure
// without propagating it. Re-execing `go test -run` for the inner half is the
// standard answer, and it has a real bonus: what is asserted is the actual
// process exit status an adapter author would see.
func TestKitDetectsANonConformingActor(t *testing.T) {
	if os.Getenv(negativeEnv) != "" {
		t.Skip("this is the outer half of the negative self-check; the inner half is TestKitNegativeInner")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH, so the negative self-check cannot re-exec the test binary")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-run", "TestKitNegativeInner", ".")
	cmd.Env = append(os.Environ(), negativeEnv+"=1")
	output, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("the kit PASSED an actor whose invocations are all refused; it is not asserting anything\n%s", output)
	}
	// The failure must be the kit's checks failing, not the package failing to
	// build or the subprocess dying for some unrelated reason.
	if !bytes.Contains(output, []byte("authentication-is-required")) &&
		!bytes.Contains(output, []byte("synchronous-result-shape")) {
		t.Fatalf("the subprocess failed for a reason other than a kit check:\n%s", output)
	}
	t.Logf("the kit correctly failed a non-conforming actor (exit: %v)", err)
}

// TestKitNegativeInner is the inner half: the kit driven against an actor
// that requires a credential the config does not supply, so every invocation
// is refused and the checks that need a working invocation fail. It is
// EXPECTED TO FAIL, and only runs when its parent asks for it.
func TestKitNegativeInner(t *testing.T) {
	if os.Getenv(negativeEnv) == "" {
		t.Skip("inner half of the negative self-check; run by TestKitDetectsANonConformingActor")
	}

	actor := conformance.NewReferenceActor("reference-workload-token")
	defer actor.Close()

	conformance.Run(t, conformance.Config{
		Endpoint: actor.URL(),
		// AuthToken deliberately omitted: the actor requires one.
		Input:   json.RawMessage(`{"subject":"synchronous"}`),
		Timeout: 5 * time.Second,
	})
}
