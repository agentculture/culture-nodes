package contracts_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/schemas"
)

func newValidator(t *testing.T) *contracts.Validator {
	t.Helper()
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

// TestAllEmbeddedSchemasCompile fails loudly if any shipped schema is not a
// compilable Draft 2020-12 document, or if a $ref between them dangles.
func TestAllEmbeddedSchemasCompile(t *testing.T) {
	v := newValidator(t)
	names := v.SchemaNames()
	if len(names) == 0 {
		t.Fatal("no schemas were compiled")
	}
	for _, want := range []string{
		contracts.SchemaWorkflow,
		contracts.SchemaLedgerEnvelope,
		contracts.SchemaLedgerRecord,
		contracts.SchemaRunnerOperation,
		contracts.SchemaRunnerResult,
	} {
		if !contains(names, want) {
			t.Errorf("schema %q was not compiled; got %v", want, names)
		}
	}
	// Every MVP record type has its own schema (PRD §10.2).
	for _, recordType := range contracts.LedgerRecordTypes() {
		if name := contracts.LedgerRecordSchema(recordType); !contains(names, name) {
			t.Errorf("record type %q has no schema (%s)", recordType, name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestEmbeddedExamplesValidate keeps the shipped reference documents honest:
// the PRD §11.1 authoring example, and one document per contract family.
func TestEmbeddedExamplesValidate(t *testing.T) {
	v := newValidator(t)
	for _, tc := range []struct{ example, schema string }{
		{"examples/deliver-change.workflow.json", contracts.SchemaWorkflow},
		{"examples/ledger-claim.record.json", contracts.SchemaLedgerRecord},
		{"examples/ledger-evidence.record.json", contracts.SchemaLedgerRecord},
		{"examples/runner-operation.json", contracts.SchemaRunnerOperation},
		{"examples/runner-result.json", contracts.SchemaRunnerResult},
	} {
		t.Run(tc.example, func(t *testing.T) {
			data, err := schemas.ExamplesFS.ReadFile(tc.example)
			if err != nil {
				t.Fatalf("read example: %v", err)
			}
			if err := v.ValidateJSON(tc.schema, data); err != nil {
				t.Errorf("example is invalid under %s: %v", tc.schema, err)
			}
		})
	}
}

// TestValidWorkflowFixture covers the trimmed authoring shape kept next to the
// malformed fixtures it is derived from.
func TestValidWorkflowFixture(t *testing.T) {
	v := newValidator(t)
	if err := v.ValidateJSON(contracts.SchemaWorkflow, fixture(t, "valid", "workflow-minimal.json")); err != nil {
		t.Errorf("minimal workflow should be valid: %v", err)
	}
}

// TestValidLedgerRecordsCoverEveryType validates one minimal record per MVP
// record type against both the dispatching record schema and its own schema.
func TestValidLedgerRecordsCoverEveryType(t *testing.T) {
	v := newValidator(t)
	var records []json.RawMessage
	if err := json.Unmarshal(fixture(t, "valid", "ledger-records.json"), &records); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	seen := map[string]bool{}
	for i, raw := range records {
		var probe struct {
			RecordType string `json:"record_type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		seen[probe.RecordType] = true
		if err := v.ValidateJSON(contracts.SchemaLedgerRecord, raw); err != nil {
			t.Errorf("record %d (%s) failed the dispatching schema: %v", i, probe.RecordType, err)
		}
		if err := v.ValidateJSON(contracts.LedgerRecordSchema(probe.RecordType), raw); err != nil {
			t.Errorf("record %d (%s) failed its own schema: %v", i, probe.RecordType, err)
		}
	}
	for _, recordType := range contracts.LedgerRecordTypes() {
		if !seen[recordType] {
			t.Errorf("fixture has no record of type %q", recordType)
		}
	}
}

// TestMalformedFixturesReportPointerPaths is the diagnostic contract: a
// rejection names the offending field with a JSON Pointer, so an agent or a
// human is told where the document is wrong rather than merely that it is.
func TestMalformedFixturesReportPointerPaths(t *testing.T) {
	v := newValidator(t)
	for _, tc := range []struct {
		fixture     string
		schema      string
		wantPointer string
		wantIn      string
	}{
		{
			fixture:     "workflow-missing-ownerref.json",
			schema:      contracts.SchemaWorkflow,
			wantPointer: "/spec/nodes/build/ownerRef",
			wantIn:      "ownerRef",
		},
		{
			fixture:     "workflow-bad-node-kind.json",
			schema:      contracts.SchemaWorkflow,
			wantPointer: "/spec/nodes/build/kind",
		},
		{
			fixture:     "workflow-unknown-property.json",
			schema:      contracts.SchemaWorkflow,
			wantPointer: "/spec/nodes/build/owner_ref",
		},
		{
			fixture:     "ledger-bad-authority.json",
			schema:      contracts.SchemaLedgerClaim,
			wantPointer: "/authority",
		},
		{
			fixture:     "ledger-unknown-record-type.json",
			schema:      contracts.SchemaLedgerRecord,
			wantPointer: "/record_type",
		},
		{
			fixture:     "ledger-missing-supersedes.json",
			schema:      contracts.SchemaLedgerClaim,
			wantPointer: "/supersedes",
		},
		{
			// Also proves record.schema.json dispatches past the envelope
			// into the per-type schema: only task.schema.json knows what a
			// task status may be.
			fixture:     "ledger-task-bad-status.json",
			schema:      contracts.SchemaLedgerRecord,
			wantPointer: "/data/status",
		},
		{
			// Locks format assertion on: without it a date-time is just a
			// string and this fixture would validate.
			fixture:     "ledger-bad-timestamp.json",
			schema:      contracts.SchemaLedgerClaim,
			wantPointer: "/created_at",
		},
		{
			fixture:     "runner-result-missing-observation.json",
			schema:      contracts.SchemaRunnerResult,
			wantPointer: "/observations/changed_paths",
		},
		{
			fixture:     "runner-operation-unpinned-image.json",
			schema:      contracts.SchemaRunnerOperation,
			wantPointer: "/execution/image_digest",
		},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			err := v.ValidateJSON(tc.schema, fixture(t, "invalid", tc.fixture))
			if err == nil {
				t.Fatal("expected validation to fail, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantPointer) {
				t.Errorf("error does not name %s:\n%v", tc.wantPointer, err)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error does not mention %q:\n%v", tc.wantIn, err)
			}

			var ve *contracts.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error is not a *contracts.ValidationError: %T", err)
			}
			if ve.Schema != tc.schema {
				t.Errorf("violation reports schema %q, want %q", ve.Schema, tc.schema)
			}
			if len(ve.Violations) == 0 {
				t.Fatal("ValidationError carries no violations")
			}
			var pointers []string
			for _, violation := range ve.Violations {
				pointers = append(pointers, violation.Pointer)
			}
			if !contains(pointers, tc.wantPointer) {
				t.Errorf("no violation with pointer %s; got %v", tc.wantPointer, pointers)
			}
		})
	}
}

func TestValidateAcceptsGoValues(t *testing.T) {
	v := newValidator(t)
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, "valid", "workflow-minimal.json"), &doc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if err := v.Validate(contracts.SchemaWorkflow, doc); err != nil {
		t.Errorf("Validate on a decoded Go value: %v", err)
	}

	delete(doc, "kind")
	err := v.Validate(contracts.SchemaWorkflow, doc)
	if err == nil {
		t.Fatal("expected validation to fail without kind")
	}
	if !strings.Contains(err.Error(), "/kind") {
		t.Errorf("error does not name /kind:\n%v", err)
	}
}

// TestNodeHooksAreDeclaredButUnvalidated pins the current, deliberate state of
// pre_run/post_run: the keywords exist so authoring can begin, and they accept
// anything because their contract is not specified yet. When the hook contract
// lands this test should fail and be replaced — that failure is the signal, not
// a regression.
func TestNodeHooksAreDeclaredButUnvalidated(t *testing.T) {
	v := newValidator(t)
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, "valid", "workflow-minimal.json"), &doc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	spec := doc["spec"].(map[string]any)
	node := spec["nodes"].(map[string]any)["build"].(map[string]any)
	node["pre_run"] = map[string]any{"shape": []any{1, "not specified", nil}}
	node["post_run"] = "not even an object"

	if err := v.Validate(contracts.SchemaWorkflow, doc); err != nil {
		t.Errorf("pre_run/post_run are unvalidated stubs and should be accepted: %v", err)
	}
}

func TestValidateUnknownSchema(t *testing.T) {
	v := newValidator(t)
	err := v.ValidateJSON("ledger/nope.schema.json", []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error for an unknown schema name")
	}
	if !strings.Contains(err.Error(), "ledger/nope.schema.json") {
		t.Errorf("error does not name the missing schema:\n%v", err)
	}
}

func TestValidateJSONRejectsMalformedJSON(t *testing.T) {
	v := newValidator(t)
	if err := v.ValidateJSON(contracts.SchemaWorkflow, []byte(`{"apiVersion":`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestValidationErrorMessageShape(t *testing.T) {
	v := newValidator(t)
	err := v.ValidateJSON(contracts.SchemaWorkflow, fixture(t, "invalid", "workflow-missing-ownerref.json"))
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, contracts.SchemaWorkflow) {
		t.Errorf("message does not name the schema:\n%s", msg)
	}
	if !strings.HasPrefix(strings.TrimSpace(strings.Split(msg, "\n")[1]), "at /spec/nodes/build/ownerRef:") {
		t.Errorf("unexpected violation line shape:\n%s", msg)
	}
}

func fixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}
