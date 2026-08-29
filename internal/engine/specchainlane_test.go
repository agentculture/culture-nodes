package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t13 (plan jira-flow-spec-read-related-bugs, issue #199 / #230; spec
// c7/h5): the board-driven /think leg is minted from a TICKET FACT. These
// tests publish the REAL committed lane workflow
// (examples/spec-chain-lane/workflow.yaml — the document a deployment loads)
// and prove, through the exact path a sweep delivery takes
// (Store.DeliverSignalEvent with Trigger set to a real *engine.Engine), that:
//
//   - the sweep's In-Progress transition fact for a ticket mints ONE lane
//     run in the same delivery, whose input is the fact verbatim and whose
//     trigger_event_id is that fact's id (the prod signal t14 reads);
//   - the sweep's github_pr facts on the same event family and a To-Do
//     transition (jira-intake's own trigger) mint nothing here;
//   - a second In-Progress fact for a ticket already mid-flight ATTACHES to
//     the open run instead of minting a sibling (maxConcurrentSubjectRuns: 1).
func compileSpecChainLaneExample(t *testing.T) *compiler.CompiledWorkflow {
	t.Helper()
	path := filepath.Join("..", "..", "examples", "spec-chain-lane", "workflow.yaml")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cw, diags, err := compiler.Compile(source, compiler.FormatForPath(path))
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Fatalf("compile %s: %s at %s: %s", path, d.Code, d.Path, d.Message)
		}
	}
	return cw
}

type specChainLaneFixture struct {
	f *fixture
}

func newSpecChainLaneFixture(t *testing.T) *specChainLaneFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	f := newFixtureOn(t, s, "trigger-subject.workflow.yaml")
	f.cw = compileSpecChainLaneExample(t)
	publishFixtureWorkflow(t, f)
	return &specChainLaneFixture{f: f}
}

// deliver raises one transition fact exactly the way the sweep does: the
// per-issue `:status` source key and a status watermark, subject only when
// the caller supplies one.
func (c *specChainLaneFixture) deliver(t *testing.T, name string, payload map[string]any, subject string) storepg.SignalDelivery {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	id, _ := payload["id"].(string)
	status, _ := payload["status"].(string)
	delivery, err := c.f.store.DeliverSignalEvent(c.f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: c.f.ns.ID,
		Name:        name,
		Payload:     raw,
		Emitter:     "pr-upkeep/sweep",
		SourceKey:   "jira:example:" + id + ":status",
		Watermark:   json.RawMessage(`{"status":"` + status + `","event":"` + name + `"}`),
		Subject:     subject,
		Trigger:     c.f.engine,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(%s): %v", name, err)
	}
	return delivery
}

func ticketFact(issue, status string) map[string]any {
	return map[string]any{
		"source":      "jira",
		"id":          issue,
		"project":     "SCRUM",
		"severity":    "medium",
		"kind":        "story",
		"title":       "Read related bugs before speccing",
		"status":      status,
		"details_url": "example://jira/" + issue,
	}
}

func TestTicketFactMintsSpecChainLaneRunInOneDelivery(t *testing.T) {
	c := newSpecChainLaneFixture(t)

	delivery := c.deliver(t, "pr-upkeep.jira.transitioned.in-progress", ticketFact("SCRUM-9", "In Progress"), "")
	if len(delivery.Triggered) != 1 {
		t.Fatalf("In-Progress ticket fact: want exactly 1 triggered run, got %d (%+v)", len(delivery.Triggered), delivery.Triggered)
	}
	minted := delivery.Triggered[0]
	if minted.Attached || minted.Deferred || minted.RunID == "" {
		t.Fatalf("the ticket fact must mint a NEW run in the same delivery: %+v", minted)
	}

	run := c.f.run(minted.RunID)
	if run.State.Terminal() {
		t.Fatalf("freshly minted lane run is unexpectedly terminal: %s", run.State)
	}
	if run.TriggerEventID != delivery.Event.ID {
		t.Fatalf("trigger_event_id = %q, want the ticket fact %q (the prod signal t14 reads)", run.TriggerEventID, delivery.Event.ID)
	}
	var input struct {
		Source string `json:"source"`
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(run.Input, &input); err != nil {
		t.Fatalf("decode run input: %v", err)
	}
	if input.Source != "jira" || input.ID != "SCRUM-9" || input.Status != "In Progress" || input.Title == "" {
		t.Fatalf("the ticket fact is not the run's input verbatim: %+v", input)
	}
}

func TestOtherFactsOnTheTransitionFamilyDoNotMintLaneRuns(t *testing.T) {
	c := newSpecChainLaneFixture(t)

	pr := map[string]any{"source": "github_pr", "id": "42", "title": "a PR", "status": "In Progress"}
	if d := c.deliver(t, "pr-upkeep.jira.transitioned.in-progress", pr, ""); len(d.Triggered) != 0 {
		t.Fatalf("a github_pr fact must not mint the /think leg: %+v", d.Triggered)
	}
	if d := c.deliver(t, "pr-upkeep.jira.transitioned.to-do", ticketFact("SCRUM-9", "To Do"), ""); len(d.Triggered) != 0 {
		t.Fatalf("a To-Do transition is jira-intake's fact, not this lane's: %+v", d.Triggered)
	}
}

func TestSecondInProgressFactAttachesToTheOpenLaneRun(t *testing.T) {
	c := newSpecChainLaneFixture(t)

	first := c.deliver(t, "pr-upkeep.jira.transitioned.in-progress", ticketFact("SCRUM-9", "In Progress"), "SCRUM-9")
	if len(first.Triggered) != 1 || first.Triggered[0].Attached {
		t.Fatalf("first fact must mint: %+v", first.Triggered)
	}
	// A different watermark, same subject, while the first run is open.
	again := ticketFact("SCRUM-9", "In Progress")
	again["title"] = "Read related bugs before speccing (edited)"
	second, err := c.f.store.DeliverSignalEvent(c.f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: c.f.ns.ID,
		Name:        "pr-upkeep.jira.transitioned.in-progress",
		Payload:     mustJSON(t, again),
		Emitter:     "pr-upkeep/sweep",
		SourceKey:   "jira:example:SCRUM-9:status",
		Watermark:   json.RawMessage(`{"status":"In Progress","edit":"2"}`),
		Subject:     "SCRUM-9",
		Trigger:     c.f.engine,
	})
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if len(second.Triggered) != 1 || !second.Triggered[0].Attached {
		t.Fatalf("a second fact for a ticket mid-flight must ATTACH, not mint a sibling: %+v", second.Triggered)
	}
	if second.Triggered[0].RunID != first.Triggered[0].RunID {
		t.Fatalf("attached to %q, want the open run %q", second.Triggered[0].RunID, first.Triggered[0].RunID)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}
