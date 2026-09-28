package api_test

// Task t35 (#328; spec c86, honesty h59): both lanes that produce
// automation source -- workflow generation and devague plan import --
// emit declarations that pass the real declaration validator and publish
// through the real declaration publish path (declengine.Publish, the t19
// choke point), with must links where the workflow/plan had edges. These
// are the declaration-output counterparts of the lanes' existing tests
// (workflow_generations_test.go, planimports_test.go and
// cmd/nodes/planimport_test.go), which stay green: the default output of
// both lanes is unchanged until t37 retires the graph engine.
//
// Three properties every test below also pins:
//   - the recorded author is the authenticated principal, never a body field;
//   - nothing the lanes publish is active (ADR 0014 §3: a human activates);
//   - publish warnings (reference and t30 sensitivity) are surfaced in the
//     lane's own output, never swallowed.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	api "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// --- wire shapes (mirroring api/openapi/openapi.yaml) -----------------------

type declSetEntryWire struct {
	Name        string   `json:"name"`
	Format      string   `json:"format"`
	Source      string   `json:"source"`
	Valid       bool     `json:"valid"`
	Digest      string   `json:"digest"`
	Diagnostics []string `json:"diagnostics"`
	Warnings    []string `json:"warnings"`
	BaseDigest  string   `json:"base_digest"`
	Diff        string   `json:"diff"`
	VersionID   string   `json:"version_id"`
	Version     int      `json:"version"`
	Author      string   `json:"author"`
}

type declSetLinkWire struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type declSetWire struct {
	Declarations []declSetEntryWire `json:"declarations"`
	Links        []declSetLinkWire  `json:"links"`
	Valid        bool               `json:"valid"`
	Published    bool               `json:"published"`
	Diagnostics  []string           `json:"diagnostics"`
	Warnings     []string           `json:"warnings"`
}

type generationWire struct {
	RunID        string       `json:"run_id"`
	Status       string       `json:"status"`
	Output       string       `json:"output"`
	Source       string       `json:"source"`
	Valid        bool         `json:"valid"`
	Declarations *declSetWire `json:"declarations"`
}

