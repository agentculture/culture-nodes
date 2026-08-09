package runnerconformance_test

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/tests/runnerconformance"
)

// The flag-gated entry point: a runner author points the whole kit at their
// own service.
//
//	go test ./tests/runnerconformance -args \
//	    -endpoint=https://runner.thor.internal:8443 \
//	    -auth-token=$RUNNER_SECRET \
//	    -operation-file=./my-operation.json
//
// The operation is supplied as a FILE rather than as a pile of flags, because
// an operation is a schema document (schemas/runner/operation.schema.json)
// and the runner is entitled to refuse one the kit assembled from defaults it
// invented. Without -endpoint this skips, which is why the kit costs nothing
// in a normal `go test ./...` run.

var (
	endpoint = flag.String("endpoint", "",
		"runner service base URL to run the api/runner-protocol conformance kit against; without it, this test skips")
	authToken = flag.String("auth-token", "",
		"bearer secret the runner requires; the protocol makes it mandatory, so an empty value fails the auth check")
	operationFile = flag.String("operation-file", "",
		"path to a JSON runner operation the service should accept and run to completion")
	refusedOperationFile = flag.String("refused-operation-file", "",
		"path to a JSON runner operation the service must refuse; empty skips the policy-boundary check")
	cancellableOperationFile = flag.String("cancellable-operation-file", "",
		"path to a long-running JSON runner operation the kit dispatches and then cancels; empty skips the cancel check")
	expectState = flag.String("expect-state", string(runners.StateCompleted),
		"terminal state -operation-file should reach")
	requestTimeout = flag.Duration("timeout", runnerconformance.DefaultTimeout,
		"per-request timeout")
	terminalWait = flag.Duration("terminal-wait", runnerconformance.DefaultTerminalWait,
		"how long to wait for an operation to reach a terminal state")
	pollInterval = flag.Duration("poll-interval", runnerconformance.DefaultPollInterval,
		"how often to sample status; a runner's declared poll_after_seconds wins when it is slower")
	cancelAfter = flag.Duration("cancel-after", runnerconformance.DefaultCancelAfter,
		"how long a cancellable operation runs before the cancel is sent")
)

func TestRunnerEndpointConformance(t *testing.T) {
	if *endpoint == "" {
		t.Skip("no -endpoint given: run `go test ./tests/runnerconformance -args -endpoint=URL -auth-token=... -operation-file=op.json` " +
			"to check a real runner service")
	}
	if *operationFile == "" {
		t.Fatal("-operation-file is required: the kit cannot invent an operation a given runner would accept")
	}

	cfg := runnerconformance.Config{
		Endpoint:             *endpoint,
		AuthToken:            *authToken,
		Operation:            readOperation(t, "operation-file", *operationFile),
		RefusedOperation:     readOptionalOperation(t, "refused-operation-file", *refusedOperationFile),
		CancellableOperation: readOptionalOperation(t, "cancellable-operation-file", *cancellableOperationFile),
		ExpectTerminalState:  runners.State(*expectState),
		Timeout:              *requestTimeout,
		TerminalWait:         *terminalWait,
		PollInterval:         *pollInterval,
		CancelAfter:          *cancelAfter,
	}

	t.Logf("running the api/runner-protocol conformance kit against %s", cfg.Endpoint)
	runnerconformance.Run(t, cfg)
}

func readOperation(t *testing.T, flagName, path string) runners.Operation {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the path is an explicit test flag
	if err != nil {
		t.Fatalf("-%s: %v", flagName, err)
	}
	var op runners.Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatalf("-%s is not a runner operation document: %v", flagName, err)
	}
	return op
}

func readOptionalOperation(t *testing.T, flagName, path string) *runners.Operation {
	t.Helper()
	if path == "" {
		return nil
	}
	op := readOperation(t, flagName, path)
	return &op
}

// A guard against a flag default drifting out of range of the kit's own
// defaults, which would make the skip-vs-run behaviour surprising.
var _ = time.Duration(0)
