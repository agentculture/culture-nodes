// Package paritytest is task t19's CLI/Web-API parity harness
// (docs/plans/2026-08-08-culture-nodes-app-design.md, task t19's brief):
// enumerate the Go control-plane CLI's (cmd/nodes) product verbs, the
// Python-front CLI's (culture_nodes) verb surface, and the web client's
// (web/src/api) operation surface, and assert every one of them maps to a
// documented operation in api/openapi/openapi.yaml — extended by task t16
// to also cover a handful of query parameters, and to add the web-client
// inventory alongside the pre-existing Go/Python ones.
//
// This is deliberately narrow. It is an *operation inventory*, not
// enforcement that a CLI verb actually calls the API and only the API, nor
// that the web client actually implements a given function today — that
// stronger guarantee ("the CLI calls the API, never the engine or the store
// directly") is task t24's job. What this harness catches today: a product
// verb (or, since t16, a query parameter, or a web-client operation) whose
// intended mapping was never documented, or was documented and then
// quietly removed from api/openapi/openapi.yaml — the spec and every
// front's verb surface cannot drift apart without a visible test failure.
package paritytest

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// openapiParam is one query/path/etc. parameter entry as
// api/openapi/openapi.yaml declares it inline on an operation.
type openapiParam struct {
	Name string `json:"name"`
	In   string `json:"in"`
}

// openapiOperation is one path+method entry's operationId and its inline
// (non-$ref) parameters.
type openapiOperation struct {
	OperationID string         `json:"operationId"`
	Parameters  []openapiParam `json:"parameters"`
}

// openapiDoc captures the path+method+operationId+parameters inventory from
// api/openapi/openapi.yaml — see internal/api/contract_test.go for the
// sibling test that walks the same file against the live mux. Parameters is
// intentionally only ever read for its inline (non-$ref) query entries —
// see operationQueryParams below — since every query parameter this harness
// checks today is declared inline rather than via components/parameters.
type openapiDoc struct {
	Paths map[string]map[string]openapiOperation `json:"paths"`
}

// loadOpenAPIDoc parses api/openapi/openapi.yaml (sigs.k8s.io/yaml, already
// a go.mod dependency) into an openapiDoc.
func loadOpenAPIDoc(t *testing.T) openapiDoc {
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
	return doc
}

// operationIDs returns the set of every operationId doc declares.
func operationIDs(doc openapiDoc) map[string]bool {
	ids := make(map[string]bool)
	for _, methods := range doc.Paths {
		for _, op := range methods {
			if op.OperationID != "" {
				ids[op.OperationID] = true
			}
		}
	}
	return ids
}

// operationQueryParams returns, per operationId, the set of query parameter
// names declared inline on that operation.
func operationQueryParams(doc openapiDoc) map[string]map[string]bool {
	params := make(map[string]map[string]bool)
	for _, methods := range doc.Paths {
		for _, op := range methods {
			addOperationQueryParams(params, op)
		}
	}
	return params
}

// addOperationQueryParams records op's inline query-parameter names into
// params, creating the per-operation set on first use. A blank OperationID
// (an undocumented path/method) contributes nothing.
func addOperationQueryParams(params map[string]map[string]bool, op openapiOperation) {
	if op.OperationID == "" {
		return
	}
	set := params[op.OperationID]
	if set == nil {
		set = make(map[string]bool)
		params[op.OperationID] = set
	}
	for _, p := range op.Parameters {
		if p.In == "query" {
			set[p.Name] = true
		}
	}
}

