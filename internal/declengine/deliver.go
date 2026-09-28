// deliver.go wires the declaration engine into the running control plane
// (task t38, #328, deviation d2). Tasks t9-t18 built the primitives -- the
// firing loop, the switch, the drain gate, freeze/replay, node lifecycle --
// and each left a wiring obligation. This file is where they meet the
// process:
//
//   - Router is the post-commit handler every signal delivery that offers
//     its fact to graph triggers also offers to declarations
//     (postgres.DeliveredEventHandler; the call site and why it is after the
//     commit are documented on that interface).
//   - Driver is the periodic half the scheduler process runs: node deadlines,
//     action.* results, human.decision / code.result reactions, and a
//     re-runnable thaw-and-replay.
//   - NewPostgres builds the production engine, its dispatcher behind the
//     switch's ShadowGate.
//
// Under the switch's rules (c80/h53): in 'before' the declaration engine
// records nothing -- the one thing it still does is store an event that
// targets a node frozen by a flip back (c81), which is not an evaluation;
// in 'shadow' it evaluates and records would-fire firings through
// ShadowGate, dispatching nothing; in 'after' ShadowGate hands the dispatch
// to WorkerDispatcher. The graph engine is never called from here, so its
// behaviour in 'before' and 'shadow' is untouched by construction.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// DefaultMarkerKeyEnv is the environment variable a control plane reads its
// origin-marker HMAC key from (Config.MarkerKeyEnv) when the declaration
// engine is enabled.
const DefaultMarkerKeyEnv = "NODES_DECLARATION_MARKER_KEY"

// RootNode is the shared root node a declaration whose trigger is an outside
// fact starts on (spec, q4: "Declarations whose trigger is an outside fact
// (jira.issue.created) start on a shared root node"). A delivered event
// starts there unless a verified origin marker derives the node from the
// landing node its parent firing opened (task t38d: a payload's `node`
// alone never moves it).
const RootNode = "root"

// EventFromSignal maps one delivered signal event onto the engine's Event.
// The payload is the event's variables (a non-object payload carries none).
// The event's Node is always RootNode here (task t38d, owner decision d6):
// an event without a verified origin marker arrives at root, whatever its
// payload names. Handle moves a verified reaction to its parent firing's
// landing node (deriveNode); a payload's "node" key is only a variable and
// never routes (task t38g: not even for a verified parent that opened no
// landing node -- that reaction is recorded, not fired). One payload key is
// read as routing:
//
//   - "origin": {marker, artifact_kind, artifact_id, author, bridge_account},
//     the stamped-artifact facts MarkerService.Resolve verifies before any
//     lineage is inherited. Absent, the event starts a fresh lineage.
//
// Emitter is the signal row's emitter (task t38g): Handle refuses a
// control-plane event name the control plane did not emit.
//
// Subject is the delivery's in-memory correlation key (never derived from the
// payload here, exactly as Event.Subject's doc comment requires).
func EventFromSignal(ev postgres.SignalEvent) Event {
	vars := map[string]any{}
	if err := json.Unmarshal(ev.Payload, &vars); err != nil || vars == nil {
		vars = map[string]any{}
	}
	var origin OriginEvent
	if o, ok := vars["origin"].(map[string]any); ok {
		text := func(k string) string { s, _ := o[k].(string); return s }
		origin = OriginEvent{Marker: text("marker"), ArtifactKind: text("artifact_kind"), ArtifactID: text("artifact_id"),
			Author: text("author"), BridgeAccount: text("bridge_account")}
	}
	return Event{NamespaceID: ev.NamespaceID, ID: ev.ID, Kind: ev.Name, Node: RootNode, Variables: vars, Origin: origin, Subject: ev.Subject, Emitter: ev.Emitter}
}

// Router is the declaration engine's postgres.DeliveredEventHandler.
type Router struct {
	Engine *Engine
	Switch SwitchStore
}

