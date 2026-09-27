package declengine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// conditionTestDecl is ordinaryTestDecl (activation_test.go) with a
// controllable node, trigger kind and condition -- used to publish two
// declarations whose conditions may or may not overlap.
func conditionTestDecl(name, node, triggerKind, condition string) decl.Declaration {
	return decl.Declaration{
		Name:        name,
		Trigger:     decl.Trigger{Kind: triggerKind, ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Condition:   condition,
		Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test","input":{}}`)},
		StartNode:   decl.Node{Name: node, Deadline: "none"},
		LandingNode: decl.Node{Name: "waiting-" + name, Deadline: "1h"},
	}
}

// overlapListenerTestDecl is an ordinary declaration whose trigger kind is
// declaration.overlap -- publishing and activating one is what "configured"
// (the brief's own word) means for ReportOverlaps.
func overlapListenerTestDecl(name string) decl.Declaration {
	return decl.Declaration{
		Name:        name,
		Trigger:     decl.Trigger{Kind: OverlapTriggerKind, ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Condition:   "true",
		Action:      decl.Action{Kind: "human.ask"},
		StartNode:   decl.Node{Name: "overlap-root", Deadline: "none"},
		LandingNode: decl.Node{Name: "overlap-seen", Deadline: "none"},
	}
}

func publishAndActivate(t *testing.T, db *postgres.Store, ns string, d decl.Declaration) postgres.DeclarationVersion {
	t.Helper()
	v, err := Publish(context.Background(), db, StoreActivationLookup{Store: db}, PublishInput{
		NamespaceID: ns, Body: mustCanonical(t, d), Format: decl.FormatJSON,
		Principal: ActivationPrincipal{Kind: PrincipalHuman, Author: "human:overlap-test"},
	})
	if err != nil {
		t.Fatalf("publish %q: %v", d.Name, err)
	}
	if err := Activate(context.Background(), db, ns, v.ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "human:overlap-test"}, ""); err != nil {
		t.Fatalf("activate %q: %v", d.Name, err)
	}
	return v
}

func countOverlapSignalEvents(t *testing.T, db *postgres.Store, ns string) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name=$2`, ns, OverlapTriggerKind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Acceptance criterion 2, first half: without a configured listener, the
// report is computed but nothing reaches signal_events -- "when configured"
// (the brief's own words) gates emission, not detection.
func TestReportOverlapsNoListenerNoSignalEvent(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ns := pgtest.MustNamespace(t, db, "overlap-no-listener").ID

	publishAndActivate(t, db, ns, conditionTestDecl("high", "ready", "jira.issue.created", "event.priority == 'High'"))
	publishAndActivate(t, db, ns, conditionTestDecl("not-low", "ready", "jira.issue.created", "event.priority != 'Low'"))

	if n := countOverlapSignalEvents(t, db, ns); n != 0 {
		t.Fatalf("signal_events has %d declaration.overlap rows with no listener configured, want 0", n)
	}
}

// Acceptance criterion 2, second half: "when configured, a
// declaration.overlap event reaches the signal_events log." Also exercises
// the brief's exact fixture pair (priority=='High' vs priority!='Low') end
// to end through Publish/Activate, the two hooks named in the brief.
func TestReportOverlapsListenerConfiguredEmitsSignalEvent(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "overlap-listener").ID

	publishAndActivate(t, db, ns, overlapListenerTestDecl("listener"))
	publishAndActivate(t, db, ns, conditionTestDecl("high", "ready", "jira.issue.created", "event.priority == 'High'"))
	// Activating the second declaration is the "every activation" half of
	// h42 -- this call is what must produce the event, not the first one
	// (only one active declaration exists when "high" activates).
	if n := countOverlapSignalEvents(t, db, ns); n != 0 {
		t.Fatalf("no pair exists yet (only one candidate declaration active): got %d rows", n)
	}
	publishAndActivate(t, db, ns, conditionTestDecl("not-low", "ready", "jira.issue.created", "event.priority != 'Low'"))

	if n := countOverlapSignalEvents(t, db, ns); n != 1 {
		t.Fatalf("signal_events has %d declaration.overlap rows, want 1", n)
	}

	var payload []byte
	if err := db.Pool().QueryRow(ctx,
		`SELECT payload FROM signal_events WHERE namespace_id=$1 AND name=$2`, ns, OverlapTriggerKind).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var pair OverlapPair
	if err := json.Unmarshal(payload, &pair); err != nil {
		t.Fatalf("payload %s: %v", payload, err)
	}
	if pair.Status != OverlapConfirmed {
		t.Fatalf("delivered pair status = %q, want %q", pair.Status, OverlapConfirmed)
	}
	if pair.Node != "ready" || pair.TriggerKind != "jira.issue.created" {
		t.Fatalf("delivered pair = %+v, want node=ready trigger=jira.issue.created", pair)
	}
	names := map[string]bool{pair.A.Name: true, pair.B.Name: true}
	if !names["high"] || !names["not-low"] {
		t.Fatalf("delivered pair does not name both declarations: %+v", pair)
	}
}

// The brief's second fixture: a contradiction is decided and never
// reported, even with a listener configured.
func TestReportOverlapsContradictionNeverEmitsEvenWithListener(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ns := pgtest.MustNamespace(t, db, "overlap-contradiction").ID

	publishAndActivate(t, db, ns, overlapListenerTestDecl("listener"))
	publishAndActivate(t, db, ns, conditionTestDecl("high", "ready", "jira.issue.created", "event.priority == 'High'"))
	publishAndActivate(t, db, ns, conditionTestDecl("low", "ready", "jira.issue.created", "event.priority == 'Low'"))

	if n := countOverlapSignalEvents(t, db, ns); n != 0 {
		t.Fatalf("signal_events has %d declaration.overlap rows for a decided contradiction, want 0", n)
	}
}

// ReportOverlaps itself (the function Publish/Activate hook into) returns
// the full pair set regardless of whether a listener is configured, which
// is what a future show/focus surface will call directly.
func TestReportOverlapsReturnsPairsRegardlessOfListener(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "overlap-return-value").ID

	publishAndActivate(t, db, ns, conditionTestDecl("high", "ready", "jira.issue.created", "event.priority == 'High'"))
	publishAndActivate(t, db, ns, conditionTestDecl("not-low", "ready", "jira.issue.created", "event.priority != 'Low'"))

	pairs, err := ReportOverlaps(ctx, db, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Status != OverlapConfirmed {
		t.Fatalf("ReportOverlaps() = %+v, want one confirmed pair", pairs)
	}
	report := FormatOverlapReport(pairs)
	if report == "" {
		t.Fatal("FormatOverlapReport must render a non-empty report for a non-empty pair set")
	}
}