// genDecl renders one declaration as JSON source for the fake generating
// actor's output.
func genDecl(t *testing.T, name, trigger, action string, with map[string]any, start, landing string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"name":         name,
		"trigger":      map[string]any{"kind": trigger},
		"action":       map[string]any{"kind": action, "with": with},
		"start_node":   map[string]any{"name": start, "deadline": "none"},
		"landing_node": map[string]any{"name": landing, "deadline": "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// completeGeneration drives the generation run's `generate` node through
// the real claiming path with the given actor output, then records the
// human confirmation when approve is set.
func completeGeneration(t *testing.T, fx *declHumanFixture, runID string, output any, approve bool) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	// Each fixture owns a fresh namespace holding exactly this one run, so
	// the first claimable item in it is the generate node.
	var claimedID string
	var fencing int64
	var attempt int
	for i := 0; i < 80 && claimedID == ""; i++ {
		claimed, err := fx.store.ClaimWork(ctx, fx.nsID, "gen-worker", 2*time.Minute, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range claimed {
			if claimedID == "" {
				claimedID, fencing, attempt = c.ID, c.FencingToken, int(c.Attempt)
			}
		}
		if claimedID == "" {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if claimedID == "" {
		t.Fatalf("generate node of run %s never became claimable", runID)
	}
	if _, err := fx.srv.Engine.CompleteAttempt(ctx, engine.CompletionRequest{
		WorkID: claimedID, WorkerID: "gen-worker", FencingToken: fencing, Attempt: attempt,
		TechStatus: engine.StatusSucceeded, Outcome: "generated", Output: raw,
	}); err != nil {
		t.Fatalf("complete generate: %v", err)
	}
	if !approve {
		return
	}
	var tasks struct {
		Items []struct {
			ID    string `json:"id"`
			RunID string `json:"run_id"`
		} `json:"items"`
	}
	rr := doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/human-tasks?status=pending", "", nil, &tasks)
	if rr.Code != http.StatusOK {
		t.Fatalf("list human tasks: %d %s", rr.Code, rr.Body.String())
	}
	taskID := ""
	for _, task := range tasks.Items {
		if task.RunID == runID {
			taskID = task.ID
		}
	}
	if taskID == "" {
		t.Fatalf("no pending confirm task for generation run %s", runID)
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/human-tasks/"+taskID+"/decision", fx.token,
		map[string]any{"outcome": "approved", "decider_actor_id": fx.actorID, "expected_ledger_version": 0}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm generation: %d %s", rr.Code, rr.Body.String())
	}
}

type declHumanFixture struct {
	srv     *api.Server
	store   *storepg.Store
	nsID    string
	token   string
	actorID string
}

func newDeclHumanFixture(t *testing.T) *declHumanFixture {
	t.Helper()
	srv, nsID, token, actorID := newDeclarationHumanFixture(t)
	return &declHumanFixture{srv: srv, store: requireStore(t), nsID: nsID, token: token, actorID: actorID}
}

func hasSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// TestDeclarationGenerationValidatesConfirmsAndPublishes is the generation
// lane's h59 acceptance: an actor drafts a declaration set, the lane
// validates it with the real declaration parser (surfacing reference and
// sensitivity warnings), a human confirms, and the confirmed set publishes
// through declengine.Publish with its must link -- authored by the
// authenticated principal, inactive.
func TestDeclarationGenerationValidatesConfirmsAndPublishes(t *testing.T) {
	fx := newDeclHumanFixture(t)

	var created generationWire
	rr := doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations", fx.token, map[string]any{
		"description": "when a human answers, announce it", "actor_ref": "actor://company/planner", "output": "declarations",
	}, &created)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create generation: %d %s", rr.Code, rr.Body.String())
	}
	if created.Output != "declarations" || created.Status != "proposed" {
		t.Fatalf("created = %+v", created)
	}

	// Publishing before a human confirmed the proposal is refused.
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations/"+created.RunID+"/publish", fx.token, map[string]any{}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("publish before confirm: status %d, want 409: %s", rr.Code, rr.Body.String())
	}

	intake := genDecl(t, "gen-intake", "human.decision", "agent.work",
		map[string]any{"uses": "actor://company/developer", "input": map[string]any{}}, "gen-ready", "gen-answered")
	announce := genDecl(t, "gen-announce", "github.pr.approved", "discord.post",
		map[string]any{"uses": "actor://company/discord", "input": map[string]any{"text": "{gen-intake:answer} {gen-missing:x}"}}, "gen-answered", "gen-posted")
	completeGeneration(t, fx, created.RunID, map[string]any{
		"format":       "json",
		"declarations": []map[string]any{{"source": intake}, {"source": announce}},
		"links":        []map[string]any{{"from": "gen-announce", "to": "gen-intake", "kind": "must"}},
	}, true)

	var got generationWire
	rr = doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/workflow-generations/"+created.RunID, "", nil, &got)
	if rr.Code != http.StatusOK {
		t.Fatalf("get generation: %d %s", rr.Code, rr.Body.String())
	}
	if got.Status != "confirmed" || got.Output != "declarations" || !got.Valid || got.Declarations == nil || got.Source != "" {
		t.Fatalf("generation = %+v", got)
	}
	set := got.Declarations
	if len(set.Declarations) != 2 || !set.Valid || set.Published {
		t.Fatalf("set = %+v", set)
	}
	for _, e := range set.Declarations {
		if !e.Valid || e.Digest == "" {
			t.Fatalf("entry %+v did not validate", e)
		}
	}
	// Validation-time warnings are computed with the proposed links: the
	// must-linked reference is quiet, the unlinked one warns, and the
	// human->discord widening (t30) warns -- all surfaced, none refusing.
	if hasSubstring(set.Warnings, "reference {gen-intake:answer}") {
		t.Fatalf("a reference across a proposed must link warned: %v", set.Warnings)
	}
	if !hasSubstring(set.Warnings, "reference {gen-missing:x}") {
		t.Fatalf("unlinked reference not surfaced: %v", set.Warnings)
	}
	// gen-intake is not published yet: the widening is found by resolving
	// the set's own member, not the store.
	if !hasSubstring(set.Warnings, `step gen-intake variable "answer" comes from human`) {
		t.Fatalf("sensitivity widening not surfaced: %v", set.Warnings)
	}

	var published generationWire
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations/"+created.RunID+"/publish", fx.token,
		map[string]any{"author": "someone-forged"}, &published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish generation: %d %s", rr.Code, rr.Body.String())
	}
	pset := published.Declarations
	if pset == nil || !pset.Published || len(pset.Declarations) != 2 {
		t.Fatalf("published = %+v", published)
	}
	for _, e := range pset.Declarations {
		if e.Author != fx.actorID || e.VersionID == "" || e.Version != 1 {
			t.Fatalf("published entry %+v: author must be the authenticated principal %s", e, fx.actorID)
		}
	}
	if hasSubstring(pset.Warnings, "reference {gen-intake:answer}") || !hasSubstring(pset.Warnings, "reference {gen-missing:x}") ||
		!hasSubstring(pset.Warnings, `step gen-intake variable "answer" comes from human`) {
		t.Fatalf("publish warnings not surfaced: %v", pset.Warnings)
	}

	var shown declarationShowResp
	rr = doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/declarations/gen-announce", "", nil, &shown)
	if rr.Code != http.StatusOK {
		t.Fatalf("show: %d %s", rr.Code, rr.Body.String())
	}
	if shown.Active {
		t.Fatal("a generated declaration was activated; activation is a separate human step")
	}
	if len(shown.Links) != 1 || shown.Links[0].To != "gen-intake" || shown.Links[0].Kind != "must" {
		t.Fatalf("links = %+v, want one must link to gen-intake", shown.Links)
	}
}

