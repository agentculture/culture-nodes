package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// publishActiveBy publishes and activates d under author, the identity the
// sensitivity check resolves as the owner of the variables it produces.
func publishActiveBy(t *testing.T, db *postgres.Store, ns, author string, d decl.Declaration) postgres.DeclarationVersion {
	t.Helper()
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.PublishDeclaration(context.Background(), postgres.PublishDeclarationInput{NamespaceID: ns, Name: d.Name, Body: body, Author: author})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeclarationActivation(context.Background(), ns, v.ID, "activate", author, ""); err != nil {
		t.Fatal(err)
	}
	return v
}

// announceDeclaration renders the triggering Jira issue's summary into a
// Discord post: the spec's own example of a widening (team -> public).
func announceDeclaration(text string) decl.Declaration {
	d := active("announce").Declaration
	d.Trigger.Kind = "jira.issue.created"
	d.Condition = "true"
	d.Action = decl.Action{Kind: "discord.post", With: json.RawMessage(`{"uses":"actor://discord","input":{"text":"` + text + `"}}`)}
	return d
}

func sensitivityEngine(t *testing.T, db *postgres.Store, calls *int) *Engine {
	t.Helper()
	t.Setenv("TCA_PG_SENS_KEY", strings.Repeat("k", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_PG_SENS_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db},
		dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
			*calls++
			return DispatchResult{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// handleJira delivers one jira.issue.created event and returns the terminal
// outcome and reason the firing loop recorded for the declaration.
func handleJira(t *testing.T, e *Engine, db *postgres.Store, ns, declID string, vars map[string]any) (string, string) {
	t.Helper()
	eventID := deliver(t, db, ns)
	if err := e.Handle(context.Background(), Event{NamespaceID: ns, ID: eventID, Kind: "jira.issue.created", Node: "ready", Variables: vars}); err != nil {
		t.Fatal(err)
	}
	var outcome, reason string
	if err := db.Pool().QueryRow(context.Background(), `SELECT outcome,reason FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3 ORDER BY created_at DESC,id DESC LIMIT 1`,
		ns, eventID, declID).Scan(&outcome, &reason); err != nil {
		t.Fatal(err)
	}
	return outcome, reason
}

func approvalCount(t *testing.T, db *postgres.Store, ns string) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM declaration_sensitivity_approvals WHERE namespace_id=$1`, ns).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Criteria 1 and 2 end to end: a widening render is blocked (never
// dispatched) and opens exactly one approval task addressed to the owner;
// only that owner, as a human, can decide it; a refusal keeps blocking; an
// approval (which supersedes the refusal, append-only) lets the next event
// fire; and a new declaration version needs a new approval.
func TestPostgresSensitivityBlockApproveRefuse(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-sensitivity").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	v1 := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("New issue: {summary}"))
	declID := v1.DeclarationID
	vars := map[string]any{"summary": "customer X is churning"}

	for i := 0; i < 2; i++ {
		outcome, reason := handleJira(t, e, db, ns, declID, vars)
		if outcome != OutcomeSensitivityBlocked {
			t.Fatalf("event %d: outcome %q (%s), want %q", i, outcome, reason, OutcomeSensitivityBlocked)
		}
		for _, want := range []string{`"summary"`, "jira (team audience)", "discord (public audience)", "alice@example.com", "pending"} {
			if !strings.Contains(reason, want) {
				t.Errorf("blocked reason %q does not name %s", reason, want)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("a blocked widening dispatched %d times", calls)
	}
	var firings int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1`, ns).Scan(&firings); err != nil || firings != 0 {
		t.Fatalf("a blocked widening claimed %d firings (err %v)", firings, err)
	}
	if n := approvalCount(t, db, ns); n != 1 {
		t.Fatalf("two blocked events opened %d approval tasks, want exactly 1", n)
	}
	inbox, err := ListSensitivityApprovals(ctx, db, ns, "alice@example.com", SensitivityPending)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("owner inbox = %+v, %v", inbox, err)
	}
	task := inbox[0]
	if task.Variable != "summary" || task.SourceSystem != "jira" || task.TargetSystem != "discord" || task.DeclarationVersionID != v1.ID || task.SourceVersionID != v1.ID {
		t.Fatalf("approval task = %+v", task)
	}

	// Only a human, and only the owner.
	if _, err := DecideSensitivityApproval(ctx, db, ns, task.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "alice@example.com"}, SensitivityApproved, ""); !errors.Is(err, ErrSensitivityNotHuman) {
		t.Fatalf("agent approval err = %v, want ErrSensitivityNotHuman", err)
	}
	if _, err := DecideSensitivityApproval(ctx, db, ns, task.ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "bob@example.com"}, SensitivityApproved, ""); !errors.Is(err, ErrSensitivityNotOwner) {
		t.Fatalf("non-owner approval err = %v, want ErrSensitivityNotOwner", err)
	}
	if ds, _ := ListSensitivityDecisions(ctx, db, ns, task.ID); len(ds) != 0 {
		t.Fatalf("refused deciders still recorded decisions: %+v", ds)
	}

	owner := ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}
	refusal, err := DecideSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityRefused, "not for the public channel")
	if err != nil {
		t.Fatal(err)
	}
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeSensitivityBlocked || !strings.Contains(reason, "refused") {
		t.Fatalf("after refusal: %q %q, want still blocked and naming the refusal", outcome, reason)
	}
	approval, err := DecideSensitivityApproval(ctx, db, ns, task.ID, owner, SensitivityApproved, "fine after all")
	if err != nil {
		t.Fatal(err)
	}
	if approval.SupersedesID != refusal.ID {
		t.Fatalf("approval supersedes %q, want the refusal %q", approval.SupersedesID, refusal.ID)
	}
	if outcome, reason := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeFired || calls != 1 {
		t.Fatalf("after approval: %q %q calls=%d, want fired once", outcome, reason, calls)
	}
	if n := approvalCount(t, db, ns); n != 1 {
		t.Fatalf("approved widening opened more tasks: %d", n)
	}

	// Append-only: both decisions remain, and neither can be rewritten.
	ds, err := ListSensitivityDecisions(ctx, db, ns, task.ID)
	if err != nil || len(ds) != 2 || ds[0].Decision != SensitivityRefused || ds[1].Decision != SensitivityApproved {
		t.Fatalf("decision history = %+v, %v", ds, err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE declaration_sensitivity_decisions SET decision='approved' WHERE id=$1`, refusal.ID); err == nil {
		t.Fatal("a sensitivity decision was rewritten in place")
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM declaration_sensitivity_approvals WHERE id=$1`, task.ID); err == nil {
		t.Fatal("a sensitivity approval task was deleted")
	}
	got, err := GetSensitivityApproval(ctx, db, ns, task.ID)
	if err != nil || got.Status != SensitivityApproved || got.DecisionID != approval.ID {
		t.Fatalf("approval state = %+v, %v", got, err)
	}

	// A new version is a new question.
	v2 := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("Issue filed: {summary}"))
	if outcome, _ := handleJira(t, e, db, ns, declID, vars); outcome != OutcomeSensitivityBlocked {
		t.Fatalf("new version reused the old approval: outcome %q", outcome)
	}
	if n := approvalCount(t, db, ns); n != 2 {
		t.Fatalf("new version opened %d tasks in total, want 2", n)
	}
	if pending, _ := ListSensitivityApprovals(ctx, db, ns, "", SensitivityPending); len(pending) != 1 || pending[0].DeclarationVersionID != v2.ID {
		t.Fatalf("pending after republish = %+v", pending)
	}
}

