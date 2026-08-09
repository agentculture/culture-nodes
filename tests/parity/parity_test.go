// Package paritytest is task t19's CLI/Web-API parity harness
// (docs/plans/2026-08-08-culture-nodes-app-design.md, task t19's brief):
// enumerate the Go control-plane CLI's (cmd/nodes) product verbs and the
// planned Python-front CLI's (culture_nodes) verb surface, and assert every
// one of them maps to a documented operation in api/openapi/openapi.yaml.
//
// This is deliberately narrow. It is an *operation inventory*, not
// enforcement that a CLI verb actually calls the API and only the API —
// that stronger guarantee ("the CLI calls the API, never the engine or the
// store directly") is task t24's job, once the Python front becomes a real
// client of this API. What this harness catches today: a product verb
// whose intended operation was never documented, or was documented and
// then quietly removed from api/openapi/openapi.yaml — the spec and the
// verb surface cannot drift apart without a visible test failure.
package paritytest

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// openapiDoc captures only the path+method+operationId inventory from
// api/openapi/openapi.yaml — see internal/api/contract_test.go for the
// sibling test that walks the same file against the live mux.
type openapiDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
	} `json:"paths"`
}

// loadOperationIDs parses api/openapi/openapi.yaml (sigs.k8s.io/yaml,
// already a go.mod dependency) and returns the set of every documented
// operationId.
func loadOperationIDs(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "api", "openapi", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc openapiDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	ids := make(map[string]bool)
	for _, methods := range doc.Paths {
		for _, op := range methods {
			if op.OperationID != "" {
				ids[op.OperationID] = true
			}
		}
	}
	if len(ids) == 0 {
		t.Fatalf("%s declared no operations", path)
	}
	return ids
}

// verbMapping is one product verb's expected OpenAPI operation.
type verbMapping struct {
	// Verb is a human-readable name for the surface verb — a Go CLI
	// command path (e.g. "nodes validate") or a planned Python-front CLI
	// path (e.g. "run create").
	Verb string
	// OperationID is the api/openapi/openapi.yaml operationId this verb
	// must map to.
	OperationID string
	// Note explains the mapping when it is not immediately obvious (e.g. a
	// verb that is still a CLI stub today).
	Note string
}

// goCLIProductVerbs enumerates cmd/nodes verbs that are, or are documented
// (PRD §12.1) to become, thin callers of this API — as opposed to the
// introspection verbs (whoami, learn, explain, overview, doctor, cli
// overview) and the process-lifecycle/operational verbs with no
// client-facing operation of their own, both listed in
// goCLIOutOfScopeVerbs below with their exclusion rationale.
//
// "validate" is real today (cmd/nodes/validate.go compiles locally rather
// than calling POST /v1alpha1/workflows/validate — routing it through the
// API instead is t24's job); "run" and "inspect" are still recognized-but-
// stubbed process modes (cmd/nodes/modes.go). All three already have a
// documented operation to call once wired, which is exactly what this
// harness checks now, ahead of them actually being wired.
var goCLIProductVerbs = []verbMapping{
	{
		Verb:        "nodes validate",
		OperationID: "validateWorkflow",
		Note:        "compiles locally today; the operation it will call through the API (t24) already exists.",
	},
	{
		Verb:        "nodes run",
		OperationID: "createRun",
		Note:        "PRD §12.1 'create and follow a run'; still a stub (cmd/nodes/modes.go). Following a run additionally uses streamRunEvents, covered elsewhere in this inventory.",
	},
	{
		Verb:        "nodes inspect",
		OperationID: "getRun",
		Note:        "PRD §12.1 'inspect a run or attempt'; still a stub (cmd/nodes/modes.go).",
	},
}