// HandleDeliveredEvent routes one committed delivery by the namespace's
// current switch mode. See this file's package comment for what each mode
// does; the dispatch-time mode read inside ShadowGate stays authoritative
// for whether an action is actually dispatched.
func (r Router) HandleDeliveredEvent(ctx context.Context, d postgres.SignalDelivery) error {
	if r.Engine == nil || r.Switch == nil {
		return errors.New("declengine: router needs an engine and a switch store")
	}
	event := EventFromSignal(d.Event)
	if event.NamespaceID == "" {
		return errors.New("declengine: delivered event has no namespace")
	}
	// The mode is read, and acted on, under the namespace's shared switch
	// lock, so no flip can commit between the read and the last write this
	// evaluation makes (switchlock.go). This runs after the delivery's own
	// commit; the lock never spans the delivery transaction.
	return holdShared(ctx, r.Switch, event.NamespaceID, func(ctx context.Context) error {
		mode, err := r.Switch.Mode(ctx, event.NamespaceID)
		if err != nil {
			return err
		}
		if mode == ModeBefore {
			_, err := r.Engine.StoreIfFrozen(ctx, event)
			return err
		}
		return r.Engine.Handle(ctx, event)
	})
}

// StoreIfFrozen is the whole of what the engine does with an event in
// 'before' (c81): when the event carries a marker that verifies to a firing
// whose landing node is frozen, the event is stored against that node for
// ThawAndReplay; otherwise nothing happens and nothing is recorded -- not
// even a marker rejection, because in 'before' an unverifiable marker is
// simply not the declaration engine's event. stored reports whether a new
// row was written.
func (e *Engine) StoreIfFrozen(ctx context.Context, event Event) (stored bool, err error) {
	if event.NamespaceID == "" || event.ID == "" || event.Origin.Marker == "" {
		return false, nil
	}
	nb, ok := e.backend.(NodeBackend)
	fb, ok2 := e.backend.(FreezeBackend)
	if !ok || !ok2 {
		return false, nil
	}
	event.Origin.NamespaceID, event.Origin.EventID, event.Origin.EventKind = event.NamespaceID, event.ID, event.Kind
	// Binding a completed run's artifact is not a dispatch -- it records what
	// an 'after'-era action already created -- so it runs in 'before' too,
	// even though ShadowGate's own PrepareOrigin skips outside 'after'. It is
	// the ONE write 'before' allows besides storing a frozen node's event
	// (review finding A3, kept by t38b; TestPostgresBeforeWritesOnlyTheBinding
	// AndTheStoredEvent pins that nothing else is written).
	if err := e.bindCompletedArtifact(ctx, event.Origin); err != nil {
		return false, err
	}
	parent, _, err := e.markers.verify(ctx, event.Origin)
	if err != nil || parent == "" {
		return false, err
	}
	node, found, err := nb.NodeByFiring(ctx, event.NamespaceID, parent)
	if err != nil || !found || node.State != NodeStateFrozen {
		return false, err
	}
	event.Node = node.Name
	return fb.StoreFrozenEvent(ctx, event.NamespaceID, node.ID, event)
}

type originPreparer interface {
	PrepareOrigin(context.Context, OriginEvent, *MarkerService) error
}

func (e *Engine) bindCompletedArtifact(ctx context.Context, origin OriginEvent) error {
	d := e.dispatcher
	if g, ok := d.(ShadowGate); ok {
		d = g.Underlying
	}
	if p, ok := d.(originPreparer); ok {
		return p.PrepareOrigin(ctx, origin, e.markers)
	}
	return nil
}

// NewPostgres builds the control plane's production engine: the Postgres
// backend and marker store, and WorkerDispatcher behind the switch's
// ShadowGate, so the one engine obeys before/shadow/after wherever it is
// called from. producerActorID empty selects DeclarationEngineActorID.
func NewPostgres(cfg Config, db *postgres.Store, producerActorID string) (*Engine, error) {
	if db == nil {
		return nil, errors.New("declengine: store required")
	}
	gate := ShadowGate{
		Switch:     PostgresSwitchStore{Store: db},
		Underlying: WorkerDispatcher{Store: db, ProducerActorID: producerActorID},
		GraphRuns:  PostgresGraphRunLookup{Store: db},
		ShadowLine: PostgresShadowLineageStore{Store: db},
	}
	return New(cfg, PostgresBackend{Store: db}, PostgresMarkerStore{Store: db}, gate)
}

// DefaultDriverBatch bounds how many nodes ExpireDue closes, and how many
// action results EmitActionResults emits, per namespace per tick.
const DefaultDriverBatch = 100

// Driver is the declaration engine's periodic half, run by the scheduler
// process (internal/scheduler Options.Declarations) under the scheduler's
// single-active advisory lock, on the scheduler's tick with the scheduler's
// caller-supplied clock. It also routes the scheduler's own schedule fires
// (HandleDeliveredEvent), so one value carries both obligations.
type Driver struct {
	Engine *Engine
	Store  *postgres.Store
	// Switch defaults to PostgresSwitchStore over Store.
	Switch SwitchStore
	// Batch defaults to DefaultDriverBatch.
	Batch int
}

