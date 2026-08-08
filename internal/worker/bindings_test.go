package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Binding resolution is tested in-package and without a database: the three
// surfaces are supplied as closures, so what is under test is the pointer
// semantics of PRD §11.2 rather than the store's ability to answer a query.
// The store's half is covered by the end-to-end test.

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
		Projection: func(_ context.Context, kind ledger.ProjectionKind, subject string) (ledger.Projection, error) {
			return ledger.Projection{Kind: kind, Subject: subject, Items: []ledger.Record{}, Digest: "sha256:test"}, nil
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
		Bindings: map[string]string{
			"request":    "/run/input",
			"score":      "/nodes/analyze/output/score",
			"readyTasks": "/ledger/projections/ready_tasks",
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

func TestUnresolvableBindingsFailLoudly(t *testing.T) {
	cases := []struct {
		name    string
		pointer string
		wantMsg string
	}{
		{"node that never ran", "/nodes/never-ran/output", "has no succeeded attempt"},
		{"evidence surface", "/nodes/analyze/evidence", "not resolvable yet"},
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
