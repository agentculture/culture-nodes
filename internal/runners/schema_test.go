package runners_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/schemas"
)

// The Go structs in this package are a *mirror* of schemas/runner/*.json, not
// a second definition of the contract. These tests are what makes "mirror"
// checkable: a struct that drops a field, renames a tag, or emits a shape the
// schema rejects fails here rather than at the first real dispatch.

func newValidator(t *testing.T) *contracts.Validator {
	t.Helper()
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("contracts.NewValidator: %v", err)
	}
	return v
}

func example(t *testing.T, name string) []byte {
	t.Helper()
	data, err := schemas.ExamplesFS.ReadFile("examples/" + name)
	if err != nil {
		t.Fatalf("read example %s: %v", name, err)
	}
	return data
}

// canonical normalises JSON so a comparison is about content, not key order
// or whitespace.
func canonical(t *testing.T, data []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode json: %v\n%s", err, data)
	}
	out, err := contracts.CanonicalJSON(value)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return string(out)
}

// TestOperationRoundTripsTheReferenceExample decodes the checked-in example
// operation into the Go mirror and re-encodes it. Byte-equal canonical JSON
// proves the mirror is lossless: no field of the schema's example is dropped
// on the way in or invented on the way out.
func TestOperationRoundTripsTheReferenceExample(t *testing.T) {
	original := example(t, "runner-operation.json")

	var op runners.Operation
	if err := json.Unmarshal(original, &op); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("encode operation: %v", err)
	}

	if got, want := canonical(t, encoded), canonical(t, original); got != want {
		t.Errorf("operation round trip lost or invented fields:\n got: %s\nwant: %s", got, want)
	}
	if err := newValidator(t).ValidateJSON(contracts.SchemaRunnerOperation, encoded); err != nil {
		t.Errorf("re-encoded operation does not validate: %v", err)
	}
}

// TestResultRoundTripsTheReferenceExample is the same proof for the result
// document, which is the harder one: it carries the observations block, the
// artifact map with additionalProperties, and a nullable exit code.
func TestResultRoundTripsTheReferenceExample(t *testing.T) {
	original := example(t, "runner-result.json")

	var res runners.Result
	if err := json.Unmarshal(original, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}

	if got, want := canonical(t, encoded), canonical(t, original); got != want {
		t.Errorf("result round trip lost or invented fields:\n got: %s\nwant: %s", got, want)
	}
	if err := newValidator(t).ValidateJSON(contracts.SchemaRunnerResult, encoded); err != nil {
		t.Errorf("re-encoded result does not validate: %v", err)
	}
}

// TestGoBuiltOperationValidates builds an operation in Go rather than
// decoding one, because that is what the compiler and worker will do.
func TestGoBuiltOperationValidates(t *testing.T) {
	op := runners.Operation{
		OperationID:    "op_01JAV3QK2M0000000000000011",
		Runner:         "lambda",
		RunnerRevision: contracts.Digest([]byte("revision")),
		Execution: runners.Execution{
			Kind:        runners.ExecutionFunction,
			ImageRef:    "deliver-change/run-tests",
			ImageDigest: contracts.Digest([]byte("image")),
		},
		Command: runners.Command{Argv: []string{"python", "-m", "pytest", "-q"}},
		Policy: runners.Policy{
			TimeoutSeconds: 600,
			Network:        runners.NetworkNone,
			// Left nil deliberately: MarshalJSON must still emit [].
			AllowedOutputPaths: nil,
		},
		Evidence: runners.EvidenceRequest{CaptureExit: true, CaptureLogs: true},
	}

	encoded, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"allowed_output_paths":[]`)) {
		t.Errorf("an unset output-path policy must serialise as an explicit empty array, got: %s", encoded)
	}
	if err := newValidator(t).ValidateJSON(contracts.SchemaRunnerOperation, encoded); err != nil {
		t.Fatalf("Go-built operation does not validate: %v", err)
	}
}

// TestResultKeepsNullExitCodeDistinctFromZero guards the single most
// dangerous encoding shortcut available here: rendering "the process did not
// exit normally" as exit code 0.
func TestResultKeepsNullExitCodeDistinctFromZero(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		name string
		exit *runners.Exit
		want string
	}{
		{"explicit zero", &runners.Exit{Code: &zero}, `"code":0`},
		{"no honest exit", &runners.Exit{Code: nil}, `"code":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.exit)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.Contains(encoded, []byte(tc.want)) {
				t.Errorf("got %s, want it to contain %s", encoded, tc.want)
			}
		})
	}
}

// TestObservationsKeepAdditionalKeys proves the observations block does not
// quietly drop runner-specific observations on the way through the mirror —
// which is exactly where the Lambda adapter's honesty lives.
func TestObservationsKeepAdditionalKeys(t *testing.T) {
	res := minimalResult()
	res.Observations.Additional = map[string]runners.Observation{
		"handler_completion": {Measured: true, Complete: true, Method: "lambda_invoke_function_error"},
		"workspace_snapshot": {Measured: false, Complete: false, Note: "Lambda cannot observe a workspace."},
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := newValidator(t).ValidateJSON(contracts.SchemaRunnerResult, encoded); err != nil {
		t.Fatalf("result with additional observations does not validate: %v", err)
	}

	var decoded runners.Result
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, name := range []string{"handler_completion", "workspace_snapshot"} {
		if _, ok := decoded.Observations.Get(name); !ok {
			t.Errorf("observation %q was dropped by the round trip", name)
		}
	}
	if got := decoded.Observations.Names(); len(got) != 6 {
		t.Errorf("Names() = %v, want the four required plus two additional", got)
	}
}

// TestRequiredObservationsAreAlwaysEmitted proves the zero value of the
// honesty block is still a complete, valid honesty block: four observations,
// all saying nothing was measured. Silence is not an option the encoding
// offers.
func TestRequiredObservationsAreAlwaysEmitted(t *testing.T) {
	encoded, err := json.Marshal(runners.Observations{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, name := range []string{"exit_status", "changed_paths", "logs", "resource_usage"} {
		if _, ok := decoded[name]; !ok {
			t.Errorf("required observation %q missing from an empty Observations", name)
		}
	}
}

// minimalResult is the smallest schema-valid completed result, used by tests
// that only care about one part of the document.
func minimalResult() runners.Result {
	zero := 0
	started := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	return runners.Result{
		OperationID: "op_01JAV3QK2M0000000000000011",
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &zero},
		Timing: runners.Timing{
			StartedAt:  started,
			FinishedAt: started.Add(time.Second),
			DurationMs: 1000,
		},
		Environment: runners.Environment{
			RunnerRevision: contracts.Digest([]byte("revision")),
			ImageDigest:    contracts.Digest([]byte("image")),
			PolicyDigest:   contracts.Digest([]byte("policy")),
		},
		Changes: runners.Changes{Complete: false},
	}
}
