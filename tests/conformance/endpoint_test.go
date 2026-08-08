package conformance_test

import (
	"encoding/json"
	"flag"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/tests/conformance"
)

// The flag-gated entry point: an adapter author points the whole kit at their
// own endpoint.
//
//	go test ./tests/conformance -args -endpoint=https://my-actor.example
//
// Everything else has a default, so the shortest useful command line is one
// flag. Without -endpoint this skips, which is why the kit costs nothing in a
// normal `go test ./...` run.

var (
	endpoint = flag.String("endpoint", "",
		"actor base URL to run the PRD §13 conformance kit against; without it, this test skips")
	authToken = flag.String("auth-token", "",
		"scoped workload token the actor requires; empty means the endpoint is unauthenticated")
	input = flag.String("input", `{}`,
		"JSON input the actor should accept and answer synchronously")
	asyncInput = flag.String("async-input", "",
		"JSON input the actor should answer with a §13.3 acceptance; empty skips the asynchronous checks")
	badInput = flag.String("bad-input", "",
		"JSON input the actor must reject; empty skips the contract-failure check")
	nodeID = flag.String("node-id", "conformance",
		"node id to send in §13.1's node block")
	contractDigest = flag.String("contract-digest", "",
		"contract digest to send in §13.1's node block")
	workflowName = flag.String("workflow-name", "conformance-kit",
		"workflow name to send in §13.1's workflow block")
	workflowDigest = flag.String("workflow-digest", "",
		"workflow version digest to send in §13.1's workflow block")
	callbackBase = flag.String("callback-base-url", "",
		"externally reachable base URL for the kit's callback receiver; set this when the actor cannot reach loopback")
	timeout = flag.Duration("timeout", conformance.DefaultTimeout,
		"per-invocation timeout")
	callbackWait = flag.Duration("callback-wait", conformance.DefaultCallbackWait,
		"how long to wait for an asynchronous actor's terminal callback")
	expectCallbackRetry = flag.Bool("expect-callback-retry", false,
		"require the actor to redeliver a refused terminal callback with the same event id")
	requireCancellation = flag.Bool("require-cancellation", false,
		"fail rather than skip when the actor does not declare supports_cancellation")
)

func TestActorEndpointConformance(t *testing.T) {
	if *endpoint == "" {
		t.Skip("no -endpoint given: run `go test ./tests/conformance -args -endpoint=URL` to check a real actor")
	}

	cfg := conformance.Config{
		Endpoint:            *endpoint,
		AuthToken:           *authToken,
		WorkflowName:        *workflowName,
		WorkflowDigest:      *workflowDigest,
		NodeID:              *nodeID,
		ContractDigest:      *contractDigest,
		Input:               jsonFlag(t, "input", *input),
		AsyncInput:          jsonFlag(t, "async-input", *asyncInput),
		BadInput:            jsonFlag(t, "bad-input", *badInput),
		CallbackBaseURL:     *callbackBase,
		Timeout:             *timeout,
		CallbackWait:        *callbackWait,
		ExpectCallbackRetry: *expectCallbackRetry,
		RequireCancellation: *requireCancellation,
	}

	t.Logf("running the PRD §13 conformance kit against %s", cfg.Endpoint)
	if cfg.CallbackBaseURL == "" && len(cfg.AsyncInput) > 0 {
		t.Log("note: the callback receiver is advertised on a loopback address; " +
			"pass -callback-base-url if the actor cannot reach it")
	}
	conformance.Run(t, cfg)
}

// jsonFlag validates a JSON-valued flag up front, so a typo in a shell
// argument is reported as a bad flag rather than as an actor that rejected an
// input it never really saw.
func jsonFlag(t *testing.T, name, value string) json.RawMessage {
	t.Helper()
	if value == "" {
		return nil
	}
	if !json.Valid([]byte(value)) {
		t.Fatalf("-%s is not valid JSON: %s", name, value)
	}
	return json.RawMessage(value)
}

// A guard against a flag default drifting out of range of the kit's own
// defaults, which would make the skip-vs-run behaviour surprising.
var _ = time.Duration(0)