// A reference with no value renders its default or the placeholder, which
// exposes nothing: no block and no task. A narrowing or same-audience render
// is never blocked either.
func TestPostgresSensitivityOnlyPresentWideningValuesBlock(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ns := pgtest.MustNamespace(t, db, "tca-sensitivity-absent").ID
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	v := publishActiveBy(t, db, ns, "alice@example.com", announceDeclaration("New issue: {summary:(none)}"))
	if outcome, reason := handleJira(t, e, db, ns, v.DeclarationID, map[string]any{"other": "x"}); outcome != OutcomeFired {
		t.Fatalf("absent variable: %q %q, want fired", outcome, reason)
	}

	same := active("comment-back").Declaration
	same.Trigger.Kind, same.Condition = "jira.issue.created", "true"
	same.Action = decl.Action{Kind: "jira.comment", With: json.RawMessage(`{"uses":"actor://jira","input":{"body":"{summary}"}}`)}
	ns2 := pgtest.MustNamespace(t, db, "tca-sensitivity-same").ID
	v2 := publishActiveBy(t, db, ns2, "alice@example.com", same)
	if outcome, reason := handleJira(t, e, db, ns2, v2.DeclarationID, map[string]any{"summary": "s"}); outcome != OutcomeFired {
		t.Fatalf("same-audience render: %q %q, want fired", outcome, reason)
	}
	if approvalCount(t, db, ns)+approvalCount(t, db, ns2) != 0 {
		t.Fatal("a non-widening render opened an approval task")
	}
}

// Publish warns (never refuses) once per widening reference.
func TestPostgresSensitivityWarnings(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-sensitivity-warn").ID
	intake := active("jira-intake").Declaration
	intake.Trigger.Kind = "jira.issue.created"
	intake.Action = decl.Action{Kind: "jira.comment", With: json.RawMessage(`{"uses":"actor://jira","input":{}}`)}
	publishActiveBy(t, db, ns, "alice@example.com", intake)

	d := announceDeclaration("{jira-intake:owner} {jira-intake:owner} {summary} {1:x:y}")
	ws, err := SensitivityWarnings(ctx, db, ns, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("warnings = %q, want one each for jira-intake:owner, summary and the lineage-only step 1", ws)
	}
	for _, w := range ws {
		if !strings.HasPrefix(w, "sensitivity: ") {
			t.Errorf("warning %q lacks the sensitivity prefix", w)
		}
	}
	comment := d
	comment.Action.Kind = "jira.comment"
	comment.Action.With = json.RawMessage(`{"body":"{jira-intake:owner} {summary}"}`)
	if ws, err := SensitivityWarnings(ctx, db, ns, comment); err != nil || len(ws) != 0 {
		t.Fatalf("same-audience references warned: %q %v", ws, err)
	}
}
