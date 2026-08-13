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

// hasPrefix reports whether any string in haystack starts with prefix, so a
// test can assert "a violation was reported somewhere under this node's
// keyword" without pinning the exact sub-pointer a schema library chooses.
func hasPrefix(haystack []string, prefix string) bool {
	for _, s := range haystack {
		if strings.HasPrefix(s, prefix) {
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
			// Also proves record.schema.json dispatches into the
			// additively-registered grade.schema.json the same way it
			// dispatches into the PRD §10.2 types.
			fixture:     "ledger-grade-rating-out-of-range.json",
			schema:      contracts.SchemaLedgerRecord,
			wantPointer: "/data/rating",
		},
		{
			fixture:     "ledger-grade-missing-evaluated-actor.json",
			schema:      contracts.SchemaLedgerGrade,
			wantPointer: "/data/evaluated_actor_id",
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

// TestNodeHooksAreNowValidated supersedes the earlier pinned-permissiveness
// test (task t14): pre_run/post_run are no longer unvalidated stubs. Both
// hooks reuse the code node's own operation shape via $ref, and post_run
// additionally requires on_failure — a post-run check failure must route to a
// declared outcome or an explicit assurance rejection, never to silence
// (honesty condition h32). The old fixture's junk shapes — an object with an
// unknown "shape" key, and a bare string — now fail, each with a JSON Pointer
// naming exactly where.
func TestNodeHooksAreNowValidated(t *testing.T) {
	v := newValidator(t)

	loadNode := func(t *testing.T) (doc map[string]any, node map[string]any) {
		t.Helper()
		if err := json.Unmarshal(fixture(t, "valid", "workflow-minimal.json"), &doc); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		spec := doc["spec"].(map[string]any)
		node = spec["nodes"].(map[string]any)["build"].(map[string]any)
		return doc, node
	}

	t.Run("junk shapes are now rejected with a pointer", func(t *testing.T) {
		doc, node := loadNode(t)
		node["pre_run"] = map[string]any{"shape": []any{1, "not specified", nil}}
		node["post_run"] = "not even an object"

		err := v.Validate(contracts.SchemaWorkflow, doc)
		if err == nil {
			t.Fatal("expected the tightened pre_run/post_run schema to reject junk shapes")
		}
		var ve *contracts.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("error is not a *contracts.ValidationError: %T", err)
		}
		var pointers []string
		for _, violation := range ve.Violations {
			pointers = append(pointers, violation.Pointer)
		}
		if !hasPrefix(pointers, "/spec/nodes/build/pre_run") {
			t.Errorf("no violation under /spec/nodes/build/pre_run; got %v", pointers)
		}
		if !hasPrefix(pointers, "/spec/nodes/build/post_run") {
			t.Errorf("no violation under /spec/nodes/build/post_run; got %v", pointers)
		}
	})

	t.Run("post_run without on_failure is rejected", func(t *testing.T) {
		doc, node := loadNode(t)
		node["post_run"] = map[string]any{
			"operation": map[string]any{"image": "repo/hook@sha256:" + strings.Repeat("a", 64), "argv": []any{"true"}},
		}
		if err := v.Validate(contracts.SchemaWorkflow, doc); err == nil {
			t.Fatal("expected post_run without on_failure to be rejected: a post-run failure must map to a declared outcome or an assurance rejection, never silence")
		}
	})

	t.Run("a well-formed hook pair validates", func(t *testing.T) {
		doc, node := loadNode(t)
		node["pre_run"] = map[string]any{
			"operation": map[string]any{"image": "repo/guard@sha256:" + strings.Repeat("a", 64), "argv": []any{"check"}},
		}
		node["post_run"] = map[string]any{
			"operation":  map[string]any{"image": "repo/verify@sha256:" + strings.Repeat("b", 64), "argv": []any{"verify"}},
			"on_failure": map[string]any{"outcome": "completed"},
		}
		if err := v.Validate(contracts.SchemaWorkflow, doc); err != nil {
			t.Errorf("well-formed pre_run/post_run should validate: %v", err)
		}

		// The other on_failure shape — the reject_assurance sentinel — also
		// validates.
		node["post_run"].(map[string]any)["on_failure"] = "reject_assurance"
		if err := v.Validate(contracts.SchemaWorkflow, doc); err != nil {
			t.Errorf("on_failure: reject_assurance should validate: %v", err)
		}
	})
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
