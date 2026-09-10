package e2etest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/ledger"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The Jira stage write-back (plan loop-closure-claude-codex, task t17; spec
// c9/c16, honesty h18) driven end to end through the REAL example workflows:
// the real API's event endpoint, the real triggers, the real worker evaluating
// the `route` decision nodes and the edge guards, a scripted runner for the
// cleanup code node, and a fake jira bridge plus fake claude/developer bridges
// speaking the actor wire contract.
//
// What it proves, in order:
//
//  1. One ticket driven intake -> dispatch -> pr-open -> merged -> cleanup
//     collects EXACTLY ONE comment per stage, each posted by the jira actor's
//     post_comment verb from a graph node, with the machine-readable first
//     line the sweep parses (`culture-nodes:stage=<stage>`).
//  2. Nothing but a graph node posts one: the sweep is not in this test at
//     all, and the comments arrive with the nodes that produced them.
//  3. Each stage comment is in its run's ledger as the jira actor's PROPOSED
//     claim -- an agent saying it commented is a completion claim, not
//     verified evidence (PRD 10.4).
//  4. A REPLAYED tick -- every one of the three facts delivered again with the
//     same source key and watermark -- creates zero runs and zero comments.
//  5. A `gh:`-keyed work item posts no stage at all: its edge guards divert
//     past both pr-upkeep stage nodes, because an orphan has no ticket to
//     comment on until the run it is in creates one.
//  6. The merge approval is not created until the `readiness` collector
//     completed (task t18, honesty h19): the approval is reachable from that
//     node and from nowhere else, so a PENDING human task is itself the
//     proof, and the task's `context_refs` carries the collector's output
//     pointer for the surface presenting it to resolve.

const (
	stageJiraKey     = "SCRUM-9"
	stageRepository  = "agentculture/culture-nodes"
	stageMergedAt    = "2026-09-04T11:00:00Z"
	jiraIntakePath   = "../../examples/jira-intake/workflow.yaml"
	cleanupGraphPath = "../../examples/cleanup/workflow.yaml"
)

// stageActorKeys are every registry key the three graphs' `uses:` references
// resolve to (the digest suffix is stripped by internal/worker/registry.go).
// All of them point at the one fake server; the script switches on the node
// id, exactly as deliveryAgents does.
var stageActorKeys = []string{
	"company/jira-comment", "company/intake",
	"company/developer", "company/security-developer",
}

// wantStageOrder is the stage sequence a ticket driven end to end shows. It is
// the enum in order, minus `spec`: that stage's writer is the spec-chain lane,
// which task t17 did not touch, so no graph emits it and this test asserts its
// ABSENCE rather than pretending otherwise.
var wantStageOrder = []string{"intake", "dispatch", "pr-open", "merged", "cleanup"}

// stageComment is one comment the fake jira bridge received.
type stageComment struct {
	NodeID string
	Issue  string
	Stage  string
	Body   string
}

type stageActors struct {
	server *httptest.Server
	mu     sync.Mutex
	// actorIDs maps registry key -> actors.id, for stamping ledger origins.
	actorIDs    map[string]string
	invocations []actors.InvocationRequest
	comments    []stageComment
	failures    []string
}

func newStageActors(t *testing.T) *stageActors {
	t.Helper()
	a := &stageActors{actorIDs: map[string]string{}}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.server.Close)
	return a
}

func (a *stageActors) handle(w http.ResponseWriter, r *http.Request) {
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

// stagePostComment is as strict as the real bridge about its input:
// adapters/jira post_comment.parse admits exactly {verb, issue, comment} plus
// an optional question_id, and refuses any other key.
func (a *stageActors) stagePostComment(req actors.InvocationRequest) (issue, comment, failure string) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(req.Input, &in); err != nil {
		return "", "", req.Node.ID + ": input is not an object"
	}
	for _, key := range []string{"verb", "issue", "comment"} {
		if _, ok := in[key]; !ok {
			return "", "", req.Node.ID + ": input lacks required key " + key
		}
	}
	for key := range in {
		switch key {
		case "verb", "issue", "comment", "question_id":
		default:
			return "", "", req.Node.ID + ": input carries a key the bridge refuses: " + key
		}
	}
	var verb string
	_ = json.Unmarshal(in["verb"], &verb)
	if verb != "post_comment" {
		return "", "", req.Node.ID + ": verb is " + verb
	}
	_ = json.Unmarshal(in["issue"], &issue)
	_ = json.Unmarshal(in["comment"], &comment)
	if issue == "" || comment == "" {
		return "", "", req.Node.ID + ": issue or comment is empty"
	}
	// The marker is the BRIDGE's to append; a graph that wrote one would be
	// forging the identity the sweep's self-echo filter trusts.
	if strings.Contains(comment, "culture-nodes:jira-actor") {
		return "", "", req.Node.ID + ": the graph wrote the actor's own marker"
	}
	return issue, comment, ""
}

