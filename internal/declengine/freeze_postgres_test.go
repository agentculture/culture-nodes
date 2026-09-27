package declengine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Acceptance 1 and 2 (task t18, #328, spec c81/h54): flipping back freezes
// an open declaration node with a visible record; an event arriving for
// that frozen node is stored against it (not matched/evaluated), and a
// redelivery of the same event id while still frozen is a silent dedup;
// flipping forward again thaws the node and replays its stored events in
// arrival order, firing the waiting reaction exactly once; a later
// redelivery of that same reaction's event id, after replay, fires nothing.
func TestPostgresFreezeStoresRedeliveryDedupsAndReplayFiresOnce(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-freeze").ID

	a := declFor("frz-a", "intake", "none", "waiting", "none", "timer", `{"uses":"actor://test"}`)
	b := declFor("frz-b", "waiting", "none", "done", "none", "timer", `{"uses":"actor://test"}`)
	va := publishActive(t, db, ns, a)
	vb := publishActive(t, db, ns, b)

	dispatcher := &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}
	sw := PostgresSwitchStore{Store: db}
	fb := PostgresBackend{db}
	gate := ShadowGate{Switch: sw, Underlying: dispatcher}
	t.Setenv("TCA_FREEZE_KEY", strings.Repeat("z", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_FREEZE_KEY"}, fb, PostgresMarkerStore{db}, gate)
	if err != nil {
		t.Fatal(err)
	}

	// Start the namespace in 'after' so a's firing dispatches for real and
	// opens a genuine landing node (mirrors t17/t13's own setup).
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "start after"); err != nil {
		t.Fatal(err)
	}
	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	fa := firingByEventDecl(t, db, ns, first, va.DeclarationID)
	node, found, err := fb.NodeByFiring(ctx, ns, fa.ID)
	if err != nil || !found || node.State != NodeStateOpen {
		t.Fatalf("opened node=%+v found=%v err=%v", node, found, err)
	}

	// Flip back to 'before': FreezeHookAt runs inside this same Flip
	// transaction and must freeze the still-open node with a visible record.
	if _, err := sw.Flip(ctx, ns, ModeBefore, "human:ops", "rollback", FreezeHookAt(fb, time.Now)); err != nil {
		t.Fatal(err)
	}
	frozen, found, err := fb.NodeStatus(ctx, ns, node.ID)
	if err != nil || !found || frozen.State != NodeStateFrozen {
		t.Fatalf("frozen node=%+v found=%v err=%v, want state=%s", frozen, found, err, NodeStateFrozen)
	}

	// An event arriving for the frozen node is stored, not fired.
	react1 := deliver(t, db, ns)
	event1 := reactEvent(t, db, ns, react1, fa, "timer", map[string]any{"x": float64(1)})
	if err := e.Handle(ctx, event1); err != nil {
		t.Fatal(err)
	}
	var firedWhileFrozen int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, ns, react1).Scan(&firedWhileFrozen); err != nil {
		t.Fatal(err)
	}
	if firedWhileFrozen != 0 {
		t.Fatalf("event fired %d declarations while node was frozen, want 0", firedWhileFrozen)
	}
	stored, err := fb.FrozenEvents(ctx, ns, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].ID != react1 || stored[0].Kind != "timer" {
		t.Fatalf("frozen events = %+v, want exactly one for %s", stored, react1)
	}

	// A provider redelivery of the same event id, still while frozen, is a
	// silent dedup: no second stored row, no error.
	if err := e.Handle(ctx, event1); err != nil {
		t.Fatal(err)
	}
	stored, err = fb.FrozenEvents(ctx, ns, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("frozen events after redelivery = %d, want 1 (deduped)", len(stored))
	}

	// Flip forward again: the node resumes and replays its stored event,
	// firing b exactly once.
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "resume"); err != nil {
		t.Fatal(err)
	}
	if err := ThawAndReplay(ctx, e, fb, ns, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	consumed, found, err := fb.NodeStatus(ctx, ns, node.ID)
	if err != nil || !found || consumed.State != NodeStateClosed || consumed.ClosedReason != NodeReasonConsumed {
		t.Fatalf("post-replay node=%+v found=%v err=%v, want closed/consumed", consumed, found, err)
	}
	var firedAfterReplay int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`,
		ns, react1, vb.DeclarationID).Scan(&firedAfterReplay); err != nil {
		t.Fatal(err)
	}
	if firedAfterReplay != 1 {
		t.Fatalf("replay fired %d times, want exactly 1", firedAfterReplay)
	}
	remaining, err := fb.FrozenEvents(ctx, ns, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("frozen events not cleared after replay: %+v", remaining)
	}

	// A later redelivery of the same reaction's event id, after it has
	// already replayed and fired, fires nothing: the node it targeted is
	// now closed, so it takes the ordinary already-closed path.
	if err := e.Handle(ctx, event1); err != nil {
		t.Fatal(err)
	}
	var firedAfterRedelivery int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`,
		ns, react1, vb.DeclarationID).Scan(&firedAfterRedelivery); err != nil {
		t.Fatal(err)
	}
	if firedAfterRedelivery != 1 {
		t.Fatalf("post-replay redelivery fired again: count=%d, want still 1", firedAfterRedelivery)
	}
}

