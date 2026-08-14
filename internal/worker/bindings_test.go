package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Binding resolution is tested in-package and without a database: the four
// surfaces are supplied as closures, so what is under test is the pointer
// semantics of PRD §11.2 rather than the store's ability to answer a query.
// The store's half is covered by the end-to-end test.

// The ids the projection closure's record set carries, named so the tests
// can prove a resolved projection contains the records that were actually
// appended rather than merely a well-shaped empty document.
const (
	testTaskRecordID     = ledger.IDPrefix + "00000000000000000000000001"
	testEvidenceRecordID = ledger.IDPrefix + "00000000000000000000000002"
	testNodeEvidenceID   = ledger.IDPrefix + "00000000000000000000000003"
	testNodeRunID        = "nr_00000000000000000000000001"
)

// testLedgerRecords is the record set the Projection closure projects over:
// one ready task and one runner-observed evidence record on it. Real records
// through the real ledger.Project dispatch, so a projection binding resolves
// what a store would answer, not a stub's invention.
func testLedgerRecords() []ledger.Record {
	return []ledger.Record{
		{
			ID:         testTaskRecordID,
			RecordType: ledger.RecordTask,
			RunID:      "run_1",
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: "actor_agent"},
			Authority:  ledger.AuthorityProposed,
			Data:       json.RawMessage(`{"goal":"add /healthz","status":"ready","assurance_state":"unverified"}`),
		},
		{
			ID:         testEvidenceRecordID,
			RecordType: ledger.RecordEvidence,
			RunID:      "run_1",
			Origin:     ledger.Origin{Kind: ledger.OriginRunner, ActorID: "runner_headspace"},
			Authority:  ledger.AuthorityObserved,
			SubjectRef: ledger.NullableID(testTaskRecordID),
			Data:       json.RawMessage(`{"collection_method":"runner_wait_status","completeness":"partial"}`),
		},
	}
}

// testNodeEvidenceRecords is what the NodeEvidence closure answers for the
// `analyze` node: evidence whose identity is the node run (NodeRunID set,
// no SubjectRef — nothing sets one on node evidence), the way the store's
// node_runs join selects it.
func testNodeEvidenceRecords() []ledger.Record {
	return []ledger.Record{
		{
			ID:         testNodeEvidenceID,
			RecordType: ledger.RecordEvidence,
			RunID:      "run_1",
			NodeRunID:  ledger.NullableID(testNodeRunID),
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: "actor_agent"},
			Authority:  ledger.AuthorityProposed,
			Data:       json.RawMessage(`{"collection_method":"agent_self_report","completeness":"partial"}`),
		},
	}
}

func testSources(t *testing.T) bindingSources {
	t.Helper()
	return bindingSources{
		RunID:    "run_1",
		RunInput: json.RawMessage(`{"subject":"widget","tags":["a","b"],"nested":{"deep":7}}`),
		NodeOutput: func(_ context.Context, nodeID string) (json.RawMessage, error) {
			switch nodeID {
			case "analyze":
				return json.RawMessage(`{"score":0.91}`), nil
			case "never-ran":
				return nil, nil
			default:
				return nil, errors.New("unexpected node " + nodeID)
			}
		},
		NodeEvidence: func(_ context.Context, nodeID string) ([]ledger.Record, error) {
			switch nodeID {
			case "analyze":
				return testNodeEvidenceRecords(), nil
			case "never-ran":
				return nil, nil
			default:
				return nil, errors.New("unexpected node " + nodeID)
			}
		},
		Projection: func(_ context.Context, kind ledger.ProjectionKind, subject string) (ledger.Projection, error) {
			return ledger.Project(testLedgerRecords(), kind, subject)
		},
	}
}