func (a *stageActors) script(req actors.InvocationRequest) (actors.InvocationResult, string) {
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
	// ---- the stage write-back: one node per stage, three graphs ----------
	case "stage-intake", "stage-dispatch", "stage-pr-open",
		"stage-merged", "stage-cleanup", "stage-cleanup-declined":
		issue, comment, failure := a.stagePostComment(req)
		if failure != "" {
			return actors.InvocationResult{}, failure
		}
		stage := strings.TrimPrefix(strings.SplitN(comment, "\n", 2)[0], "culture-nodes:stage=")
		if stage == comment || stage == "" {
			return actors.InvocationResult{}, req.Node.ID +
				": the comment's first line is not culture-nodes:stage=<stage>: " + comment
		}
		a.mu.Lock()
		id := len(a.comments) + 20000
		a.comments = append(a.comments, stageComment{
			NodeID: req.Node.ID, Issue: issue, Stage: stage, Body: comment,
		})
		a.mu.Unlock()
		return actors.InvocationResult{
			Outcome: "comment_posted",
			Output:  json.RawMessage(`{"issue":"` + issue + `","comment_id":"` + strconv.Itoa(id) + `"}`),
			LedgerDelta: claim("company/jira-comment", map[string]any{
				"verb": "post_comment", "issue": issue, "stage": stage,
			}),
		}, ""

	// ---- jira-intake -----------------------------------------------------
	case "intake":
		return actors.InvocationResult{
			Outcome:     "intake_drafted",
			Output:      json.RawMessage(`{"summary":"Picked this up.\n- culture-nodes (Claude)"}`),
			LedgerDelta: claim("company/intake", map[string]any{"statement": "intake drafted"}),
		}, ""
	case "post-comment":
		issue, _, failure := a.stagePostComment(req)
		if failure != "" {
			return actors.InvocationResult{}, failure
		}
		return actors.InvocationResult{
			Outcome: "comment_posted",
			Output:  json.RawMessage(`{"issue":"` + issue + `","comment_id":"10001"}`),
			LedgerDelta: claim("company/jira-comment",
				map[string]any{"verb": "post_comment", "issue": issue}),
		}, ""
	case "transition":
		return actors.InvocationResult{
			Outcome: "issue_transitioned",
			Output:  json.RawMessage(`{"issue":"` + stageJiraKey + `"}`),
			LedgerDelta: claim("company/jira-comment",
				map[string]any{"verb": "transition_issue", "issue": stageJiraKey}),
		}, ""

	// ---- pr-upkeep -------------------------------------------------------
	case "analyse":
		output, failure := analysePackagedResult(req.Input)
		if failure != "" {
			return actors.InvocationResult{}, failure
		}
		return actors.InvocationResult{
			Outcome:     "packaged",
			Output:      output,
			LedgerDelta: claim("company/developer", map[string]any{"statement": "findings judged"}),
		}, ""
	case "fix":
		return actors.InvocationResult{
			Outcome:     "completed",
			Output:      json.RawMessage(`{"summary":"opened a pull request for the finding"}`),
			LedgerDelta: claim("company/developer", map[string]any{"statement": "fix pushed"}),
		}, ""
	case "intake-orphan":
		return actors.InvocationResult{
			Outcome: "issue_created",
			Output:  json.RawMessage(`{"issue":"SCRUM-77","id":"10077"}`),
			LedgerDelta: claim("company/jira-comment",
				map[string]any{"verb": "create_issue", "issue": "SCRUM-77"}),
		}, ""
	case "stamp-pr":
		return actors.InvocationResult{
			Outcome:     "stamped",
			Output:      json.RawMessage(`{"summary":"wrote SCRUM-77 into the PR body"}`),
			LedgerDelta: claim("company/developer", map[string]any{"statement": "PR body keyed"}),
		}, ""
	}
	return actors.InvocationResult{}, "unexpected node dispatched to the fake bridges: " + req.Node.ID
}