// Acceptance 1: a frozen node's deadline is paused, not ticking, and it is
// excluded from ExpireDue while frozen; when it thaws, the deadline resumes
// from however much time remained rather than either firing immediately or
// getting a fresh full duration.
func TestPostgresFreezePausesDeadlineAndExcludesFromExpiry(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-freeze-deadline").ID

	a := declFor("frzd-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`)
	va := publishActive(t, db, ns, a)

	dispatcher := &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}
	sw := PostgresSwitchStore{Store: db}
	fb := PostgresBackend{db}
	gate := ShadowGate{Switch: sw, Underlying: dispatcher}
	t.Setenv("TCA_FREEZE_DEADLINE_KEY", strings.Repeat("d", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_FREEZE_DEADLINE_KEY"}, fb, PostgresMarkerStore{db}, gate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "start after"); err != nil {
		t.Fatal(err)
	}

	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	fa := firingByEventDecl(t, db, ns, first, va.DeclarationID)
	node, found, err := fb.NodeByFiring(ctx, ns, fa.ID)
	if err != nil || !found || node.Deadline.IsZero() {
		t.Fatalf("opened node=%+v found=%v err=%v", node, found, err)
	}
	originalDeadline := node.Deadline

	// Freeze 30 minutes before the original deadline: 30 minutes remained.
	freezeAt := originalDeadline.Add(-30 * time.Minute)
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := fb.FreezeOpenNodes(ctx, tx, ns, freezeAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("FreezeOpenNodes froze %d nodes, want 1", n)
	}

	var state string
	var deadlineIsNull bool
	var remainingSeconds float64
	if err := db.Pool().QueryRow(ctx, `SELECT state, deadline IS NULL, EXTRACT(EPOCH FROM deadline_remaining) FROM declaration_nodes WHERE id=$1`, node.ID).
		Scan(&state, &deadlineIsNull, &remainingSeconds); err != nil {
		t.Fatal(err)
	}
	if state != NodeStateFrozen || !deadlineIsNull {
		t.Fatalf("after freeze: state=%q deadline_is_null=%v, want frozen/NULL", state, deadlineIsNull)
	}
	if remainingSeconds < 29*60 || remainingSeconds > 31*60 {
		t.Fatalf("deadline_remaining = %.0fs, want ~1800s (30m)", remainingSeconds)
	}

	// Excluded from ExpireDue while frozen, even well past the original
	// (now-cleared) deadline.
	expired, err := e.ExpireDue(ctx, ns, originalDeadline.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range expired {
		if ev.NodeID == node.ID {
			t.Fatalf("ExpireDue expired a frozen node: %+v", ev)
		}
	}
	stillFrozen, found, err := fb.NodeStatus(ctx, ns, node.ID)
	if err != nil || !found || stillFrozen.State != NodeStateFrozen {
		t.Fatalf("node after ExpireDue=%+v found=%v err=%v, want still frozen", stillFrozen, found, err)
	}

	// Thaw 10 minutes after the freeze: the resumed deadline must reflect
	// the 30 minutes that remained at freeze time, not the original
	// absolute deadline and not a fresh full 1h.
	thawAt := freezeAt.Add(10 * time.Minute)
	thawed, err := fb.ThawFrozenNodes(ctx, ns, thawAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(thawed) != 1 || thawed[0].ID != node.ID {
		t.Fatalf("ThawFrozenNodes = %+v, want exactly node %s", thawed, node.ID)
	}
	wantDeadline := thawAt.Add(30 * time.Minute)
	if diff := thawed[0].Deadline.Sub(wantDeadline); diff < -time.Second || diff > time.Second {
		t.Fatalf("resumed deadline = %s, want ~%s (thaw time + paused remaining)", thawed[0].Deadline, wantDeadline)
	}
	if thawed[0].State != NodeStateOpen {
		t.Fatalf("thawed node state = %q, want open", thawed[0].State)
	}

	// Not yet due just before the resumed deadline.
	notYet, err := e.ExpireDue(ctx, ns, wantDeadline.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range notYet {
		if ev.NodeID == node.ID {
			t.Fatalf("ExpireDue expired the resumed node early: %+v", ev)
		}
	}

	// Due once the resumed deadline actually passes.
	due, err := e.ExpireDue(ctx, ns, wantDeadline.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, ev := range due {
		if ev.NodeID == node.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("resumed node did not expire once its (paused-then-resumed) deadline passed: due=%+v", due)
	}
}