// An invalid proposal (an unregistered action kind, a link naming a
// declaration outside the set) reports valid=false with diagnostics and
// refuses to publish, writing nothing -- the declaration counterpart of the
// workflow lane's compiler diagnostics.
func TestDeclarationGenerationInvalidSetIsReportedAndNotPublished(t *testing.T) {
	fx := newDeclHumanFixture(t)
	var created generationWire
	rr := doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations", fx.token, map[string]any{
		"description": "bad", "actor_ref": "actor://company/planner", "output": "declarations",
	}, &created)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	bad := genDecl(t, "gen-bad", "timer", "not.a.kind", map[string]any{}, "a", "b")
	completeGeneration(t, fx, created.RunID, map[string]any{
		"format": "json", "declarations": []map[string]any{{"source": bad}},
		"links": []map[string]any{{"from": "gen-bad", "to": "gen-elsewhere", "kind": "must"}},
	}, true)

	var got generationWire
	doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/workflow-generations/"+created.RunID, "", nil, &got)
	if got.Valid || got.Declarations == nil || got.Declarations.Valid {
		t.Fatalf("invalid set reported valid: %+v", got)
	}
	if len(got.Declarations.Declarations) != 1 || len(got.Declarations.Declarations[0].Diagnostics) == 0 {
		t.Fatalf("no per-declaration diagnostic: %+v", got.Declarations)
	}
	if !hasSubstring(got.Declarations.Diagnostics, "gen-elsewhere") {
		t.Fatalf("dangling link not diagnosed: %+v", got.Declarations.Diagnostics)
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations/"+created.RunID+"/publish", fx.token, map[string]any{}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish invalid set: %d, want 422: %s", rr.Code, rr.Body.String())
	}
	rr = doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/declarations/gen-bad", "", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("an invalid set wrote a declaration: %d", rr.Code)
	}
}

// The default output is unchanged (workflow), an unknown output is refused,
// and the declaration publish route refuses a workflow-output generation --
// workflow source still publishes through POST /v1alpha1/workflows.
func TestGenerationOutputSelectorKeepsWorkflowDefault(t *testing.T) {
	fx := newDeclHumanFixture(t)
	var created generationWire
	rr := doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations", fx.token, map[string]any{
		"description": "d", "actor_ref": "actor://company/planner",
	}, &created)
	if rr.Code != http.StatusAccepted || created.Output != "workflow" {
		t.Fatalf("default output: %d %+v", rr.Code, created)
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations/"+created.RunID+"/publish", fx.token, map[string]any{}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("publish a workflow-output generation: %d, want 409", rr.Code)
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations", fx.token, map[string]any{
		"description": "d", "actor_ref": "actor://company/planner", "output": "graph",
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown output: %d, want 400", rr.Code)
	}
}

// TestImportPlanAsDeclarationsPublishesTasksWithMustLinks is plan import's
// h59 acceptance, the declaration counterpart of
// TestImportPlan_RoundTripsRealDependencyEdgesAndPerTaskStatus over the
// same real devague fixture: every active task becomes a published
// declaration, every real dependency edge a must link, the snapshot is
// still imported, and nothing is active.
func TestImportPlanAsDeclarationsPublishesTasksWithMustLinks(t *testing.T) {
	fx := newDeclHumanFixture(t)
	var created struct {
		planImportWire
		Declarations *declSetWire `json:"declarations"`
	}
	rr := doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/plan-imports", fx.token, map[string]any{
		"plan_show":  json.RawMessage(readDevagueTestdata(t, "plan-show.json")),
		"deviations": json.RawMessage(readDevagueTestdata(t, "deviations.json")),
		"output":     "declarations",
		"actor_ref":  "actor://company/developer",
		"author":     "forged",
	}, &created)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import as declarations: %d %s", rr.Code, rr.Body.String())
	}
	if created.ID == "" || len(created.Tasks) != 5 || len(created.Deviations) != 3 {
		t.Fatalf("the snapshot was not imported alongside: %+v", created.planImportWire)
	}
	set := created.Declarations
	if set == nil || !set.Published || !set.Valid || len(set.Declarations) != 4 || len(set.Links) != 3 {
		t.Fatalf("declarations = %+v", set)
	}
	for _, e := range set.Declarations {
		if e.Author != fx.actorID || e.VersionID == "" {
			t.Fatalf("entry %+v: author must be the authenticated principal", e)
		}
	}
	if set.Warnings == nil {
		t.Fatal("warnings must be an array (surfaced, even when empty)")
	}

	var shown declarationShowResp
	rr = doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/declarations/plan-t22fixture-t4", "", nil, &shown)
	if rr.Code != http.StatusOK {
		t.Fatalf("show t4: %d %s", rr.Code, rr.Body.String())
	}
	if shown.Active {
		t.Fatal("plan import activated a declaration")
	}
	to := map[string]string{}
	for _, l := range shown.Links {
		to[l.To] = l.Kind
	}
	if to["plan-t22fixture-t1"] != "must" || to["plan-t22fixture-t2"] != "must" || len(to) != 2 {
		t.Fatalf("t4 links = %+v, want must links to t1 and t2", shown.Links)
	}

	// Re-importing the same plan is idempotent on the declarations: the
	// same digests republish as the same versions.
	var again struct {
		Declarations *declSetWire `json:"declarations"`
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/plan-imports", fx.token, map[string]any{
		"plan_show": json.RawMessage(readDevagueTestdata(t, "plan-show.json")), "output": "declarations", "actor_ref": "actor://company/developer",
	}, &again)
	if rr.Code != http.StatusCreated {
		t.Fatalf("re-import: %d %s", rr.Code, rr.Body.String())
	}
	for _, e := range again.Declarations.Declarations {
		if e.Version != 1 {
			t.Fatalf("re-import minted a new version for unchanged %s: %+v", e.Name, e)
		}
	}
}