func (a *stageActors) stagesFor(issue string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, comment := range a.comments {
		if comment.Issue == issue {
			out = append(out, comment.Stage)
		}
	}
	return out
}

func (a *stageActors) commentCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.comments)
}

func (a *stageActors) refusals() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.failures...)
}

func registerStageActors(t *testing.T, db *postgres.Store, namespaceID string, a *stageActors) string {
	t.Helper()
	for _, key := range stageActorKeys {
		id := "actor_" + idstore.NewULID()
		if _, err := db.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
			VALUES ($1, $2, $3, 1, 'agent', 'http', $4)
		`, id, namespaceID, key, a.server.URL); err != nil {
			t.Fatalf("register actor %s: %v", key, err)
		}
		a.actorIDs[key] = id
	}
	// The cleanup graph's code node and pr-upkeep's `readiness` collector both
	// run through the runner boundary, and their observed evidence needs a
	// registered producer identity to be attributed to
	// (worker.Options.CodeRunnerActorID). A stack carries exactly one, so both
	// nodes are attributed to this row here -- a harness limitation, not a
	// property of the graphs, which name different runner registry ids.
	runnerID := "actor_" + idstore.NewULID()
	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'runner', 'internal')
	`, runnerID, namespaceID, "headspace/cleanup"); err != nil {
		t.Fatalf("register cleanup runner actor: %v", err)
	}
	return runnerID
}

// deliverFact posts one fact to the authenticated event endpoint, the way the
// sweep does, and returns what the control plane says it did with it.
func (s *stack) deliverFact(t *testing.T, name, sourceKey, subject string, watermark, payload map[string]any) eventDeliveryView {
	t.Helper()
	var out eventDeliveryView
	body := map[string]any{
		"name": name, "payload": payload, "emitter": "e2e-sweep",
		"source_key": sourceKey, "watermark": watermark,
	}
	if subject != "" {
		body["subject"] = subject
	}
	status, raw := s.doJSON(t, http.MethodPost, "/v1alpha1/events", orphanEventSecret, body, &out)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1alpha1/events %s: status %d body %s", name, status, raw)
	}
	return out
}

func stageTicketFact() map[string]any {
	return map[string]any{
		"source": "jira", "id": stageJiraKey, "project": "SCRUM",
		"title": "A finding on the pull request", "status": "To Do",
		"description": "The board reader should not have to ask where this is.",
		// The sweep emits every one of these on a Jira fact (pr_upkeep_jira.py
		// keys: source id project severity kind file line title description
		// description_truncated status details_url) and the intake node binds
		// them unconditionally, so a sweep-shaped fact must carry them all.
		"description_truncated": false, "severity": "MAJOR", "kind": "CODE_SMELL",
		"file": "examples/pr-upkeep/sweep.py", "line": 1,
		"details_url": "https://jira.example/browse/" + stageJiraKey,
	}
}

func stageMergedFact() map[string]any {
	return map[string]any{
		"source": "github_pr", "repository": stageRepository, "number": 307,
		"url": "https://github.example/owner/repo/pull/307", "head_sha": strings.Repeat("f", 40),
		"merged_at": stageMergedAt, "issue_key": stageJiraKey,
	}
}

// oneTriggeredRun insists the delivery minted exactly one NEW run.
func oneTriggeredRun(t *testing.T, what string, delivery eventDeliveryView) string {
	t.Helper()
	if len(delivery.Triggered) != 1 || delivery.Triggered[0].Attached {
		t.Fatalf("%s: want exactly one NEW triggered run, got %+v", what, delivery)
	}
	return delivery.Triggered[0].RunID
}

