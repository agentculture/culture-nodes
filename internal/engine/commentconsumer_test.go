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

// Task t9 leg 1 (plan jira-operating-surface-flow-store, issue #197): a
// human's bare comment on a tracked ticket emitted pr-upkeep.jira.comment
// and NOTHING consumed it — measured live on SCRUM-3 (2026-08-19), where
// four go-signals (comments 10106/10107/10109/10110) died unheard and the
// operator session had to poll. These tests publish the REAL committed
// subscriber (examples/jira-comment-consumer/workflow.yaml — the document a
// deployment loads, not a testdata stand-in) and prove, through the exact
// path a real delivery takes (Store.DeliverSignalEvent with Trigger set to
// a real *engine.Engine, the same call handleDeliverEvent makes), that:
//
//   - a bare human comment fact mints a consumer run within ONE trigger
//     delivery — no polling anywhere, the run exists when the delivery
//     call returns;
//   - a fact carrying originating_question_id (a human ANSWER to a marked
//     engine question, owned by whichever parked flow asked) does NOT mint
//     a run here;
//   - a fact flagged self_originated (the WP-E author-id discipline) does
//     NOT mint a run here, whatever the emitter's own filter did;
//   - a subject-bearing second comment on an issue mid-flight ATTACHES to
//     the open run instead of minting a sibling (the t15 discipline the
//     workflow's maxConcurrentSubjectRuns declaration leans on).
func compileCommentConsumerExample(t *testing.T) *compiler.CompiledWorkflow {
	t.Helper()
	path := filepath.Join("..", "..", "examples", "jira-comment-consumer", "workflow.yaml")
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

type commentConsumerFixture struct {
	f *fixture
}

func newCommentConsumerFixture(t *testing.T) *commentConsumerFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	f := newFixtureOn(t, s, "trigger-subject.workflow.yaml")
	f.cw = compileCommentConsumerExample(t)
	publishFixtureWorkflow(t, f)
	return &commentConsumerFixture{f: f}
}

// deliver raises one pr-upkeep.jira.comment fact exactly the way the sweep
// does (name, payload, per-history-position source key, watermark; subject
// only when the caller supplies one) and returns the delivery.
func (c *commentConsumerFixture) deliver(t *testing.T, payload map[string]any, sourceKey, subject string) storepg.SignalDelivery {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	delivery, err := c.f.store.DeliverSignalEvent(c.f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: c.f.ns.ID,
		Name:        "pr-upkeep.jira.comment",
		Payload:     raw,
		Emitter:     "pr-upkeep/sweep",
		SourceKey:   sourceKey,
		Watermark:   json.RawMessage(`{"changelog_id":"","comment_id":"` + sourceKey + `"}`),
		Subject:     subject,
		Trigger:     c.f.engine,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(%s): %v", sourceKey, err)
	}
	return delivery
}

func bareCommentPayload(issue, commentID, body string) map[string]any {
	return map[string]any{
		"source": "jira",
		"id":     issue,
		"title":  "Complete a round trip between sweeps",
		"status": "In Progress",
		"answer": map[string]any{"comment_id": commentID, "body": body},
	}
}

func TestBareHumanCommentFactMintsConsumerRunInOneDelivery(t *testing.T) {
	c := newCommentConsumerFixture(t)

	delivery := c.deliver(t, bareCommentPayload("SCRUM-3", "10106", "please pick this up"), "10106", "")
	if len(delivery.Triggered) != 1 {
		t.Fatalf("bare human comment: want exactly 1 triggered run, got %d (%+v)", len(delivery.Triggered), delivery.Triggered)
	}
	minted := delivery.Triggered[0]
	if minted.Attached || minted.Deferred || minted.RunID == "" {
		t.Fatalf("bare human comment must mint a NEW run in the same delivery: %+v", minted)
	}

	// "Within one trigger delivery — no polling anywhere": the run row and
	// its entry token exist the moment the delivery call returns, with no
	// scheduler/sweep pass in between.
	run := c.f.run(minted.RunID)
	if run.State.Terminal() {
		t.Fatalf("freshly minted consumer run is unexpectedly terminal: %s", run.State)
	}
	var input struct {
		ID     string `json:"id"`
		Answer struct {
			CommentID string `json:"comment_id"`
			Body      string `json:"body"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(run.Input, &input); err != nil {
		t.Fatalf("decode run input: %v", err)
	}
	if input.ID != "SCRUM-3" || input.Answer.CommentID != "10106" || input.Answer.Body != "please pick this up" {
		t.Fatalf("the comment fact is not the run's input verbatim: %+v", input)
	}
}

func TestQuestionAnswerAndSelfOriginatedFactsDoNotMintConsumerRuns(t *testing.T) {
	c := newCommentConsumerFixture(t)

	answer := bareCommentPayload("SCRUM-3", "10118", "approve 01M09SZ68ECPAF75YE0DMVV63A")
	answer["originating_question_id"] = "01M09SZ68ECPAF75YE0DMVV63A"
	if delivery := c.deliver(t, answer, "10118", ""); len(delivery.Triggered) != 0 {
		t.Fatalf("a marked-question ANSWER belongs to the parked flow that asked, never this consumer: %+v", delivery.Triggered)
	}

	echoed := bareCommentPayload("SCRUM-3", "10119", "started run 01M0...")
	echoed["self_originated"] = true
	if delivery := c.deliver(t, echoed, "10119", ""); len(delivery.Triggered) != 0 {
		t.Fatalf("a self-originated fact must never mint a consumer run: %+v", delivery.Triggered)
	}

	// The sweep's github_pr facts share nothing with this trigger's payload
	// shape; the guard must decline them cleanly, not error the delivery
	// (c.deliver fails the test on a delivery error).
	if delivery := c.deliver(t, map[string]any{"source": "github_pr", "repository": "o/r"}, "pr-60", ""); len(delivery.Triggered) != 0 {
		t.Fatalf("a github_pr fact must not reach the Jira comment consumer: %+v", delivery.Triggered)
	}

	var runs int
	if err := c.f.store.Pool().QueryRow(c.f.ctx, `SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1`, c.f.ns.ID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("declined facts still created %d run(s)", runs)
	}
}

func TestSecondSubjectBearingCommentAttachesInsteadOfMintingSibling(t *testing.T) {
	c := newCommentConsumerFixture(t)

	first := c.deliver(t, bareCommentPayload("SCRUM-3", "10107", "go-signal one"), "10107", "SCRUM-3")
	if len(first.Triggered) != 1 || first.Triggered[0].Attached {
		t.Fatalf("first subject-bearing comment must mint a new run: %+v", first.Triggered)
	}

	second := c.deliver(t, bareCommentPayload("SCRUM-3", "10109", "go-signal two"), "10109", "SCRUM-3")
	if len(second.Triggered) != 1 || !second.Triggered[0].Attached {
		t.Fatalf("second comment on an issue mid-flight must attach, not mint a sibling: %+v", second.Triggered)
	}
	if second.Triggered[0].RunID != first.Triggered[0].RunID {
		t.Fatalf("attached to %s, want the open run %s", second.Triggered[0].RunID, first.Triggered[0].RunID)
	}
}
