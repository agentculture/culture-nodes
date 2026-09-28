package api_test

// Issue #332 (task t43): a human task's context refs are resolved server-side
// and returned beside the raw refs, so the inbox can say WHAT is being
// decided instead of printing `fix: /nodes/fix/output`.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
)

// prUpkeepInput is the shape of a real pr-upkeep run input (PR #326).
const prUpkeepInput = `{"number":326,"source":"github_pr","repository":"agentculture/culture-nodes",
 "head_sha":"e128a17aa0000000000000000000000000000000",
 "findings":[{"id":"AZ1","file":"tests/test_hand_turn_cli.py","line":141,"rule":"python:S9073",
  "kind":"CODE_SMELL","severity":"MAJOR","source":"sonarcloud",
  "title":"Split this composite assertion into separate assertions."}]}`

// fixSummary is the fix node's markdown output.
const fixSummary = `{"scope":"s","summary":"**Finding taken:** python:S9073\n\n**Verdict:** real defect, fixed"}`

// blockedInput is a declaration human.ask run input (#328 t40e).
const blockedInput = `{"question":"The pr-upkeep stamp-pr agent could not complete its task.",
 "blocked_step":"stamp-pr","ticket":"SCRUM-15","number":"329","repository":"agentculture/culture-nodes",
 "work_item":"gh:agentculture/culture-nodes#329",
 "agent_report":{"reason":"no push credential","pr":329,"evidence":["git push: 403"]},"marker":"m1"}`

// newMinimalRuns publishes the minimal workflow once (a second publish of
// the same source answers 200, which createMinimalRun refuses) and starts n
// runs of it.
func newMinimalRuns(t *testing.T, f *fixture, n int) []string {
	t.Helper()
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(readFixtureWorkflow(t, "minimal.workflow.yaml"))}, &published)
	requireStatus(t, resp, body, http.StatusCreated)
	ids := make([]string, n)
	for i := range ids {
		var run apipkg.RunOut
		resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
			createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
		requireStatus(t, resp, body, http.StatusCreated)
		ids[i] = run.ID
	}
	return ids
}

func setRunInput(t *testing.T, f *fixture, runID, input string) {
	t.Helper()
	if _, err := f.store.Pool().Exec(t.Context(), `UPDATE runs SET input = $1::jsonb WHERE id = $2`, input, runID); err != nil {
		t.Fatalf("set run input: %v", err)
	}
}

func insertHumanTask(t *testing.T, f *fixture, runID, request string) string {
	t.Helper()
	id := store.NewULID()
	if _, err := f.store.Pool().Exec(t.Context(), `
		INSERT INTO human_tasks (id, namespace_id, run_id, kind, status, request, created_at)
		VALUES ($1, $2, $3, 'approval', 'pending', $4::jsonb, now())`,
		id, f.nsID, runID, request); err != nil {
		t.Fatalf("insert human task: %v", err)
	}
	return id
}

func getHumanTask(t *testing.T, f *fixture, id string) apipkg.HumanTaskOut {
	t.Helper()
	var out apipkg.HumanTaskOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+id), nil, &out)
	requireStatus(t, resp, body, http.StatusOK)
	return out
}

func contextByName(t *testing.T, task apipkg.HumanTaskOut) map[string]apipkg.HumanTaskContextValueOut {
	t.Helper()
	out := map[string]apipkg.HumanTaskContextValueOut{}
	for _, v := range task.ResolvedContext {
		out[v.Name] = v
	}
	return out
}