func TestATicketDrivenEndToEndShowsOneCommentPerStage(t *testing.T) {
	db := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, db, "e2e-stage-write-back")

	fakes := newStageActors(t)
	runnerID := registerStageActors(t, db, ns.ID, fakes)
	s := startStack(t, stackConfig{
		namespaceID: ns.ID, agentsURL: fakes.server.URL,
		runner: &scriptedRunner{}, runnerName: "headspace/cleanup", runnerActorID: runnerID,
		eventTokenSecret: orphanEventSecret,
	})
	defer s.stop()

	intakeDigest := s.publishWorkflowAt(t, jiraIntakePath)
	s.publishWorkflowAt(t, orphanWorkflowPath)
	s.publishWorkflowAt(t, cleanupGraphPath)

	// ---- 1. intake: the ticket moved to To Do and was picked up ----------
	const ticketSourceKey = "jira:team.example.com:" + stageJiraKey + ":status"
	intakeRun := oneTriggeredRun(t, "the To Do transition", s.deliverFact(t,
		"pr-upkeep.jira.transitioned.to-do", ticketSourceKey, stageJiraKey,
		map[string]any{"status": "To Do"}, stageTicketFact()))

	intakeView := s.waitForTerminal(t, intakeRun, 60*time.Second)
	if failures := fakes.refusals(); len(failures) > 0 {
		t.Fatalf("the fake bridges refused an invocation: %v", failures)
	}
	if intakeView.Run.State != "completed" || intakeView.Run.WorkflowDigest != intakeDigest {
		t.Fatalf("intake run state=%q digest=%q, want completed on %s",
			intakeView.Run.State, intakeView.Run.WorkflowDigest, intakeDigest)
	}
	if got := nodeOutcome(intakeView, "stage-intake"); got != "comment_posted" {
		t.Fatalf("stage-intake outcome = %q, want comment_posted", got)
	}
	if got, want := fakes.stagesFor(stageJiraKey), []string{"intake"}; !equalStrings(got, want) {
		t.Fatalf("after intake the ticket shows stages %v, want %v", got, want)
	}
	// The stage record is the actor's proposed claim, not evidence.
	assertStageClaimInLedger(t, db, ns.ID, intakeRun, fakes.actorIDs["company/jira-comment"], "intake")

	// ---- 2. dispatch + pr-open: one finding worked on the ticket's PR ----
	upkeepRun := oneTriggeredRun(t, "the keyed finding fact", s.deliverUpkeepFact(t,
		"github:"+stageRepository+":pr:307:qodo-1", upkeepFact(stageJiraKey, "pr307-qodo-1")))

	// The run parks on the merge approval, which is where `pr-open` means
	// something: a decision is pending. That is the state to assert from.
	//
	// Reaching that state at all is task t18's acceptance criterion in its
	// strongest form: the approval is reachable only from `readiness`, so a
	// pending human task is proof the collector completed first. The scripted
	// runner answers the code node the way the real runner boundary does
	// (exit 0 -> the `passed` port).
	task := s.awaitPendingHumanTask(t, upkeepRun, 60*time.Second)
	if failures := fakes.refusals(); len(failures) > 0 {
		t.Fatalf("the fake bridges refused an invocation: %v", failures)
	}
	upkeepView := s.runView(t, upkeepRun)
	for node, want := range map[string]string{
		"route": "keyed", "analyse": "packaged", "stage-dispatch": "comment_posted",
		"fix": "completed", "stage-pr-open": "comment_posted", "readiness": "passed",
	} {
		if got := nodeOutcome(upkeepView, node); got != want {
			t.Errorf("pr-upkeep node %s outcome = %q, want %q", node, got, want)
		}
	}
	assertReadinessContextRef(t, task)
	if got := nodeOutcome(upkeepView, "intake-orphan"); got != "<not visited>" {
		t.Errorf("a keyed run visited intake-orphan (outcome %q)", got)
	}
	if got, want := fakes.stagesFor(stageJiraKey), []string{"intake", "dispatch", "pr-open"}; !equalStrings(got, want) {
		t.Fatalf("after the fix the ticket shows stages %v, want %v", got, want)
	}

	// ---- 3. merged + cleanup: the PR landed and the loose ends closed ----
	//
	// The fact is delivered WITHOUT a subject, and that is a deliberate
	// isolation this test has to state rather than hide.
	//
	// The sweep's real pr.merged fact carries `subject = issue_key`
	// (pr_upkeep_emit.closed_pull_event returns it, sweep.py passes it), and
	// a triggered run inherits the event's subject (internal/engine/
	// trigger.go:375). The same delivery then applies the ticket freeze
	// (internal/api/signalevents.go:210 -> freezeTicketRuns), whose walk
	// matches `runs.subject = <issue_key>` over every non-terminal run --
	// which now includes the cleanup run that delivery JUST minted, so the
	// cleanup node is parked with reason `ticket_frozen` before it can run.
	// That interaction predates this task: it is the cleanup graph's (t13)
	// against the ticket freeze, `closed_pull_event`'s own comment still says
	// these facts "mint no run", and nothing drove that graph through the
	// control plane until this test. It is reported, not fixed here -- fixing
	// it is a control-plane or fact-shape change t17 does not own.
	//
	// Delivered subject-less, the freeze's walk matches only runs whose input
	// carries `id` (the jira work-item contract), so it reaches this test's
	// already-completed intake run and nothing else, and what remains under
	// test is exactly the stage write-back.
	cleanupRun := oneTriggeredRun(t, "the pr.merged fact", s.deliverFact(t,
		"pr.merged", "github:"+stageRepository+":pr:307:merged", "",
		map[string]any{"merged_at": stageMergedAt}, stageMergedFact()))

	cleanupView := s.waitForTerminal(t, cleanupRun, 60*time.Second)
	if failures := fakes.refusals(); len(failures) > 0 {
		t.Fatalf("the fake bridges refused an invocation: %v", failures)
	}
	if cleanupView.Run.State != "completed" {
		t.Fatalf("cleanup run state = %q, want completed", cleanupView.Run.State)
	}
	for node, want := range map[string]string{
		"route": "merged", "stage-merged": "comment_posted",
		"cleanup": "passed", "stage-cleanup": "comment_posted",
	} {
		if got := nodeOutcome(cleanupView, node); got != want {
			t.Errorf("cleanup node %s outcome = %q, want %q", node, got, want)
		}
	}
	// The declined-path node belongs to pr.closed and must not have run.
	if got := nodeOutcome(cleanupView, "stage-cleanup-declined"); got != "<not visited>" {
		t.Errorf("a merged fact visited stage-cleanup-declined (outcome %q)", got)
	}

	// ---- The whole point: one comment per stage, in order ----------------
	if got := fakes.stagesFor(stageJiraKey); !equalStrings(got, wantStageOrder) {
		t.Fatalf("the driven ticket shows stages %v, want exactly %v (one per stage, in order)", got, wantStageOrder)
	}
	seen := map[string]int{}
	for _, stage := range fakes.stagesFor(stageJiraKey) {
		seen[stage]++
	}
	for stage, count := range seen {
		if count != 1 {
			t.Errorf("stage %q was posted %d times, want exactly 1", stage, count)
		}
	}
	if _, ok := seen["spec"]; ok {
		t.Error("a graph posted the `spec` stage; no graph writes it yet (its writer is the spec-chain lane)")
	}

	// ---- 4. A replayed tick creates zero runs and zero comments ----------
	runsBefore, commentsBefore := s.countRuns(t), fakes.commentCount()
	replays := []struct {
		name, sourceKey, subject string
		watermark                map[string]any
		payload                  map[string]any
	}{
		{"pr-upkeep.jira.transitioned.to-do", ticketSourceKey, stageJiraKey,
			map[string]any{"status": "To Do"}, stageTicketFact()},
		{"pr.merged", "github:" + stageRepository + ":pr:307:merged", "",
			map[string]any{"merged_at": stageMergedAt}, stageMergedFact()},
	}
	for _, replay := range replays {
		delivery := s.deliverFact(t, replay.name, replay.sourceKey, replay.subject, replay.watermark, replay.payload)
		if len(delivery.Triggered) != 0 || !delivery.Duplicate {
			t.Fatalf("replayed %s: want a duplicate with nothing triggered, got %+v", replay.name, delivery)
		}
	}
	upkeepReplay := s.deliverUpkeepFact(t,
		"github:"+stageRepository+":pr:307:qodo-1", upkeepFact(stageJiraKey, "pr307-qodo-1"))
	if len(upkeepReplay.Triggered) != 0 || !upkeepReplay.Duplicate {
		t.Fatalf("replayed pr-upkeep.pr: want a duplicate with nothing triggered, got %+v", upkeepReplay)
	}
	// Give any run the replay might have minted time to reach a bridge.
	time.Sleep(500 * time.Millisecond)
	if got := s.countRuns(t); got != runsBefore {
		t.Fatalf("the replayed tick minted runs: %d -> %d", runsBefore, got)
	}
	if got := fakes.commentCount(); got != commentsBefore {
		t.Fatalf("the replayed tick posted comments: %d -> %d", commentsBefore, got)
	}

	// ---- 5. A gh:-keyed item posts no stage ------------------------------
	orphanFact := upkeepFact("gh:"+stageRepository+"#411", "pr411-qodo-1")
	orphanFact["number"] = 411
	orphanRun := oneTriggeredRun(t, "the orphan finding fact", s.deliverUpkeepFact(t,
		"github:"+stageRepository+":pr:411:qodo-1", orphanFact))
	s.awaitPendingHumanTask(t, orphanRun, 60*time.Second)
	orphanView := s.runView(t, orphanRun)
	if got := nodeOutcome(orphanView, "route"); got != "orphan" {
		t.Fatalf("orphan run route outcome = %q, want orphan", got)
	}
	for _, node := range []string{"stage-dispatch", "stage-pr-open"} {
		if got := nodeOutcome(orphanView, node); got != "<not visited>" {
			t.Errorf("the orphan run visited %s (outcome %q); a gh: item has no ticket to comment on", node, got)
		}
	}
	if got := fakes.stagesFor("gh:" + stageRepository + "#411"); len(got) != 0 {
		t.Errorf("the orphan run posted stages %v onto its gh: work item", got)
	}
	if got := fakes.stagesFor(stageJiraKey); !equalStrings(got, wantStageOrder) {
		t.Fatalf("the orphan run changed the driven ticket's stages: %v", got)
	}
	if errs := s.errors(); len(errs) > 0 {
		t.Fatalf("stack errors: %v", errs)
	}
}

