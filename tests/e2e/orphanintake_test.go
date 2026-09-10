package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/ledger"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The orphan-ticket intake (plan loop-closure-claude-codex, task t4; spec
// decisions c22/c41/c42, claims c2/c9) driven end to end through the REAL
// examples/pr-upkeep/workflow.yaml: the real API's event endpoint, the real
// trigger, the real worker evaluating the `route` decision node, and a fake
// jira bridge plus a fake developer bridge speaking the actor wire contract.
//
// What it proves, in order:
//
//  1. A pr-upkeep.pr fact whose work_item is the transient gh: form mints a
//     run that routes to `intake-orphan`, which invokes the jira actor's
//     create_issue verb EXACTLY once, with the four orphan labels, and the
//     run then continues down the normal fix path and completes.
//  2. The mapping gh -> Jira key is in the run's ledger as the jira actor's
//     proposed claim, and the run's own work_item column is re-keyable
//     through the narrow PATCH allowance (gh: -> Jira key, once), after
//     which GET /v1alpha1/runs?work_item=SCRUM-7 finds the run.
//  3. A REPLAYED fact (same source key, same watermark) creates no run and
//     therefore no ticket.
//  4. A later fact for the same PR that already carries the Jira key (the
//     sweep found it on the stamped PR body) routes `keyed`, mints a run
//     keyed SCRUM-7 from the payload, and creates nothing.

const (
	orphanWorkflowPath = "../../examples/pr-upkeep/workflow.yaml"
	orphanEventSecret  = "e2e-only-event-token-secret-0123456789abcdef"
	orphanJiraKey      = "SCRUM-7"
	orphanGhWorkItem   = "gh:agentculture/culture-nodes#307"
)

// orphanActorKeys are the registry keys examples/pr-upkeep/workflow.yaml's
// `uses:` references resolve to (internal/worker/registry.go strips the
// digest). All three point at the same fake server; the script switches on
// the node id, exactly as deliveryAgents does.
var orphanActorKeys = []string{"company/jira-comment", "company/developer", "company/security-developer"}

var wantOrphanLabels = []string{"auto-created", "orphan", "repo:agentculture/culture-nodes", "source:github"}

type orphanActors struct {
	server *httptest.Server
	mu     sync.Mutex
	// actorIDs maps registry key -> actors.id, for stamping ledger origins.
	actorIDs    map[string]string
	invocations []actors.InvocationRequest
	failures    []string
}

func newOrphanActors(t *testing.T) *orphanActors {
	t.Helper()
	a := &orphanActors{actorIDs: map[string]string{}}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.server.Close)
	return a
}

func (a *orphanActors) handle(w http.ResponseWriter, r *http.Request) {
	var req actors.InvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad invocation", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.invocations = append(a.invocations, req)
	a.mu.Unlock()

	result, failure := a.script(req)
	if failure != "" {
		a.mu.Lock()
		a.failures = append(a.failures, failure)
		a.mu.Unlock()
		http.Error(w, failure, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (a *orphanActors) script(req actors.InvocationRequest) (actors.InvocationResult, string) {
	claim := func(actorKey string, data any) *actors.LedgerDelta {
		payload, _ := json.Marshal(data)
		return &actors.LedgerDelta{Records: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: a.actorIDs[actorKey]},
			Authority:  ledger.AuthorityProposed,
			Data:       payload,
		}}}
	}
	switch req.Node.ID {
	case "intake-orphan":
		// The fake jira bridge is as strict as the real one about its input:
		// adapters/jira create_issue.parse admits exactly
		// {verb, project, summary} plus optional {description, issue_type, labels}.
		var in map[string]json.RawMessage
		if err := json.Unmarshal(req.Input, &in); err != nil {
			return actors.InvocationResult{}, "intake-orphan: input is not an object"
		}
		for _, key := range []string{"verb", "project", "summary"} {
			if _, ok := in[key]; !ok {
				return actors.InvocationResult{}, "intake-orphan: input lacks required key " + key
			}
		}
		for key := range in {
			switch key {
			case "verb", "project", "summary", "description", "issue_type", "labels":
			default:
				return actors.InvocationResult{}, "intake-orphan: input carries a key the bridge refuses: " + key
			}
		}
		var verb string
		_ = json.Unmarshal(in["verb"], &verb)
		if verb != "create_issue" {
			return actors.InvocationResult{}, "intake-orphan: verb is " + verb
		}
		// Same result shape as adapters/jira create_issue.result.
		return actors.InvocationResult{
			Outcome: "issue_created",
			Output:  json.RawMessage(`{"issue":"` + orphanJiraKey + `","id":"10007"}`),
			LedgerDelta: claim("company/jira-comment", map[string]any{
				"verb": "create_issue", "issue": orphanJiraKey, "id": "10007",
			}),
		}, ""
	case "stamp-pr":
		return actors.InvocationResult{
			Outcome:     "stamped",
			Output:      json.RawMessage(`{"summary":"wrote ` + orphanJiraKey + ` into the PR body"}`),
			LedgerDelta: claim("company/developer", map[string]any{"statement": "PR body names " + orphanJiraKey}),
		}, ""
	// The stage write-back nodes (t17) post one structured comment per
	// transition through the jira actor; this fake answers them so the keyed
	// path reaches fix. The comment text is the graph's, not this test's concern.
	case "stage-dispatch", "stage-pr-open":
		return actors.InvocationResult{
			Outcome:     "comment_posted",
			Output:      json.RawMessage(`{"issue":"` + orphanJiraKey + `","comment_id":"20001"}`),
			LedgerDelta: claim("company/jira-comment", map[string]any{"verb": "post_comment", "issue": orphanJiraKey}),
		}, ""
	case "fix":
		return actors.InvocationResult{
			Outcome:     "no_change",
			Output:      json.RawMessage(`{}`),
			LedgerDelta: claim("company/developer", map[string]any{"statement": "finding is a false positive"}),
		}, ""
	}
	return actors.InvocationResult{}, "unexpected node dispatched to the fake bridges: " + req.Node.ID
}