// Declaration output needs an actor and an authenticated principal (the
// declaration routes' closed-by-default posture), and an unknown output is
// refused -- each before anything is written.
func TestImportPlanDeclarationOutputRefusals(t *testing.T) {
	fx := newDeclHumanFixture(t)
	plan := json.RawMessage(readDevagueTestdata(t, "plan-show.json"))
	rr := doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/plan-imports", fx.token, map[string]any{
		"plan_show": plan, "output": "declarations",
	}, nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "actor_ref") {
		t.Fatalf("missing actor_ref: %d %s", rr.Code, rr.Body.String())
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/plan-imports", fx.token, map[string]any{
		"plan_show": plan, "output": "workflow",
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown output: %d", rr.Code)
	}

	// The LAN listener with no principal: the snapshot lane still works
	// (existing behaviour), the declaration lane is refused outright.
	f := newFixture(t)
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), map[string]any{
		"plan_show": plan, "output": "declarations", "actor_ref": "actor://company/developer",
	}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	var list struct {
		Items []any `json:"items"`
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/plan-imports?slug=t22fixture"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 0 {
		t.Fatalf("a refused declaration import still wrote a snapshot: %+v", list.Items)
	}
}

// TestDeclarationSetRefusesANestedActivationBeforeWritingAnything pins the
// cortex review's finding F1 on t35: validation used to run only the parser,
// so a set whose activation rule targets another activation rule IN THE SET
// passed validation, and publish wrote the earlier members before
// declengine.Publish refused that one. The set's own members now answer the
// one-level-deep check, so the whole set is refused and nothing is written.
func TestDeclarationSetRefusesANestedActivationBeforeWritingAnything(t *testing.T) {
	fx := newDeclHumanFixture(t)
	var created generationWire
	rr := doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations", fx.token, map[string]any{
		"description": "nested activation", "actor_ref": "actor://company/planner", "output": "declarations",
	}, &created)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	completeGeneration(t, fx, created.RunID, map[string]any{
		"format": "json",
		"declarations": []map[string]any{
			{"source": ordinaryDeclSource("set-first", "")},
			{"source": activationDeclSource("set-rule-inner", "set-first")},
			{"source": activationDeclSource("set-rule-outer", "set-rule-inner")},
		},
	}, true)

	var got generationWire
	doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/workflow-generations/"+created.RunID, "", nil, &got)
	if got.Declarations == nil || got.Declarations.Valid || len(got.Declarations.Declarations) != 3 {
		t.Fatalf("a set with a nested activation reported valid: %+v", got.Declarations)
	}
	if outer := got.Declarations.Declarations[2]; outer.Valid || len(outer.Diagnostics) == 0 {
		t.Fatalf("the outer activation rule carries no diagnostic: %+v", outer)
	}
	rr = doAccess(t, fx.srv, http.MethodPost, "/v1alpha1/workflow-generations/"+created.RunID+"/publish", fx.token, map[string]any{}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish: %d, want 422: %s", rr.Code, rr.Body.String())
	}
	for _, name := range []string{"set-first", "set-rule-inner", "set-rule-outer"} {
		if rr := doAccess(t, fx.srv, http.MethodGet, "/v1alpha1/declarations/"+name, "", nil, nil); rr.Code != http.StatusNotFound {
			t.Fatalf("the refused set wrote %s: %d", name, rr.Code)
		}
	}
}