func jsonEqual(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("decode want %s: %v", want, err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// TestHumanTaskResolvesGraphApprovalRefs is the `human-merges-pr` shape:
// bindings to a node output, the run input, a sub-pointer, a literal, and a
// node that never produced output.
func TestHumanTaskResolvesGraphApprovalRefs(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, engineTask := advanceToReviewWithOutput(t, f, json.RawMessage(fixSummary))
	setRunInput(t, f, run.ID, prUpkeepInput)

	// The engine-written task: `from: /nodes/intake/output`, resolved to
	// the value the run view lists as intake's succeeded attempt result.
	view := getRunView(t, f, run.ID)
	var intakeResult json.RawMessage
	for _, nr := range view.NodeRuns {
		if nr.NodeID == "intake" {
			for _, a := range nr.Attempts {
				if a.Status == "succeeded" {
					intakeResult = a.Result
				}
			}
		}
	}
	if intakeResult == nil {
		t.Fatalf("run view carries no succeeded intake attempt: %+v", view.NodeRuns)
	}
	fetched := getHumanTask(t, f, engineTask.ID)
	if len(fetched.ResolvedContext) != 1 {
		t.Fatalf("resolved_context = %+v, want one entry for `from`", fetched.ResolvedContext)
	}
	from := fetched.ResolvedContext[0]
	if from.Name != "from" || from.Ref != "/nodes/intake/output" || from.Unresolved != "" {
		t.Fatalf("from entry = %+v", from)
	}
	// Visibility: exactly what GET /runs/{id} already returns this caller.
	if !jsonEqual(t, from.Value, intakeResult) {
		t.Fatalf("resolved from = %s, run view attempt result = %s", from.Value, intakeResult)
	}

	id := insertHumanTask(t, f, run.ID, `{
		"approver_ref":"group/platform-maintainers",
		"decision_schema_ref":"schema://pr-upkeep/merge-decision/v1",
		"allowed_outcomes":["approved","rejected"],
		"context_refs":{"bindings":{
			"fix":"/nodes/intake/output",
			"finding":"/run/input",
			"head":"/run/input/head_sha",
			"readiness":"/nodes/readiness/output",
			"evidence":"/nodes/intake/evidence",
			"observe":{"literal":{"kind":"github_pr_merged"}}}},
		"audit":{"node_id":"human-merges-pr"}}`)
	task := getHumanTask(t, f, id)

	// Raw refs are kept verbatim.
	if !strings.Contains(string(task.Request), `"fix":"/nodes/intake/output"`) &&
		!strings.Contains(string(task.Request), `"fix": "/nodes/intake/output"`) {
		t.Fatalf("request lost its raw refs: %s", task.Request)
	}
	// Display order: bindings by name.
	var names []string
	for _, v := range task.ResolvedContext {
		names = append(names, v.Name)
	}
	if got := strings.Join(names, ","); got != "evidence,finding,fix,head,observe,readiness" {
		t.Fatalf("resolved_context order = %s", got)
	}
	byName := contextByName(t, task)
	if v := byName["finding"]; v.Ref != "/run/input" || !jsonEqual(t, v.Value, json.RawMessage(prUpkeepInput)) {
		t.Fatalf("finding = %+v", v)
	}
	if !jsonEqual(t, byName["finding"].Value, getRunView(t, f, run.ID).Run.Input) {
		t.Fatalf("finding differs from the run view's run.input")
	}
	if v := byName["fix"]; !strings.Contains(string(v.Value), "Finding taken") {
		t.Fatalf("fix = %+v", v)
	}
	if v := byName["head"]; string(v.Value) != `"e128a17aa0000000000000000000000000000000"` {
		t.Fatalf("head = %+v", v)
	}
	if v := byName["observe"]; !v.Literal || v.Ref != "" || !jsonEqual(t, v.Value, json.RawMessage(`{"kind":"github_pr_merged"}`)) {
		t.Fatalf("observe literal = %+v", v)
	}
	// A node with no succeeded attempt: named, never a fabricated value.
	if v := byName["readiness"]; v.Value != nil || !strings.Contains(v.Unresolved, `"readiness" has no succeeded attempt`) {
		t.Fatalf("readiness = %+v", v)
	}
	if v := byName["evidence"]; v.Value != nil || !strings.Contains(v.Unresolved, "ledger") {
		t.Fatalf("evidence = %+v", v)
	}

	// The list carries the same resolution as the detail read.
	var list apipkg.HumanTaskListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks?status=pending"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	found := false
	for _, item := range list.Items {
		if item.ID == id {
			found = true
			if len(item.ResolvedContext) != 6 {
				t.Fatalf("list item resolved_context = %+v", item.ResolvedContext)
			}
		}
	}
	if !found {
		t.Fatalf("task %s missing from list", id)
	}

	// Principal rules unchanged: the read stays ungated exactly as before,
	// and the decision route still refuses an unauthenticated caller.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+id), nil, nil)
	requireStatus(t, resp, body, http.StatusOK)
	resp2, body2 := authedDecide(t, f, id, "", decideHumanTaskReq{Outcome: "approved", DeciderActorID: "x"})
	requireStatus(t, resp2, body2, http.StatusUnauthorized)
}

// TestHumanTaskResolvesDeclarationAskRefs is the declaration human.ask
// shape: `from: /run/input` carrying the question and the agent report.
func TestHumanTaskResolvesDeclarationAskRefs(t *testing.T) {
	f := newFixture(t)
	runID := newMinimalRuns(t, f, 1)[0]
	setRunInput(t, f, runID, blockedInput)
	id := insertHumanTask(t, f, runID, `{"allowed_outcomes":["acknowledged","rejected"],
		"context_refs":{"from":"/run/input"},"audit":{"node_id":"blocked-stamp-pr"}}`)

	task := getHumanTask(t, f, id)
	if len(task.ResolvedContext) != 1 {
		t.Fatalf("resolved_context = %+v", task.ResolvedContext)
	}
	v := task.ResolvedContext[0]
	if v.Name != "from" || v.Ref != "/run/input" || v.Truncated || v.Unresolved != "" {
		t.Fatalf("from = %+v", v)
	}
	if !jsonEqual(t, v.Value, json.RawMessage(blockedInput)) {
		t.Fatalf("from value = %s", v.Value)
	}

	// A task with no context refs carries no resolved_context at all.
	bare := getHumanTask(t, f, insertHumanTask(t, f, runID, `{"audit":{"node_id":"gate"}}`))
	if bare.ResolvedContext != nil {
		t.Fatalf("bare task resolved_context = %+v, want absent", bare.ResolvedContext)
	}
}

// TestHumanTaskContextValuesAreBounded: an oversize value is shrunk and
// marked truncated; one that cannot shrink under the bound is omitted with a
// reason instead of being sent whole.
func TestHumanTaskContextValuesAreBounded(t *testing.T) {
	f := newFixture(t)
	const bound = 16 << 10

	runIDs := newMinimalRuns(t, f, 2)
	shrinkable, wide := runIDs[0], runIDs[1]
	longSummary := strings.Repeat("é", 20_000) // 40 000 bytes of 2-byte runes
	items := make([]int, 500)
	big, _ := json.Marshal(map[string]any{"summary": longSummary, "items": items, "number": 326})
	setRunInput(t, f, shrinkable, string(big))
	task := getHumanTask(t, f, insertHumanTask(t, f, shrinkable, `{"context_refs":{"from":"/run/input"}}`))
	v := task.ResolvedContext[0]
	if !v.Truncated || v.Unresolved != "" || len(v.Value) > bound {
		t.Fatalf("shrinkable: truncated=%v unresolved=%q len=%d", v.Truncated, v.Unresolved, len(v.Value))
	}
	var shrunk struct {
		Summary string `json:"summary"`
		Items   []int  `json:"items"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(v.Value, &shrunk); err != nil {
		t.Fatalf("shrunk value is not valid JSON: %v", err)
	}
	if shrunk.Number != 326 || !strings.HasSuffix(shrunk.Summary, "…") || len(shrunk.Items) != 50 {
		t.Fatalf("shrunk = number %d, summary len %d, items %d", shrunk.Number, len(shrunk.Summary), len(shrunk.Items))
	}
	if !strings.HasPrefix(shrunk.Summary, "é") || strings.ContainsRune(shrunk.Summary, '�') {
		t.Fatal("summary was cut inside a rune")
	}

	// Thousands of short keys cannot be shrunk: omitted, with a reason.
	keysMap := map[string]int{}
	for i := 0; i < 3000; i++ {
		keysMap[store.NewULID()] = i
	}
	wideJSON, _ := json.Marshal(keysMap)
	setRunInput(t, f, wide, string(wideJSON))
	task = getHumanTask(t, f, insertHumanTask(t, f, wide, `{"context_refs":{"from":"/run/input"}}`))
	v = task.ResolvedContext[0]
	if v.Value != nil || !strings.Contains(v.Unresolved, "run view") {
		t.Fatalf("unshrinkable: value len %d unresolved %q", len(v.Value), v.Unresolved)
	}
}