func (a *orphanActors) invocationsOf(nodeID string) []actors.InvocationRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []actors.InvocationRequest
	for _, inv := range a.invocations {
		if inv.Node.ID == nodeID {
			out = append(out, inv)
		}
	}
	return out
}

func registerOrphanActors(t *testing.T, db *postgres.Store, namespaceID string, a *orphanActors) {
	t.Helper()
	for _, key := range orphanActorKeys {
		id := "actor_" + idstore.NewULID()
		if _, err := db.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
			VALUES ($1, $2, $3, 1, 'agent', 'http', $4)
		`, id, namespaceID, key, a.server.URL); err != nil {
			t.Fatalf("register actor %s: %v", key, err)
		}
		a.actorIDs[key] = id
	}
}

// publishWorkflowAt publishes any workflow file, returning its digest.
func (s *stack) publishWorkflowAt(t *testing.T, path string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var published struct {
		Digest string `json:"digest"`
	}
	status := s.postJSON("/v1alpha1/workflows", map[string]string{"format": "yaml", "source": string(source)}, &published)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("publish %s: status %d", path, status)
	}
	return published.Digest
}

// doJSON is postJSON with a method, an optional bearer token and the raw body
// returned, for the authenticated event endpoint and for PATCH.
func (s *stack) doJSON(t *testing.T, method, path, bearer string, body any, out any) (int, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, s.server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if out != nil && buf.Len() > 0 {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, buf.String(), err)
		}
	}
	return resp.StatusCode, buf.Bytes()
}

type eventDeliveryView struct {
	Triggered []struct {
		RunID    string `json:"run_id"`
		Attached bool   `json:"attached"`
	} `json:"triggered"`
	Duplicate bool `json:"duplicate"`
}

// upkeepFact is the sweep's pr-upkeep.pr payload for PR #307 carrying one
// finding (pr_upkeep_emit.upkeep_pr_fact), keyed as the caller says.
func upkeepFact(workItem, findingID string) map[string]any {
	return map[string]any{
		"source":     "github_pr",
		"repository": "agentculture/culture-nodes",
		"number":     307,
		"head_sha":   "0123456789abcdef0123456789abcdef01234567",
		"findings": []map[string]any{{
			"id": findingID, "source": "qodo", "severity": "high", "kind": "quality",
			"file": "internal/api/runs.go", "line": 1, "title": "a finding",
		}},
		"work_item": workItem,
	}
}

func (s *stack) deliverUpkeepFact(t *testing.T, sourceKey string, payload map[string]any) eventDeliveryView {
	t.Helper()
	var out eventDeliveryView
	status, body := s.doJSON(t, http.MethodPost, "/v1alpha1/events", orphanEventSecret, map[string]any{
		"name": "pr-upkeep.pr", "payload": payload, "emitter": "e2e-sweep",
		"source_key": sourceKey,
		"watermark":  map[string]any{"head_sha": payload["head_sha"], "finding": sourceKey},
	}, &out)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1alpha1/events: status %d body %s", status, body)
	}
	return out
}

func (s *stack) runWorkItem(t *testing.T, runID string) string {
	t.Helper()
	var workItem *string
	if err := s.db.Pool().QueryRow(context.Background(), `SELECT work_item FROM runs WHERE id = $1`, runID).Scan(&workItem); err != nil {
		t.Fatalf("read runs.work_item for %s: %v", runID, err)
	}
	if workItem == nil {
		return ""
	}
	return *workItem
}

func (s *stack) countRuns(t *testing.T) int {
	t.Helper()
	return countRows(t, s, `SELECT count(*) FROM runs WHERE namespace_id = $1`, s.namespaceID)
}

func nodeOutcome(view runView, nodeID string) string {
	for _, nr := range view.NodeRuns {
		if nr.NodeID == nodeID {
			return nr.Outcome
		}
	}
	return "<not visited>"
}

func TestOrphanIntakeCreatesOneTicketAndRekeysTheRun(t *testing.T) {
	db := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, db, "e2e-orphan-intake")

	fakes := newOrphanActors(t)
	registerOrphanActors(t, db, ns.ID, fakes)
	s := startStack(t, stackConfig{
		namespaceID: ns.ID, agentsURL: fakes.server.URL,
		runner: &scriptedRunner{}, runnerName: "headspace/docker", runnerActorID: "actor_unused",
		eventTokenSecret: orphanEventSecret,
	})
	defer s.stop()
	digest := s.publishWorkflowAt(t, orphanWorkflowPath)

	// ---- 1. A gh:-keyed fact creates exactly one ticket, then fixes ----
	delivery := s.deliverUpkeepFact(t, "github:agentculture/culture-nodes:pr:307:qodo-1", upkeepFact(orphanGhWorkItem, "pr307-qodo-1"))
	if len(delivery.Triggered) != 1 || delivery.Triggered[0].Attached {
		t.Fatalf("first fact: want exactly one NEW triggered run, got %+v", delivery)
	}
	runID := delivery.Triggered[0].RunID
	if got := s.runWorkItem(t, runID); got != orphanGhWorkItem {
		t.Fatalf("minted run work_item = %q, want the payload's %q", got, orphanGhWorkItem)
	}

	view := s.waitForTerminal(t, runID, 60*time.Second)
	if failures := fakes.failures; len(failures) > 0 {
		t.Fatalf("the fake bridges refused an invocation: %v", failures)
	}
	if view.Run.State != "completed" || view.Run.WorkflowDigest != digest {
		t.Fatalf("run state=%q digest=%q, want completed on %s", view.Run.State, view.Run.WorkflowDigest, digest)
	}
	for node, want := range map[string]string{"route": "orphan", "intake-orphan": "issue_created", "stamp-pr": "stamped", "fix": "no_change"} {
		if got := nodeOutcome(view, node); got != want {
			t.Errorf("node %s outcome = %q, want %q", node, got, want)
		}
	}

	creates := fakes.invocationsOf("intake-orphan")
	if len(creates) != 1 {
		t.Fatalf("create_issue was invoked %d times, want exactly 1", len(creates))
	}
	var createInput struct {
		Verb    string   `json:"verb"`
		Project string   `json:"project"`
		Summary string   `json:"summary"`
		Labels  []string `json:"labels"`
	}
	if err := json.Unmarshal(creates[0].Input, &createInput); err != nil {
		t.Fatalf("decode create_issue input %s: %v", creates[0].Input, err)
	}
	sort.Strings(createInput.Labels)
	if got, want := createInput.Labels, wantOrphanLabels; !equalStrings(got, want) {
		t.Fatalf("create_issue labels = %v, want %v", got, want)
	}
	if createInput.Verb != "create_issue" || createInput.Project == "" || createInput.Summary != orphanGhWorkItem {
		t.Fatalf("create_issue input = %+v: want verb create_issue, a project, and the summary naming the PR (%s)", createInput, orphanGhWorkItem)
	}

	// ---- 2. The mapping is in the ledger, and the run is re-keyable once ----
	assertOrphanMappingInLedger(t, db, ns.ID, runID, fakes.actorIDs["company/jira-comment"])

	var patched struct {
		WorkItem string `json:"work_item"`
	}
	status, body := s.doJSON(t, http.MethodPatch, "/v1alpha1/runs/"+runID, "", map[string]any{"work_item": orphanJiraKey}, &patched)
	if status != http.StatusOK || patched.WorkItem != orphanJiraKey {
		t.Fatalf("PATCH work_item gh->%s: status %d body %s", orphanJiraKey, status, body)
	}
	if got := s.runWorkItem(t, runID); got != orphanJiraKey {
		t.Fatalf("after PATCH, runs.work_item = %q, want %q", got, orphanJiraKey)
	}
	var listed struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if status := s.getJSON("/v1alpha1/runs?work_item="+orphanJiraKey, &listed); status != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].ID != runID {
		t.Fatalf("GET /runs?work_item=%s: status %d items %+v, want exactly run %s", orphanJiraKey, status, listed.Items, runID)
	}
	status, body = s.doJSON(t, http.MethodPatch, "/v1alpha1/runs/"+runID, "", map[string]any{"work_item": "SCRUM-8"}, nil)
	if status != http.StatusConflict {
		t.Fatalf("second re-key of an already keyed run: status %d body %s, want 409", status, body)
	}

	// ---- 3. A replayed fact creates nothing ----
	runsBefore := s.countRuns(t)
	replay := s.deliverUpkeepFact(t, "github:agentculture/culture-nodes:pr:307:qodo-1", upkeepFact(orphanGhWorkItem, "pr307-qodo-1"))
	if len(replay.Triggered) != 0 || !replay.Duplicate {
		t.Fatalf("replayed fact: want a duplicate with nothing triggered, got %+v", replay)
	}
	if got := s.countRuns(t); got != runsBefore {
		t.Fatalf("replayed fact minted a run: %d -> %d", runsBefore, got)
	}
	if n := len(fakes.invocationsOf("intake-orphan")); n != 1 {
		t.Fatalf("replayed fact created a ticket: create_issue invoked %d times", n)
	}

	// ---- 4. The next tick's fact already carries the key: keyed path ----
	keyed := s.deliverUpkeepFact(t, "github:agentculture/culture-nodes:pr:307:qodo-2", upkeepFact(orphanJiraKey, "pr307-qodo-2"))
	if len(keyed.Triggered) != 1 || keyed.Triggered[0].Attached {
		t.Fatalf("keyed fact: want exactly one NEW triggered run, got %+v", keyed)
	}
	keyedRun := keyed.Triggered[0].RunID
	keyedView := s.waitForTerminal(t, keyedRun, 60*time.Second)
	if keyedView.Run.State != "completed" {
		t.Fatalf("keyed run state = %q, want completed", keyedView.Run.State)
	}
	if got := nodeOutcome(keyedView, "route"); got != "keyed" {
		t.Fatalf("keyed run route outcome = %q, want keyed", got)
	}
	if got := nodeOutcome(keyedView, "intake-orphan"); got != "<not visited>" {
		t.Fatalf("keyed run visited intake-orphan (outcome %q); a keyed item must never create a ticket", got)
	}
	if got := s.runWorkItem(t, keyedRun); got != orphanJiraKey {
		t.Fatalf("keyed run work_item = %q, want %q from the payload", got, orphanJiraKey)
	}
	if n := len(fakes.invocationsOf("intake-orphan")); n != 1 {
		t.Fatalf("across both facts create_issue was invoked %d times, want exactly 1", n)
	}
	if errs := s.errors(); len(errs) > 0 {
		t.Fatalf("stack errors: %v", errs)
	}
}

// assertOrphanMappingInLedger: the jira actor's completion proposes a claim
// {verb: create_issue, issue: SCRUM-7}; with the run's input work_item that
// IS the gh -> Jira mapping, attributed to the actor that made it, and it
// stays `proposed` — an agent saying it created a ticket is a completion
// claim, not verified evidence (PRD §10.4).
func assertOrphanMappingInLedger(t *testing.T, db *postgres.Store, namespaceID, runID, jiraActorID string) {
	t.Helper()
	records, err := ledgerFor(t, db, namespaceID).Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("ledger records: %v", err)
	}
	for _, rec := range records {
		if rec.RecordType != ledger.RecordClaim || rec.Origin.ActorID != jiraActorID {
			continue
		}
		var data struct {
			Verb  string `json:"verb"`
			Issue string `json:"issue"`
		}
		_ = json.Unmarshal(rec.Data, &data)
		if data.Verb == "create_issue" && data.Issue == orphanJiraKey {
			if rec.Authority != ledger.AuthorityProposed {
				t.Fatalf("create_issue claim authority = %q, want proposed", rec.Authority)
			}
			return
		}
	}
	t.Fatalf("no proposed create_issue claim for %s by actor %s in the run's ledger (%d records)", orphanJiraKey, jiraActorID, len(records))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