// loadOperationIDs parses api/openapi/openapi.yaml and returns the set of
// every documented operationId.
func loadOperationIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := operationIDs(loadOpenAPIDoc(t))
	if len(ids) == 0 {
		t.Fatalf("api/openapi/openapi.yaml declared no operations")
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

// pythonFrontVerbs is the verb surface of the Python-front CLI
// (culture_nodes — a thin REST client per spec decision c28) mapped to the
// operation each verb calls. The list is hardcoded here, not discovered by
// reading culture_nodes source, matching this harness's deliberately narrow
// "operation inventory" design (see the package doc) rather than a stronger
// "the CLI genuinely implements this" guarantee — the same shallow
// mechanism goCLIProductVerbs above has always used for cmd/nodes.
//
// human-tasks list/get/decide and node-runs list were added by task t16
// alongside the culture_nodes verbs that call them
// (culture_nodes/cli/_commands/human_tasks.py, node_runs.py); run list's
// updated_since/updated_until/sort are covered separately by
// documentedQueryParams below, since they extend an existing verb/operation
// pair rather than adding a new one.
var pythonFrontVerbs = []verbMapping{
	{Verb: "workflow validate", OperationID: "validateWorkflow"},
	{Verb: "workflow publish", OperationID: "publishWorkflow"},
	{Verb: "workflow list", OperationID: "listWorkflows"},
	{Verb: "workflow get", OperationID: "getWorkflow"},
	{Verb: "run create", OperationID: "createRun"},
	{Verb: "run list", OperationID: "listRuns"},
	{Verb: "run get", OperationID: "getRun"},
	{Verb: "run cancel", OperationID: "cancelRun"},
	{Verb: "run events", OperationID: "streamRunEvents"},
	{Verb: "node-runs list", OperationID: "listNodeRuns", Note: "task t16, cmd/_commands/node_runs.py."},
	{Verb: "ledger list", OperationID: "listLedgerRecords"},
	{Verb: "ledger projection", OperationID: "getLedgerProjection"},
	{Verb: "review create", OperationID: "createReview"},
	{Verb: "review commit", OperationID: "commitReview"},
	{Verb: "human-tasks list", OperationID: "listHumanTasks", Note: "task t16, cmd/_commands/human_tasks.py."},
	{Verb: "human-tasks get", OperationID: "getHumanTask", Note: "task t16, cmd/_commands/human_tasks.py."},
	{Verb: "human-tasks decide", OperationID: "decideHumanTask", Note: "task t16, cmd/_commands/human_tasks.py; bearer-auth'd."},
}

// documentedQueryParams names, per operationId, query parameters this
// harness requires api/openapi/openapi.yaml to keep documenting — the
// param-level half of task t16's brief ("fails if any of the new
// endpoints/params is missing from openapi"), complementing verbMapping's
// whole-operation check above with a narrower one scoped to specific query
// parameters a front actually relies on.
type paramMapping struct {
	// OperationID is the api/openapi/openapi.yaml operationId the params
	// below must be documented on.
	OperationID string
	// Params are query parameter names that must appear in that
	// operation's documented parameters.
	Params []string
	// Note explains which front(s) consume the parameters.
	Note string
}

var documentedQueryParams = []paramMapping{
	{
		OperationID: "listRuns",
		Params:      []string{"state", "updated_since", "updated_until", "sort", "limit"},
		Note:        "task t11's updated_at window + sort; consumed by 'nodes run list --updated-since/--updated-until/--sort' (task t16).",
	},
	{
		OperationID: "listNodeRuns",
		Params:      []string{"updated_since", "updated_until", "cursor", "limit"},
		Note:        "the cross-run jobs timeline's keyset pagination (task t11); consumed by 'nodes node-runs list --cursor/--limit' (task t16).",
	},
	{
		OperationID: "listHumanTasks",
		Params:      []string{"status", "limit"},
		Note:        "consumed by 'nodes human-tasks list --status' (task t16).",
	},
}

// webClientOperations is the operation surface web/src/api/client.ts
// exposes today, plus the new task-t16 operations it is expected to grow
// into once a later task wires them up (web/src is out of scope for t16 —
// see this repo's CLAUDE.md worktree rules). Exactly like pythonFrontVerbs,
// this is a hardcoded inventory keyed by operationId, not an introspection
// of the TypeScript source: it cannot tell whether web/src/api/client.ts
// genuinely calls a given endpoint, only that the endpoint it names remains
// documented. That is enough to satisfy this harness's actual job — if a
// later change removed listHumanTasks from api/openapi/openapi.yaml, this
// list would catch it exactly like it would for the Go or Python fronts —
// while leaving the "does the web client really implement it" question to
// whatever task next touches web/src/api.
var webClientOperations = []verbMapping{
	{Verb: "web listRuns", OperationID: "listRuns", Note: "web/src/api/client.ts listRuns()."},
	{Verb: "web getRun", OperationID: "getRun", Note: "web/src/api/client.ts getRun()."},
	{Verb: "web getWorkflow", OperationID: "getWorkflow", Note: "web/src/api/client.ts getWorkflow()."},
	{Verb: "web listLedgerRecords", OperationID: "listLedgerRecords", Note: "web/src/api/client.ts getLedger()."},
	{Verb: "web getLedgerProjection", OperationID: "getLedgerProjection", Note: "web/src/api/client.ts getProjection()."},
	{Verb: "web streamRunEvents", OperationID: "streamRunEvents", Note: "web/src/api/client.ts runEventsUrl()."},
	{Verb: "web listNodeRuns", OperationID: "listNodeRuns", Note: "task t16 surface; not yet wired into web/src/api/client.ts."},
	{Verb: "web listHumanTasks", OperationID: "listHumanTasks", Note: "task t16 surface; not yet wired into web/src/api/client.ts."},
	{Verb: "web getHumanTask", OperationID: "getHumanTask", Note: "task t16 surface; not yet wired into web/src/api/client.ts."},
	{Verb: "web decideHumanTask", OperationID: "decideHumanTask", Note: "task t16 surface; not yet wired into web/src/api/client.ts."},
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

// assertParamsDocumented fails the test if any mapping names a query
// parameter api/openapi/openapi.yaml does not declare (inline) on its
// operationId — the param-level companion to assertMapped above.
func assertParamsDocumented(t *testing.T, params map[string]map[string]bool, mappings []paramMapping) {
	t.Helper()
	for _, m := range mappings {
		m := m
		t.Run(m.OperationID, func(t *testing.T) {
			declared := params[m.OperationID]
			for _, name := range m.Params {
				if !declared[name] {
					t.Fatalf("operation %q has no documented query parameter %q in api/openapi/openapi.yaml (%s)",
						m.OperationID, name, m.Note)
				}
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

// TestPythonFrontVerbsMapToDocumentedOperations is the same inventory for
// the Python-front CLI's verb surface, including the human-tasks and
// node-runs verbs task t16 added.
func TestPythonFrontVerbsMapToDocumentedOperations(t *testing.T) {
	assertMapped(t, loadOperationIDs(t), pythonFrontVerbs)
}

// TestWebClientOperationsMapToDocumentedOperations is the same inventory
// for the web client's operation surface (see webClientOperations' doc for
// why this does not require web/src changes to add or to pass).
func TestWebClientOperationsMapToDocumentedOperations(t *testing.T) {
	assertMapped(t, loadOperationIDs(t), webClientOperations)
}

// TestDocumentedQueryParamsArePresent is the param-level inventory task t16
// added: every query parameter a front relies on (documentedQueryParams)
// must still be documented on its operationId.
func TestDocumentedQueryParamsArePresent(t *testing.T) {
	assertParamsDocumented(t, operationQueryParams(loadOpenAPIDoc(t)), documentedQueryParams)
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