func (d Driver) switchStore() SwitchStore {
	if d.Switch != nil {
		return d.Switch
	}
	return PostgresSwitchStore{Store: d.Store}
}

// HandleDeliveredEvent implements postgres.DeliveredEventHandler for the
// schedule fires the scheduler performs.
func (d Driver) HandleDeliveredEvent(ctx context.Context, delivery postgres.SignalDelivery) error {
	return Router{Engine: d.Engine, Switch: d.switchStore()}.HandleDeliveredEvent(ctx, delivery)
}

// Drive runs one bounded pass over every namespace that has declaration
// nodes, per its switch mode:
//
//   - 'before': nothing. Open nodes were frozen by the flip, their deadlines
//     paused; a run that fails meanwhile keeps its action.* emission for
//     later, because EmitActionResults (and EmitActionReactions) emit once
//     per run, not per tick.
//   - 'shadow': RecordSettledNodeTypes (t38d: settled runs' nodes get
//     their engine-derived types), ExpireDue, EmitActionResults, then
//     EmitActionReactions
//     (reactions.go: the human.decision / code.result reactions the control
//     plane stamps for human.ask and code.run), so shadow firings' nodes
//     expire exactly as real ones would and reactions are evaluated as
//     would-fire records.
//   - 'after': first ThawAndReplay when frozen nodes or stored events exist
//     -- this is what covers a crash between the forward flip's commit and
//     the route's own replay, and it is re-runnable -- then the same four.
//
// A failure in one namespace is joined, never stops the others.
func (d Driver) Drive(ctx context.Context, now time.Time) error {
	if d.Engine == nil || d.Store == nil {
		return errors.New("declengine: driver needs an engine and a store")
	}
	batch := d.Batch
	if batch <= 0 {
		batch = DefaultDriverBatch
	}
	rows, err := d.Store.Pool().Query(ctx, `SELECT DISTINCT namespace_id FROM declaration_nodes ORDER BY namespace_id`)
	if err != nil {
		return fmt.Errorf("declengine: driver: list namespaces: %w", err)
	}
	var namespaces []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			rows.Close()
			return err
		}
		namespaces = append(namespaces, ns)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, ns := range namespaces {
		if err := d.driveNamespace(ctx, ns, now, batch); err != nil {
			failures = append(failures, fmt.Errorf("namespace %s: %w", ns, err))
		}
	}
	return errors.Join(failures...)
}

// driveNamespace holds the namespace's shared switch lock for the whole
// pass, exactly as Router does for one delivery (switchlock.go).
func (d Driver) driveNamespace(ctx context.Context, ns string, now time.Time, batch int) error {
	return holdShared(ctx, d.switchStore(), ns, func(ctx context.Context) error {
		return d.driveNamespaceLocked(ctx, ns, now, batch)
	})
}

func (d Driver) driveNamespaceLocked(ctx context.Context, ns string, now time.Time, batch int) error {
	mode, err := d.switchStore().Mode(ctx, ns)
	if err != nil || mode == ModeBefore {
		return err
	}
	var failures []error
	if mode == ModeAfter {
		var pending bool
		if err := d.Store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM declaration_nodes WHERE namespace_id=$1 AND state=$2)
			OR EXISTS(SELECT 1 FROM declaration_node_frozen_events WHERE namespace_id=$1)`, ns, NodeStateFrozen).Scan(&pending); err != nil {
			return err
		}
		if pending {
			if err := ThawAndReplay(ctx, d.Engine, PostgresBackend{Store: d.Store}, ns, now); err != nil {
				failures = append(failures, err)
			}
		}
	}
	// t38d: settled runs' nodes get their engine-derived types recorded
	// before any action result or reaction to them is evaluated.
	if _, err := d.Engine.RecordSettledNodeTypes(ctx, ns); err != nil {
		failures = append(failures, err)
	}
	if _, err := d.Engine.ExpireDue(ctx, ns, now, batch); err != nil {
		failures = append(failures, err)
	}
	if _, err := d.Engine.EmitActionResults(ctx, ns, batch); err != nil {
		failures = append(failures, err)
	}
	// t38c: the control plane stamps human.ask and code.run (reactions.go).
	if _, err := d.Engine.EmitActionReactions(ctx, ns, batch); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

var (
	_ postgres.DeliveredEventHandler = Router{}
	_ postgres.DeliveredEventHandler = Driver{}
)
