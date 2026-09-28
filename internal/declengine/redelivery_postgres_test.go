package declengine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestPostgresDecidedEventRedelivery(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-redelivery").ID
	falseDecl := declFor("redelivery-false", RootNode, "none", "waiting", "none", "pr-upkeep.pr", `{"uses":"actor://test"}`)
	falseDecl.Condition = "false"
	fv := publishActive(t, db, ns, falseDecl)
	trueDecl := declFor("redelivery-fired", RootNode, "none", "waiting", "none", "pr-upkeep.pr", `{"uses":"actor://test"}`)
	trueDecl.Condition = "true"
	tv := publishActive(t, db, ns, trueDecl)
	sw := PostgresSwitchStore{Store: db}
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "test"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	t.Setenv("TCA_REDELIVERY_KEY", strings.Repeat("r", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_REDELIVERY_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	router := Router{Engine: e, Switch: sw}
	in := postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "pr-upkeep.pr", Emitter: "test", SourceKey: "redelivery-event", Watermark: json.RawMessage(`1`), Declarations: router}
	deliver := func() postgres.SignalDelivery {
		t.Helper()
		d, err := db.DeliverSignalEvent(ctx, in)
		if err != nil || d.DeclarationErr != nil {
			t.Fatalf("delivery: %v; declaration: %v", err, d.DeclarationErr)
		}
		return d
	}
	first := deliver()
	before := countRows(t, db, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, first.Event.ID)
	if before == 0 || calls != 1 {
		t.Fatalf("first delivery: evaluations=%d calls=%d", before, calls)
	}
	second := deliver()
	if second.Event.ID != first.Event.ID {
		t.Fatalf("duplicate event id %s, want %s", second.Event.ID, first.Event.ID)
	}
	if got := countRows(t, db, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, first.Event.ID); got != before {
		t.Fatalf("redelivery appended %d rows", got-before)
	}
	if calls != 1 {
		t.Fatalf("redelivery dispatched %d times", calls)
	}
	if got, _ := terminalOutcome(t, db, ns, first.Event.ID, fv.DeclarationID); got != OutcomeConditionFalse {
		t.Fatalf("false outcome %q", got)
	}
	if got, _ := terminalOutcome(t, db, ns, first.Event.ID, tv.DeclarationID); got != OutcomeFired {
		t.Fatalf("fired outcome %q", got)
	}
	falseDecl.Condition = "true"
	publishActive(t, db, ns, falseDecl)
	deliver()
	if got := countRows(t, db, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, first.Event.ID); got != before {
		t.Fatalf("new version appended %d rows", got-before)
	}
}

func TestPostgresSensitivityBlockStaysDecidedOnRedelivery(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-redelivery-sensitive").ID
	// A real delivery lands at root; the helper's declaration starts at "ready".
	d := announceDeclaration("New issue: {summary}", "summary")
	d.StartNode.Name = RootNode
	d.StartNode.Deadline = "none"
	v := publishActiveBy(t, db, ns, "alice@example.com", d)
	sw := PostgresSwitchStore{Store: db}
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "test"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e := sensitivityEngine(t, db, &calls)
	in := postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "jira.issue.created", Emitter: "test", Payload: json.RawMessage(`{"summary":"private"}`), SourceKey: "sensitive-event", Watermark: json.RawMessage(`1`), Declarations: Router{Engine: e, Switch: sw}}
	first, err := db.DeliverSignalEvent(ctx, in)
	if err != nil || first.DeclarationErr != nil {
		t.Fatalf("first: %v; declaration: %v", err, first.DeclarationErr)
	}
	if got, _ := terminalOutcome(t, db, ns, first.Event.ID, v.DeclarationID); got != OutcomeSensitivityBlocked {
		t.Fatalf("first outcome %q", got)
	}
	before := countRows(t, db, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, first.Event.ID)
	inbox, err := ListSensitivityApprovals(ctx, db, ns, "alice@example.com", SensitivityPending)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("approvals: %+v; %v", inbox, err)
	}
	if _, err := DecideSensitivityApproval(ctx, db, ns, inbox[0].ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "alice@example.com"}, SensitivityApproved, "approved"); err != nil {
		t.Fatal(err)
	}
	second, err := db.DeliverSignalEvent(ctx, in)
	if err != nil || second.DeclarationErr != nil {
		t.Fatalf("second: %v; declaration: %v", err, second.DeclarationErr)
	}
	if got := countRows(t, db, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, first.Event.ID); got != before {
		t.Fatalf("redelivery appended %d rows", got-before)
	}
	if calls != 0 {
		t.Fatalf("blocked event dispatched %d times after approval", calls)
	}
}

func TestPostgresUndecidedEventRedelivery(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-redelivery-crash").ID
	v := publishActive(t, db, ns, declFor("redelivery-crash", RootNode, "none", "waiting", "none", "pr-upkeep.pr", `{"uses":"actor://test"}`))
	sw := PostgresSwitchStore{Store: db}
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TCA_REDELIVERY_KEY", strings.Repeat("r", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_REDELIVERY_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { return DispatchResult{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	in := postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "pr-upkeep.pr", Emitter: "test", SourceKey: "crash-event", Watermark: json.RawMessage(`1`)}
	first, err := db.DeliverSignalEvent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := (PostgresBackend{db}).Record(ctx, Evaluation{NamespaceID: ns, EventID: first.Event.ID, DeclarationID: v.DeclarationID, VersionID: v.ID, Outcome: OutcomeMatched, Reason: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	in.Declarations = Router{Engine: e, Switch: sw}
	second, err := db.DeliverSignalEvent(ctx, in)
	if err != nil || second.DeclarationErr != nil {
		t.Fatalf("recovery: %v; declaration: %v", err, second.DeclarationErr)
	}
	if got, _ := terminalOutcome(t, db, ns, first.Event.ID, v.DeclarationID); got != OutcomeFired {
		t.Fatalf("recovery outcome %q", got)
	}
}
