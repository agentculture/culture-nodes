package runners_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/schemas"
)

// The runner protocol's wire payloads are schemas/runner/{operation,result}
// .schema.json — not copies of them, not supersets of them. The envelopes
// declared in protocol.go exist only to say which operation a status refers
// to and whether it has finished; everything an operation or a result claims
// is claimed by the schema document itself. These tests hold that line.

// TestOperationStatusCarriesTheResultDocumentVerbatim is the contract's
// central claim as a test: what a runner puts in a terminal status is the
// result schema's document, byte for byte, not a re-serialisation of it into
// some status-specific shape.
func TestOperationStatusCarriesTheResultDocumentVerbatim(t *testing.T) {
	original := example(t, "runner-result.json")

	var result runners.Result
	if err := json.Unmarshal(original, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}

	status := runners.OperationStatus{
		OperationID: result.OperationID,
		State:       result.State,
		Result:      &result,
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}

	var envelope struct {
		OperationID string          `json:"operation_id"`
		State       string          `json:"state"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode status envelope: %v", err)
	}
	if got, want := canonical(t, envelope.Result), canonical(t, original); got != want {
		t.Errorf("the status envelope did not carry the result document verbatim:\n got: %s\nwant: %s", got, want)
	}
	if err := newValidator(t).ValidateJSON(contracts.SchemaRunnerResult, envelope.Result); err != nil {
		t.Errorf("the embedded result does not validate against the runner result schema: %v", err)
	}
	if envelope.State != string(result.State) {
		t.Errorf("envelope state %q disagrees with the result's own state %q", envelope.State, result.State)
	}

	var decoded runners.OperationStatus
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Errorf("a status built from the reference result does not validate: %v", err)
	}
	if !decoded.Terminal() {
		t.Error("a status carrying a completed result is terminal")
	}
}

// TestNonTerminalStatusCarriesNoResult is the honesty rule polling depends on:
// an operation that has not finished has no result, and a runner that ships
// one anyway is making a claim it cannot have measured yet.
func TestNonTerminalStatusCarriesNoResult(t *testing.T) {
	status := runners.OperationStatus{OperationID: "op_01JAV3QK2M0000000000000011", State: runners.StateRunning}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(encoded), "result") {
		t.Errorf("a running status must not carry a result key at all, got %s", encoded)
	}
	if status.Terminal() {
		t.Error("running is not a terminal state")
	}
	if err := status.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestOperationStatusValidate(t *testing.T) {
	const opID = "op_01JAV3QK2M0000000000000011"
	terminal := func(mutate func(*runners.OperationStatus)) runners.OperationStatus {
		result := minimalResult()
		status := runners.OperationStatus{OperationID: result.OperationID, State: result.State, Result: &result}
		if mutate != nil {
			mutate(&status)
		}
		return status
	}

	for _, tc := range []struct {
		name   string
		status runners.OperationStatus
		wantIn string
	}{
		{
			name:   "no operation id",
			status: runners.OperationStatus{State: runners.StateRunning},
			wantIn: "operation id",
		},
		{
			name:   "state the protocol does not define",
			status: runners.OperationStatus{OperationID: opID, State: "in_progress"},
			wantIn: "not a runner-protocol operation state",
		},
		{
			name:   "terminal without a result",
			status: terminal(func(s *runners.OperationStatus) { s.Result = nil }),
			wantIn: "carries no result document",
		},
		{
			name:   "non-terminal with a result",
			status: terminal(func(s *runners.OperationStatus) { s.State = runners.StateRunning }),
			wantIn: "not finished",
		},
		{
			name: "envelope state disagrees with the result",
			status: terminal(func(s *runners.OperationStatus) {
				s.State = runners.StateFailed // the result inside still says completed
			}),
			wantIn: "disagrees",
		},
		{
			name: "result is about a different operation",
			status: terminal(func(s *runners.OperationStatus) {
				s.OperationID = "op_01JAV3QK2M0000000000000099"
			}),
			wantIn: "different operation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.status.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not explain the problem (%q)", err, tc.wantIn)
			}

			// A malformed status is a contract failure, never a failed
			// result: the runtime learned nothing about the execution, so it
			// has nothing honest to record about it.
			var dispatchErr *runners.DispatchError
			if !errors.As(err, &dispatchErr) {
				t.Fatalf("error is not a *DispatchError: %v", err)
			}
			if dispatchErr.Kind != runners.ErrorContractFailure {
				t.Errorf("Kind = %q, want %q", dispatchErr.Kind, runners.ErrorContractFailure)
			}
			if dispatchErr.Retryable() {
				t.Error("a contract failure is not retryable")
			}
		})
	}
}

func TestAcceptanceValidate(t *testing.T) {
	const opID = "op_01JAV3QK2M0000000000000011"

	for _, tc := range []struct {
		name       string
		acceptance runners.Acceptance
		wantIn     string
	}{
		{
			name:       "no operation id",
			acceptance: runners.Acceptance{},
			wantIn:     "operation id",
		},
		{
			name:       "acknowledges a different operation",
			acceptance: runners.Acceptance{OperationID: "op_01JAV3QK2M0000000000000099"},
			wantIn:     "different operation",
		},
		{
			name:       "negative poll interval",
			acceptance: runners.Acceptance{OperationID: opID, PollAfterSeconds: -1},
			wantIn:     "poll_after_seconds",
		},
		{
			name:       "retention too short to be sampled",
			acceptance: runners.Acceptance{OperationID: opID, StatusRetentionSeconds: 30},
			wantIn:     "status_retention_seconds",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.acceptance.Validate(opID)
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.acceptance)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not explain the problem (%q)", err, tc.wantIn)
			}
			var dispatchErr *runners.DispatchError
			if !errors.As(err, &dispatchErr) || dispatchErr.Kind != runners.ErrorContractFailure {
				t.Errorf("want a contract-failure DispatchError, got %v", err)
			}
		})
	}

	accepted := runners.Acceptance{OperationID: opID}
	if err := accepted.Validate(opID); err != nil {
		t.Fatalf("a bare acceptance that echoes the operation id is valid: %v", err)
	}
	if got := accepted.PollInterval(); got != runners.DefaultPollInterval {
		t.Errorf("PollInterval() = %v, want the protocol default %v", got, runners.DefaultPollInterval)
	}
	if got := accepted.StatusRetention(); got != runners.MinStatusRetention {
		t.Errorf("StatusRetention() = %v, want the protocol minimum %v", got, runners.MinStatusRetention)
	}
}

// TestServiceIdentityBuildsTheProtocolURLs keeps the paths the document
// states and the paths the code builds as one fact.
func TestServiceIdentityBuildsTheProtocolURLs(t *testing.T) {
	for _, endpoint := range []string{
		"https://runner.thor.internal:8443",
		"https://runner.thor.internal:8443/",
	} {
		identity := runners.ServiceIdentity{Endpoint: endpoint, ImageDigest: testDigest, SecretRef: testSecret}
		if got, want := identity.ExecuteURL(), "https://runner.thor.internal:8443"+runners.OperationsPath; got != want {
			t.Errorf("ExecuteURL() = %q, want %q", got, want)
		}
		if got, want := identity.StatusURL("op_1"), "https://runner.thor.internal:8443"+runners.OperationsPath+"/op_1"; got != want {
			t.Errorf("StatusURL() = %q, want %q", got, want)
		}
		if got, want := identity.CancelURL("op_1"), "https://runner.thor.internal:8443"+runners.OperationsPath+"/op_1/cancel"; got != want {
			t.Errorf("CancelURL() = %q, want %q", got, want)
		}
	}

	// A base path is preserved: an operator who mounts the runner behind a
	// path prefix is not punished for it.
	behindPrefix := runners.ServiceIdentity{Endpoint: "https://gateway.example/runners/headspace", ImageDigest: testDigest, SecretRef: testSecret}
	if got, want := behindPrefix.ExecuteURL(), "https://gateway.example/runners/headspace"+runners.OperationsPath; got != want {
		t.Errorf("ExecuteURL() = %q, want %q", got, want)
	}
}

// TestProtocolDocumentMatchesTheCode is what keeps api/runner-protocol/README
// .md a contract rather than an essay: every path, header and version it
// quotes is the constant this package exports, and the schema files it calls
// the wire payloads are the ones actually embedded in the binary.
func TestProtocolDocumentMatchesTheCode(t *testing.T) {
	doc := readProtocolDoc(t)

	for _, quoted := range []string{
		runners.ProtocolVersion,
		runners.OperationsPath,
		runners.ProtocolVersionHeader,
		runners.IdempotencyKeyHeader,
		runners.CallbackURLHeader,
		runners.CallbackTokenHeader,
	} {
		if !strings.Contains(doc, quoted) {
			t.Errorf("the protocol document never mentions %q, which this package exports as part of the wire contract", quoted)
		}
	}

	// The protocol's payload constants and the validator's schema identifiers
	// are the same two files; stating them twice is only safe while they
	// cannot drift.
	if runners.OperationSchemaPath != contracts.SchemaRunnerOperation ||
		runners.ResultSchemaPath != contracts.SchemaRunnerResult {
		t.Errorf("the protocol's payload paths (%q, %q) have drifted from the validated schema identifiers (%q, %q)",
			runners.OperationSchemaPath, runners.ResultSchemaPath,
			contracts.SchemaRunnerOperation, contracts.SchemaRunnerResult)
	}

	for _, schemaPath := range []string{
		runners.OperationSchemaPath,
		runners.ResultSchemaPath,
	} {
		if !strings.Contains(doc, schemaPath) {
			t.Errorf("the protocol document does not name %s as a wire payload", schemaPath)
		}
		if _, err := schemas.FS.ReadFile(schemaPath); err != nil {
			t.Errorf("the document names %s as the wire payload but it is not an embedded schema: %v", schemaPath, err)
		}
	}

	// The three statements acceptance criteria 1 and 3 require the document
	// to make. They are checked as text because the document is where an
	// implementer reads them.
	for _, phrase := range []string{
		"202",                    // dispatch is an acceptance, not a result
		"strictly optional",      // the completion callback
		"mandatory",              // caller authentication
		"401",                    // an unauthenticated request is refused
		"403",                    // an unauthorised one too
		"unrestricted shell",     // the operation schema's policy-boundary language
		"never a fabricated",     // the DispatchError contract over HTTP
		"secret_ref",             // the registry identity's credential reference
		"AllowInsecureTransport", // the plaintext opt-in the registry demands
		"status sampling",        // completion is learned by polling
	} {
		if !strings.Contains(doc, phrase) {
			t.Errorf("the protocol document does not state %q", phrase)
		}
	}
}

// TestProtocolDocumentErrorTableMatchesRetryability reads the document's
// HTTP-to-error-kind table and holds every "Retryable" cell to what
// ErrorKind.Retryable actually returns. The table is the thing an
// implementer and a reviewer read; a row that says "no" where the runtime
// retries is a lie about the runtime's behaviour, and it is the kind of lie
// that is only ever caught in production.
func TestProtocolDocumentErrorTableMatchesRetryability(t *testing.T) {
	kinds := map[string]runners.ErrorKind{}
	for _, kind := range []runners.ErrorKind{
		runners.ErrorRetryableTransport, runners.ErrorRateLimited, runners.ErrorRunnerUnavailable,
		runners.ErrorRejectedInput, runners.ErrorAuthOrPolicy, runners.ErrorContractFailure,
		runners.ErrorExecutionFailure, runners.ErrorTimeout, runners.ErrorCancellation,
	} {
		kinds[string(kind)] = kind
	}

	rows := 0
	for _, line := range strings.Split(readProtocolDoc(t), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 5 {
			continue
		}
		kind, ok := kinds[strings.Trim(strings.TrimSpace(cells[2]), "`")]
		if !ok {
			continue
		}
		rows++
		declared := strings.TrimSpace(cells[3])
		if declared != "yes" && declared != "no" {
			t.Errorf("row for %q declares retryability as %q; want yes or no", kind, declared)
			continue
		}
		if want := map[bool]string{true: "yes", false: "no"}[kind.Retryable()]; declared != want {
			t.Errorf("the document says %q is retryable=%s; ErrorKind.Retryable says %s", kind, declared, want)
		}
	}
	if rows < 6 {
		t.Errorf("found only %d classified error rows in the document; the error table is missing or has changed shape", rows)
	}
}

// readProtocolDoc loads api/runner-protocol/README.md relative to this test's
// own file, so the test does not depend on the working directory.
func readProtocolDoc(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repository root")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	path := filepath.Join(root, "api", "runner-protocol", "README.md")
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from this test's own location
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
