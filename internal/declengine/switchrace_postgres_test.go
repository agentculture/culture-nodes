package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t38b (#328): the defects the t38 review found, each driven
// deterministically against PostgreSQL. The flip races (A1/A2) interleave a
// handler and a flip through pausingSwitch -- a channel handshake, never a
// sleep -- and detect "the flip is waiting on the handler" by reading
// pg_locks, not by timing.

// pausingSwitch is the router's switch store with one seam: the FIRST Mode
// read reports what it read on read, then blocks until release is closed.
// Every other method (Flip, the switch lock) is PostgresSwitchStore's own.
type pausingSwitch struct {
	PostgresSwitchStore
	read    chan string
	release chan struct{}
	once    sync.Once
}

func newPausingSwitch(db *postgres.Store) *pausingSwitch {
	return &pausingSwitch{PostgresSwitchStore: PostgresSwitchStore{Store: db}, read: make(chan string, 1), release: make(chan struct{})}
}

func (p *pausingSwitch) Mode(ctx context.Context, ns string) (string, error) {
	mode, err := p.PostgresSwitchStore.Mode(ctx, ns)
	first := false
	p.once.Do(func() { first = true })
	if first {
		p.read <- mode
		<-p.release
	}
	return mode, err
}

// flipAndReplay is the production flip path the API route runs: the flip
// (freezing on 'before'), then the replay on 'after'.
func flipAndReplay(ctx context.Context, db *postgres.Store, e *Engine, ns, mode string) error {
	_, replayErr, err := FlipSwitch(ctx, PostgresSwitchStore{Store: db}, e, ns, mode, "human:ops", "race", time.Now)
	return errors.Join(err, replayErr)
}

// waitFlipBlockedOrDone returns once the flip has either finished or is
// waiting in pg_locks for the namespace's switch lock -- whichever the code
// under test makes true. It polls a condition; it never sleeps a guess.
func waitFlipBlockedOrDone(t *testing.T, db *postgres.Store, ns string, done <-chan error) (finished bool, err error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return true, err
		default:
		}
		var waiting int
		if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l, (SELECT hashtextextended($1,0) AS k) key
			WHERE l.locktype='advisory' AND NOT l.granted AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND l.classid::bigint=((key.k>>32)&4294967295) AND l.objid::bigint=(key.k&4294967295)`, switchLockKey(ns)).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("flip neither finished nor waited on the switch lock")
	return false, nil
}

func raceEngine(t *testing.T, db *postgres.Store, env string) *Engine {
	t.Helper()
	t.Setenv(env, strings.Repeat("r", 32))
	e, err := New(Config{MarkerKeyEnv: env}, PostgresBackend{db}, PostgresMarkerStore{db},
		ShadowGate{Switch: PostgresSwitchStore{Store: db}, Underlying: &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type deliveryResult struct {
	d   postgres.SignalDelivery
	err error
}

func deliverAsync(db *postgres.Store, ns string, payload json.RawMessage, h postgres.DeliveredEventHandler) <-chan deliveryResult {
	out := make(chan deliveryResult, 1)
	go func() {
		d, err := db.DeliverSignalEvent(context.Background(), postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "timer", Payload: payload, Emitter: "test", Declarations: h})
		out <- deliveryResult{d, err}
	}()
	return out
}

// A1: a handler that read 'after' and a flip to 'before' that lands while it
// is still evaluating. The flip must wait for the evaluation, so the node
// the evaluation opens is one the flip then freezes -- never an 'open' node
// written after the switch already said 'before'.
func TestPostgresFlipToBeforeWaitsForAnInFlightEvaluation(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-race-a1").ID
	va := publishActive(t, db, ns, declFor("race-a1", RootNode, "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`))
	e := raceEngine(t, db, "TCA_RACE_A1_KEY")
	if _, err := (PostgresSwitchStore{Store: db}).Flip(ctx, ns, ModeAfter, "human:ops", "start"); err != nil {
		t.Fatal(err)
	}

	sw := newPausingSwitch(db)
	handled := deliverAsync(db, ns, json.RawMessage(`{}`), Router{Engine: e, Switch: sw})
	if mode := <-sw.read; mode != ModeAfter {
		t.Fatalf("router read %q, want %q", mode, ModeAfter)
	}
	flipped := make(chan error, 1)
	go func() { flipped <- flipAndReplay(ctx, db, e, ns, ModeBefore) }()
	finished, flipErr := waitFlipBlockedOrDone(t, db, ns, flipped)
	close(sw.release)
	if finished {
		t.Errorf("the flip committed (err=%v) while an evaluation that had read the old mode was still in flight", flipErr)
	}
	r := <-handled
	if r.err != nil || r.d.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", r.err, r.d.DeclarationErr)
	}
	if !finished {
		flipErr = <-flipped
	}
	if flipErr != nil {
		t.Fatal(flipErr)
	}

	var open int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_nodes WHERE namespace_id=$1 AND state='open'`, ns).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("%d declaration nodes are 'open' after the flip to 'before' committed, want 0 (all frozen)", open)
	}
	fa := firingByEventDecl(t, db, ns, r.d.Event.ID, va.DeclarationID)
	node, found, err := (PostgresBackend{db}).NodeByFiring(ctx, ns, fa.ID)
	if err != nil || !found || node.State != NodeStateFrozen {
		t.Fatalf("the in-flight evaluation's node = %+v found=%v err=%v, want frozen by the flip", node, found, err)
	}
	if got, _ := terminalOutcome(t, db, ns, r.d.Event.ID, va.DeclarationID); got != OutcomeFired {
		t.Fatalf("the evaluation that began in 'after' ended %q, want %q (it ran wholly in 'after')", got, OutcomeFired)
	}
}

// A2: a handler that read 'before' and a flip to 'after' that lands while it
// is still deciding whether to store a reaction for a frozen node. The flip
// must wait, so the reaction is stored and then replayed -- never lost
// between "not evaluated because 'before'" and "not stored because thawed".
func TestPostgresFlipToAfterWaitsSoAReactionIsNotLost(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-race-a2").ID
	va := publishActive(t, db, ns, declFor("race-a2-a", RootNode, "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`))
	vb := publishActive(t, db, ns, declFor("race-a2-b", "waiting", "none", "done", "none", "timer", `{"uses":"actor://test"}`))
	e := raceEngine(t, db, "TCA_RACE_A2_KEY")
	plain := Router{Engine: e, Switch: PostgresSwitchStore{Store: db}}
	if err := flipAndReplay(ctx, db, e, ns, ModeAfter); err != nil {
		t.Fatal(err)
	}
	first := <-deliverAsync(db, ns, json.RawMessage(`{}`), plain)
	if first.err != nil || first.d.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", first.err, first.d.DeclarationErr)
	}
	fa := firingByEventDecl(t, db, ns, first.d.Event.ID, va.DeclarationID)
	if err := flipAndReplay(ctx, db, e, ns, ModeBefore); err != nil {
		t.Fatal(err)
	}

	sw := newPausingSwitch(db)
	handled := deliverAsync(db, ns, originPayload(t, reactEvent(t, db, ns, "placeholder", fa, "timer", nil)), Router{Engine: e, Switch: sw})
	if mode := <-sw.read; mode != ModeBefore {
		t.Fatalf("router read %q, want %q", mode, ModeBefore)
	}
	flipped := make(chan error, 1)
	go func() { flipped <- flipAndReplay(ctx, db, e, ns, ModeAfter) }()
	finished, flipErr := waitFlipBlockedOrDone(t, db, ns, flipped)
	close(sw.release)
	if finished {
		t.Errorf("the flip committed (err=%v) while an evaluation that had read the old mode was still in flight", flipErr)
	}
	r := <-handled
	if r.err != nil || r.d.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", r.err, r.d.DeclarationErr)
	}
	if !finished {
		flipErr = <-flipped
	}
	if flipErr != nil {
		t.Fatal(flipErr)
	}

	if n := firingsFor(t, db, ns, r.d.Event.ID); n != 1 {
		t.Fatalf("the reaction delivered across the flip fired %d times, want exactly 1 (stored, then replayed)", n)
	}
	if got, _ := terminalOutcome(t, db, ns, r.d.Event.ID, vb.DeclarationID); got != OutcomeFired {
		t.Fatalf("reaction outcome = %q, want %q", got, OutcomeFired)
	}
}