func TestResolveNodeInputFromPointer(t *testing.T) {
	cases := []struct {
		name    string
		pointer string
		want    string
	}{
		{"whole run input", "/run/input", `{"subject":"widget","tags":["a","b"],"nested":{"deep":7}}`},
		{"member of run input", "/run/input/subject", `"widget"`},
		{"array element", "/run/input/tags/1", `"b"`},
		{"nested member", "/run/input/nested/deep", `7`},
		{"node output", "/nodes/analyze/output", `{"score":0.91}`},
		{"member of node output", "/nodes/analyze/output/score", `0.91`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveNodeInput(context.Background(), testSources(t), &inputBinding{From: tc.pointer})
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.pointer, err)
			}
			assertEqualJSON(t, got, tc.want)
		})
	}
}

func TestResolveNodeInputBindingsMap(t *testing.T) {
	got, err := resolveNodeInput(context.Background(), testSources(t), &inputBinding{
		Bindings: map[string]bindingValue{
			"request":    {Pointer: "/run/input"},
			"score":      {Pointer: "/nodes/analyze/output/score"},
			"readyTasks": {Pointer: "/ledger/projections/ready_tasks"},
		},
	})
	if err != nil {
		t.Fatalf("resolve bindings: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode resolved bindings: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("resolved %d bindings, want 3: %s", len(decoded), got)
	}
	assertEqualJSON(t, decoded["score"], `0.91`)
	if !strings.Contains(string(decoded["readyTasks"]), `"ready_tasks"`) {
		t.Errorf("readyTasks = %s, want the projection document", decoded["readyTasks"])
	}
}

// A node with no declared binding gets `{}`, not the run input. Inheriting
// the run input by default would make every actor's contract implicitly
// depend on the workflow's.
func TestUndeclaredBindingResolvesToAnEmptyObject(t *testing.T) {
	for _, binding := range []*inputBinding{nil, {}} {
		got, err := resolveNodeInput(context.Background(), testSources(t), binding)
		if err != nil {
			t.Fatalf("resolve %v: %v", binding, err)
		}
		assertEqualJSON(t, got, `{}`)
	}
}

// TestEvidenceBindingResolvesAppendedRecords proves the `evidence` binding
// resolves the run's actual evidence: its empty subject reads as unscoped, so
// the projection carries the evidence record the record set holds — not an
// empty items list a consumer could mistake for "no evidence was appended".
func TestEvidenceBindingResolvesAppendedRecords(t *testing.T) {
	got, err := resolvePointer(context.Background(), testSources(t), "/ledger/projections/evidence")
	if err != nil {
		t.Fatalf("resolve /ledger/projections/evidence: %v", err)
	}

	var projection ledger.Projection
	if err := json.Unmarshal(got, &projection); err != nil {
		t.Fatalf("decode resolved projection: %v", err)
	}
	if projection.Kind != ledger.KindEvidenceFor {
		t.Errorf("kind = %q, want %q", projection.Kind, ledger.KindEvidenceFor)
	}
	if projection.Subject != "" {
		t.Errorf("subject = %q, want empty: the binding asks for the run's evidence, not one reference's", projection.Subject)
	}
	if len(projection.Items) != 1 || projection.Items[0].ID != testEvidenceRecordID {
		t.Fatalf("items = %v, want exactly the appended evidence record %s", ids(projection.Items), testEvidenceRecordID)
	}
	if projection.Items[0].RecordType != ledger.RecordEvidence {
		t.Errorf("resolved record type = %q, want evidence", projection.Items[0].RecordType)
	}
}

// TestNodeEvidenceBindingResolvesThatNodesRecords is task t7's flip: before
// it, /nodes/<id>/evidence was compiler-accepted but resolver-refused. It now
// resolves to exactly that node's evidence records — identity is the node
// run, not a SubjectRef (node evidence never carries one) — as a JSON array
// a deeper pointer can address into.
func TestNodeEvidenceBindingResolvesThatNodesRecords(t *testing.T) {
	got, err := resolvePointer(context.Background(), testSources(t), "/nodes/analyze/evidence")
	if err != nil {
		t.Fatalf("resolve /nodes/analyze/evidence: %v", err)
	}

	var records []ledger.Record
	if err := json.Unmarshal(got, &records); err != nil {
		t.Fatalf("decode resolved evidence: %v", err)
	}
	if len(records) != 1 || records[0].ID != testNodeEvidenceID {
		t.Fatalf("records = %v, want exactly the node's evidence record %s", ids(records), testNodeEvidenceID)
	}
	if records[0].RecordType != ledger.RecordEvidence {
		t.Errorf("resolved record type = %q, want evidence", records[0].RecordType)
	}
	if records[0].NodeRunID.String() != testNodeRunID {
		t.Errorf("node_run_id = %q, want %q: the node run is evidence's identity", records[0].NodeRunID, testNodeRunID)
	}

	// A pointer may keep walking into the resolved array.
	deeper, err := resolvePointer(context.Background(), testSources(t), "/nodes/analyze/evidence/0/data/collection_method")
	if err != nil {
		t.Fatalf("resolve into the evidence array: %v", err)
	}
	assertEqualJSON(t, deeper, `"agent_self_report"`)
}

// A node that appended no evidence resolves to an empty array, not an error:
// unlike a missing output, "zero evidence records" is itself the true answer.
func TestNodeEvidenceBindingResolvesToEmptyArrayWhenNoneAppended(t *testing.T) {
	got, err := resolvePointer(context.Background(), testSources(t), "/nodes/never-ran/evidence")
	if err != nil {
		t.Fatalf("resolve /nodes/never-ran/evidence: %v", err)
	}
	assertEqualJSON(t, got, `[]`)
}

func ids(records []ledger.Record) []string {
	out := make([]string, len(records))
	for i, rec := range records {
		out[i] = rec.ID
	}
	return out
}

func TestProjectionBindingVocabulary(t *testing.T) {
	// The compiler's accepted projection names and the ledger's projection
	// kinds are two vocabularies; every accepted name must map to a kind, or a
	// definition the compiler blessed would fail at dispatch time.
	compilerNames := []string{
		"current_scope", "confirmed_claims", "open_assumptions", "open_questions",
		"ready_tasks", "active_tasks", "verification_queue", "decision_history",
		"evidence", "delivery_summary",
	}
	for _, name := range compilerNames {
		if _, _, ok := projectionKindFor(name); !ok {
			t.Errorf("projection %q is accepted by the compiler but does not map to a ledger projection", name)
		}
	}
	if _, _, ok := projectionKindFor("invented_projection"); ok {
		t.Error("an unknown projection name resolved; §10.9's vocabulary is closed on purpose")
	}
}

// TestCompilerAndResolverAgreeOnNodeBindingSurfaces closes t7's trap in both
// directions: a per-node surface the compiler accepts in a data binding must
// be answered by this resolver, and a surface this resolver refuses must be
// a compile error — otherwise a workflow could publish a binding that only
// fails at dispatch time. The compiler's verdict comes from actually
// compiling a workflow that binds the surface, not from a mirrored list that
// could drift.
func TestCompilerAndResolverAgreeOnNodeBindingSurfaces(t *testing.T) {
	// The union of every name either side has treated as a node surface,
	// plus an invented one proving the harness can tell the verdicts apart.
	for _, surface := range []string{"output", "evidence", "artifacts", "error", "invented"} {
		t.Run(surface, func(t *testing.T) {
			compilerAccepts := compilesWithNodeSurface(t, surface)
			resolverAnswers := resolverAnswersNodeSurface(t, surface)
			if compilerAccepts != resolverAnswers {
				t.Errorf("compiler accepts a /nodes/<id>/%s binding: %v, resolver answers it: %v — the verdicts must agree in both directions",
					surface, compilerAccepts, resolverAnswers)
			}
		})
	}
}

// compilesWithNodeSurface compiles a minimal two-node workflow whose second
// node's input binds /nodes/dep/<surface>, and reports whether the compiler
// accepted it.
func compilesWithNodeSurface(t *testing.T, surface string) bool {
	t.Helper()
	doc := fmt.Sprintf(`
apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata:
  name: surface-agreement
  version: 1.0.0
  ownerRef: team/platform-ai
spec:
  entry: dep
  contract:
    input:
      schemaRef: ./contracts/in.schema.json
    output:
      schemaRef: ./contracts/out.schema.json
  nodes:
    dep:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/dep@sha256:aaaaaa
      contract:
        outcomes:
          completed:
            schemaRef: ./contracts/dep.schema.json
    consume:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/consume@sha256:bbbbbb
      input:
        from: /nodes/dep/%s
      contract:
        outcomes:
          completed:
            schemaRef: ./contracts/consume.schema.json
    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/consume/output
  edges:
    - from: dep.completed
      to: consume
    - from: consume.completed
      to: finish
`, surface)

	_, diags, err := compiler.Compile([]byte(doc), compiler.FormatYAML)
	if err != nil {
		t.Fatalf("compile the agreement fixture: %v", err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			return false
		}
	}
	return true
}

// resolverAnswersNodeSurface reports whether this package's resolver answers
// the surface at all. A data-level error (a node with no output yet, a
// missing member) still counts as answered: the surface exists and the run's
// data was the problem — only a "not resolvable" refusal counts as no.
func resolverAnswersNodeSurface(t *testing.T, surface string) bool {
	t.Helper()
	_, err := resolvePointer(context.Background(), testSources(t), "/nodes/analyze/"+surface)
	if err == nil {
		return true
	}
	return !strings.Contains(err.Error(), "not resolvable")
}

func TestUnresolvableBindingsFailLoudly(t *testing.T) {
	cases := []struct {
		name    string
		pointer string
		wantMsg string
	}{
		{"node that never ran", "/nodes/never-ran/output", "has no succeeded attempt"},
		{"artifacts surface", "/nodes/analyze/artifacts", "not resolvable"},
		{"error surface", "/nodes/analyze/error", "not resolvable"},
		{"artifacts root", "/artifacts/workspace", "not resolvable"},
		{"missing member", "/run/input/absent", `no member "absent"`},
		{"index past the end", "/run/input/tags/9", `no element "9"`},
		{"into a scalar", "/run/input/subject/deeper", "cannot address"},
		{"unknown projection", "/ledger/projections/nonsense", "not one of PRD"},
		{"not a pointer", "run/input", "must start with"},
		{"empty pointer", "", "empty pointer"},
		{"bare run", "/run", "only run surface"},
		{"bare ledger", "/ledger", "only ledger surface"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePointer(context.Background(), testSources(t), tc.pointer)
			if err == nil {
				t.Fatalf("resolve %q succeeded; an unresolvable binding must fail loudly", tc.pointer)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

// RFC 6901 escaping, in the order the spec requires: ~1 before ~0, so an
// escaped tilde followed by a one cannot be misread as an escaped slash.
func TestPointerEscaping(t *testing.T) {
	src := bindingSources{RunInput: json.RawMessage(`{"a/b":1,"c~d":2,"e~1f":3}`)}
	for pointer, want := range map[string]string{
		"/run/input/a~1b":  `1`,
		"/run/input/c~0d":  `2`,
		"/run/input/e~01f": `3`,
	} {
		got, err := resolvePointer(context.Background(), src, pointer)
		if err != nil {
			t.Fatalf("resolve %q: %v", pointer, err)
		}
		assertEqualJSON(t, got, want)
	}
}

func assertEqualJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	gotCanonical, _ := json.Marshal(a)
	wantCanonical, _ := json.Marshal(b)
	if string(gotCanonical) != string(wantCanonical) {
		t.Errorf("resolved = %s, want %s", gotCanonical, wantCanonical)
	}
}