// goCLIOutOfScopeVerbs are cmd/nodes verbs deliberately NOT required to map
// to a documented OpenAPI operation, each with why — recorded rather than
// silently omitted, per this repo's CLAUDE.md "record deviations from the
// PRD explicitly ... don't drift silently" ground rule, applied here to the
// verb-surface inventory itself.
var goCLIOutOfScopeVerbs = map[string]string{
	"nodes migrate": "applies schema migrations directly against PostgreSQL by design " +
		"(docs/adr/0002-migration-policy.md) — an out-of-band operational verb with no REST " +
		"surface, the same reason a k8s pre-install Job runs it rather than the API.",
	"nodes serve": "starts the API server this spec describes; it is the API's host process, not a client of it.",
	"nodes all":   "starts the API server (+ scheduler) for local development; same reason as 'nodes serve'.",
	"nodes scheduler": "a process-lifecycle mode with no client-facing operation of its own (PRD §12.1); " +
		"still a stub (cmd/nodes/modes.go).",
	"nodes worker": "a process-lifecycle mode with no client-facing operation of its own (PRD §12.1); " +
		"still a stub, and worker wiring belongs to a later task (t12).",
	"nodes whoami":       "introspection verb: identity from culture.yaml, not a product verb.",
	"nodes learn":        "introspection verb: self-teaching prompt, not a product verb.",
	"nodes explain":      "introspection verb: verb documentation, not a product verb.",
	"nodes overview":     "introspection verb: descriptive snapshot, not a product verb.",
	"nodes doctor":       "introspection verb: environment/identity checks, not a product verb.",
	"nodes cli overview": "introspection verb: describes the CLI surface itself, not a product verb.",
}

// pythonFrontPlannedVerbs is the planned verb surface of the Python-front
// CLI (culture_nodes — today's mesh-agent scaffold) once it becomes a thin
// client of this API: task t24 per this repo's build plan. The list is
// hardcoded here, not discovered, because t24 has not landed on this branch
// — this is exactly the "operation inventory" this harness's job
// description owns today, ahead of t24's stronger "the CLI calls the API
// and nothing else" enforcement.
var pythonFrontPlannedVerbs = []verbMapping{
	{Verb: "workflow validate", OperationID: "validateWorkflow"},
	{Verb: "workflow publish", OperationID: "publishWorkflow"},
	{Verb: "workflow list", OperationID: "listWorkflows"},
	{Verb: "workflow get", OperationID: "getWorkflow"},
	{Verb: "run create", OperationID: "createRun"},
	{Verb: "run list", OperationID: "listRuns"},
	{Verb: "run get", OperationID: "getRun"},
	{Verb: "run cancel", OperationID: "cancelRun"},
	{Verb: "run events", OperationID: "streamRunEvents"},
	{Verb: "ledger list", OperationID: "listLedgerRecords"},
	{Verb: "ledger projection", OperationID: "getLedgerProjection"},
	{Verb: "review create", OperationID: "createReview"},
	{Verb: "review commit", OperationID: "commitReview"},
}

// assertMapped fails the test if any mapping names an operationId
// api/openapi/openapi.yaml does not document — the harness's actual job.
func assertMapped(t *testing.T, ids map[string]bool, mappings []verbMapping) {
	t.Helper()
	for _, m := range mappings {
		m := m
		t.Run(m.Verb, func(t *testing.T) {
			if !ids[m.OperationID] {
				t.Fatalf("%s has no documented OpenAPI operation %q in api/openapi/openapi.yaml (%s)",
					m.Verb, m.OperationID, m.Note)
			}
		})
	}
}

// TestGoCLIProductVerbsMapToDocumentedOperations is the operation inventory
// for cmd/nodes: every enumerated product verb must name an operationId
// api/openapi/openapi.yaml actually documents.
func TestGoCLIProductVerbsMapToDocumentedOperations(t *testing.T) {
	assertMapped(t, loadOperationIDs(t), goCLIProductVerbs)
}

// TestPlannedPythonFrontVerbsMapToDocumentedOperations is the same
// inventory for t24's planned Python-front verb surface.
func TestPlannedPythonFrontVerbsMapToDocumentedOperations(t *testing.T) {
	assertMapped(t, loadOperationIDs(t), pythonFrontPlannedVerbs)
}

// TestOutOfScopeVerbsAreDocumentedNotSilent proves every entry in
// goCLIOutOfScopeVerbs carries a non-empty rationale — an empty reason
// would be exactly the silent drift this harness exists to prevent, just
// relocated to the exclusion list instead of the mapping itself.
func TestOutOfScopeVerbsAreDocumentedNotSilent(t *testing.T) {
	for verb, reason := range goCLIOutOfScopeVerbs {
		if reason == "" {
			t.Errorf("%s is excluded from the operation-mapping requirement with no recorded reason", verb)
		}
	}
}