// assertStageClaimInLedger: the jira actor's completion proposes a claim
// naming the verb, the issue and the stage. It stays `proposed` -- an agent
// saying it recorded a stage is a completion claim, not verified evidence
// (PRD 10.4), and nothing in this loop promotes its own proposal.
func assertStageClaimInLedger(t *testing.T, db *postgres.Store, namespaceID, runID, jiraActorID, stage string) {
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
			Stage string `json:"stage"`
		}
		_ = json.Unmarshal(rec.Data, &data)
		if data.Verb == "post_comment" && data.Stage == stage {
			if rec.Authority != ledger.AuthorityProposed {
				t.Fatalf("stage claim authority = %q, want proposed", rec.Authority)
			}
			return
		}
	}
	t.Fatalf("no proposed post_comment claim for stage %q by actor %s in run %s's ledger (%d records)",
		stage, jiraActorID, runID, len(records))
}

// assertReadinessContextRef reads the merge task the way a presenting surface
// does: `context_refs` carries the approval's input bindings as AUTHORED (
// internal/engine/humantask.go writes pointers, never resolved values), so the
// readiness block reaches a person as `/nodes/readiness/output` -- the code
// node's output document, whose `artifacts.stdout_ref` is the block.
func assertReadinessContextRef(t *testing.T, task humanTaskOut) {
	t.Helper()
	var request struct {
		ContextRefs struct {
			Bindings map[string]json.RawMessage `json:"bindings"`
		} `json:"context_refs"`
	}
	if err := json.Unmarshal(task.Request, &request); err != nil {
		t.Fatalf("decode human task request: %v", err)
	}
	raw, ok := request.ContextRefs.Bindings["readiness"]
	if !ok {
		t.Fatalf("the merge task carries no `readiness` context ref (bindings: %v)",
			request.ContextRefs.Bindings)
	}
	var pointer string
	if err := json.Unmarshal(raw, &pointer); err != nil {
		t.Fatalf("the `readiness` context ref is not a pointer string: %s", raw)
	}
	if pointer != "/nodes/readiness/output" {
		t.Fatalf("the `readiness` context ref = %q, want /nodes/readiness/output", pointer)
	}
}