// failingActive is PostgresBackend with a switchable transient failure in
// Active -- the first read Handle makes after resolving an event's origin.
type failingActive struct {
	PostgresBackend
	fail *bool
}

func (f failingActive) Active(ctx context.Context, ns string) ([]ActiveDeclaration, error) {
	if *f.fail {
		return nil, errors.New("transient: active declarations unavailable")
	}
	return f.PostgresBackend.Active(ctx, ns)
}

func deferralCount(t *testing.T, db *postgres.Store, ns string) (rows, attempts int) {
	t.Helper()
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*),COALESCE(SUM(attempts),0) FROM declaration_subject_deferrals WHERE namespace_id=$1`, ns).Scan(&rows, &attempts); err != nil {
		t.Fatal(err)
	}
	return rows, attempts
}

// D1: a drain whose replay fails keeps the deferral, so a later drain still
// fires it; a replay that is itself deferred again keeps its queue entry.
func TestPostgresDrainSubjectKeepsTheDeferralUntilItsReplaySucceeds(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-drain-keep").ID
	a := declFor("dk-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`)
	a.Trigger.MaxConcurrentSubject = 1
	va := publishActive(t, db, ns, a)
	fail := false
	t.Setenv("TCA_DRAIN_KEEP_KEY", strings.Repeat("k", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_DRAIN_KEEP_KEY"}, failingActive{PostgresBackend{db}, &fail}, PostgresMarkerStore{db},
		&chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	first, second := deliver(t, db, ns), deliver(t, db, ns)
	for _, id := range []string{first, second} {
		if err := e.Handle(ctx, Event{NamespaceID: ns, ID: id, Kind: "timer", Node: "intake", Subject: "ISSUE-9"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := terminalOutcome(t, db, ns, second, va.DeclarationID); got != OutcomeDeferred {
		t.Fatalf("second event = %q, want deferred", got)
	}

	// The slot is still taken: the replay defers again, and the queue entry
	// it re-points must survive the drain that replayed it.
	if err := e.DrainSubject(ctx, ns, va.DeclarationID); err != nil {
		t.Fatal(err)
	}
	if rows, attempts := deferralCount(t, db, ns); rows != 1 || attempts != 2 {
		t.Fatalf("after a replay that deferred again: rows=%d attempts=%d, want the one entry kept (attempts 2)", rows, attempts)
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE declaration_nodes SET state='closed' WHERE namespace_id=$1`, ns); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := e.DrainSubject(ctx, ns, va.DeclarationID); err == nil {
		t.Fatal("DrainSubject with a failing replay returned nil, want the replay's error")
	}
	if rows, _ := deferralCount(t, db, ns); rows != 1 {
		t.Fatalf("a failed replay left %d deferrals, want the entry kept (1)", rows)
	}
	fail = false
	if err := e.DrainSubject(ctx, ns, va.DeclarationID); err != nil {
		t.Fatal(err)
	}
	if got, _ := terminalOutcome(t, db, ns, second, va.DeclarationID); got != OutcomeFired {
		t.Fatalf("after the retried drain the deferred event = %q, want fired", got)
	}
	if rows, _ := deferralCount(t, db, ns); rows != 0 {
		t.Fatalf("a successful replay left %d deferrals, want 0", rows)
	}
}

// A4: a shadow firing records its would-fire trail but spends no real
// budget, so it can never budget-block a real firing after the flip.
func TestPostgresShadowFiringSpendsNoRealBudget(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-shadow-budget").ID
	va := publishActive(t, db, ns, declFor("sb-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`))
	insertDeclarationBudget(t, db, ns, "node", "waiting", intp(1), nil)
	e := raceEngine(t, db, "TCA_SHADOW_BUDGET_KEY")
	sw := PostgresSwitchStore{Store: db}
	if _, err := sw.Flip(ctx, ns, ModeShadow, "human:ops", "shadow"); err != nil {
		t.Fatal(err)
	}
	shadowed := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: shadowed, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := terminalOutcome(t, db, ns, shadowed, va.DeclarationID); got != OutcomeShadow {
		t.Fatalf("shadow firing = %q, want %q", got, OutcomeShadow)
	}
	var spent int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_budget_spend WHERE namespace_id=$1`, ns).Scan(&spent); err != nil || spent != 0 {
		t.Fatalf("shadow wrote %d budget spend rows (err=%v), want 0", spent, err)
	}
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "after"); err != nil {
		t.Fatal(err)
	}
	realEv := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: realEv, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	if got, reason := terminalOutcome(t, db, ns, realEv, va.DeclarationID); got != OutcomeFired {
		t.Fatalf("first real firing after shadow = %q (%s), want %q", got, reason, OutcomeFired)
	}
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_budget_spend WHERE namespace_id=$1`, ns).Scan(&spent); err != nil || spent != 1 {
		t.Fatalf("real firing wrote %d budget spend rows (err=%v), want 1", spent, err)
	}
}

// namespaceSnapshot digests every row of every table that has a
// namespace_id column, per table, for ns -- inserts, deletes and updates all
// change a digest.
func namespaceSnapshot(t *testing.T, db *postgres.Store, ns string) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Pool().Query(ctx, `SELECT table_name FROM information_schema.columns
		WHERE table_schema='public' AND column_name='namespace_id' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	out := map[string]string{}
	for _, table := range tables {
		var digest string
		if err := db.Pool().QueryRow(ctx, `SELECT COALESCE(md5(string_agg(t::text, '|' ORDER BY t::text)),'') FROM `+pgxIdent(table)+` t WHERE namespace_id=$1`, ns).Scan(&digest); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		out[table] = digest
	}
	return out
}

func pgxIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// A3, kept: in 'before', a reaction to a frozen node whose parent run has
// completed binds that run's artifact to the minted marker (what the
// 'after'-era action already created) and stores the event for replay.
// Nothing else in the namespace is written.
func TestPostgresBeforeWritesOnlyTheBindingAndTheStoredEvent(t *testing.T) {
	db, firing, e, _, ns := workerFiring(t, ledger.AuthorityProposed)
	ctx := context.Background()
	var nonce, mac string
	var bound *string
	if err := db.Pool().QueryRow(ctx, `SELECT nonce,mac,artifact_id FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, ns, firing).Scan(&nonce, &mac, &bound); err != nil {
		t.Fatal(err)
	}
	if bound != nil {
		t.Fatalf("marker already bound to %q before the reaction; the fixture must leave it unbound", *bound)
	}
	if _, replayErr, err := FlipSwitch(ctx, PostgresSwitchStore{Store: db}, e, ns, ModeBefore, "human:ops", "rollback", time.Now); err != nil || replayErr != nil {
		t.Fatalf("flip: err=%v replayErr=%v", err, replayErr)
	}
	before := namespaceSnapshot(t, db, ns)
	// The delivery itself writes one signal_events row and one outbox row
	// (postgres.TypeSignalDelivered); those tables are checked by count below.
	deliveryOwned := func() (events, otherOutbox int) {
		t.Helper()
		if err := db.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM signal_events WHERE namespace_id=$1),
			(SELECT count(*) FROM outbox WHERE namespace_id=$1 AND topic<>$2)`, ns, postgres.TypeSignalDelivered).Scan(&events, &otherOutbox); err != nil {
			t.Fatal(err)
		}
		return events, otherOutbox
	}
	eventsBefore, outboxBefore := deliveryOwned()

	payload, err := json.Marshal(map[string]any{"origin": map[string]string{
		"marker": "cn1:" + firing + ":github.pr:" + nonce + ":" + mac, "artifact_kind": "github.pr", "artifact_id": "pr-42"}})
	if err != nil {
		t.Fatal(err)
	}
	r := <-deliverAsync(db, ns, payload, Router{Engine: e, Switch: PostgresSwitchStore{Store: db}})
	if r.err != nil || r.d.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", r.err, r.d.DeclarationErr)
	}

	after := namespaceSnapshot(t, db, ns)
	deliveryTables := map[string]bool{"signal_events": true, "outbox": true}
	engineWrites := map[string]bool{
		"declaration_minted_markers":     true, // the binding
		"declaration_node_frozen_events": true, // the stored event
	}
	for table, digest := range after {
		if before[table] != digest && !engineWrites[table] && !deliveryTables[table] {
			t.Errorf("'before' wrote to %s; only the artifact binding and the stored event are allowed", table)
		}
	}
	for table := range engineWrites {
		if before[table] == after[table] {
			t.Errorf("%s unchanged; the reaction should have written it", table)
		}
	}
	if events, otherOutbox := deliveryOwned(); events != eventsBefore+1 || otherOutbox != outboxBefore {
		t.Errorf("signal_events %d->%d and non-delivery outbox rows %d->%d: want exactly the delivery's own event and nothing else",
			eventsBefore, events, outboxBefore, otherOutbox)
	}
	if err := db.Pool().QueryRow(ctx, `SELECT artifact_id FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, ns, firing).Scan(&bound); err != nil || bound == nil || *bound != "pr-42" {
		t.Fatalf("marker binding = %v err=%v, want pr-42", bound, err)
	}
}
