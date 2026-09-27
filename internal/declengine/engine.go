// Package declengine evaluates trigger-condition-action declarations
// alongside the existing graph engine (internal/engine), which it neither
// calls into nor changes: the two coexist until the graph engine retires.
//
// One event runs the firing loop for every active declaration it matches:
// trigger match -> lineage check -> CEL condition -> dispatch through the
// existing worker/actor/runner path -> the landing node opened. Each step
// appends a declaration_evaluations row, so "why did (or didn't) it fire"
// is always answerable from records rather than logs.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/kinds"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Evaluation outcomes the firing loop records, in pipeline order. The
// terminal ones (every outcome but matched, lineage checked, condition true
// and dispatching) end one declaration's evaluation of one event.
const (
	OutcomeMatched         = "matched"
	OutcomeLineageMissing  = "lineage missing"
	OutcomeLineageChecked  = "lineage checked"
	OutcomeLoopLimited     = "loop-limited"
	OutcomeConditionError  = "condition error"
	OutcomeConditionFalse  = "condition false"
	OutcomeConditionTrue   = "condition true"
	OutcomeEvaluationError = "evaluation failed"
	OutcomeDuplicate       = "duplicate"
	OutcomeDispatching     = "dispatching"
	OutcomeDispatchFailed  = "dispatch failed"
	OutcomeFired           = "fired"
	// OutcomeDeferred is task t10's per-subject concurrency backstop
	// (spec c84/h57): the declaration's max_concurrent_subject cap was
	// already met for this event's subject, so it was queued instead of
	// claimed -- never dropped, and never counted as a firing.
	OutcomeDeferred = "deferred"
)

// Config carries the deployment's engine settings. MarkerKeyEnv names the
// environment variable holding the origin-marker HMAC key; the key itself
// is never a constant in code and is never persisted with a firing. New
// refuses to start when the variable holds fewer than 32 bytes.
type Config struct {
	MarkerKeyEnv string
}

// ActiveDeclaration is the version of one declaration currently activated
// in a namespace, with the ordering links declared from it.
type ActiveDeclaration struct {
	ID, VersionID string
	Declaration   decl.Declaration
	Links         []postgres.DeclarationLink
}

// Event is an already ingested signal event. Origin is verified before any
// ancestry is used. Authority is deliberately absent: receiving an event here
// cannot upgrade the emitter's evidence or any action's proposal.
type Event struct {
	NamespaceID, ID, Kind, Node string
	Variables                   map[string]any
	Origin                      OriginEvent
	// Subject is an optional, caller-supplied correlation key (task t10,
	// spec c84/h57) -- e.g. a Jira issue key or a PR identity -- mirroring
	// the graph engine's SignalEvent.Subject/runs.subject. It is never
	// derived from Variables here: the caller (whatever routes an event to
	// Handle) decides it, exactly as TriggerEvent's caller does today.
	Subject string
}

// Evaluation is one recorded step of one declaration's evaluation of one
// event. Reason is prose for a reader. Variables is set only on the fired
// evaluation: what this firing exposes to later declarations in its lineage.
type Evaluation struct {
	NamespaceID, EventID, DeclarationID, VersionID, FiringID, Outcome, Reason string
	Variables                                                                 map[string]any
}

// Landing opens the declaration's landing node for a fired firing.
// Deadline is zero for a node declared with deadline "none".
type Landing struct {
	NamespaceID, FiringID string
	Node                  decl.Node
	Deadline              time.Duration
}

// Backend implementations must claim atomically across processes, and scope
// every read and write to the namespace. Lineage returns causal ancestors
// only, nearest first, one entry per logical firing (re-mints collapsed).
type Backend interface {
	Active(context.Context, string) ([]ActiveDeclaration, error)
	Lineage(context.Context, string, string) ([]Ancestor, error)
	Claim(context.Context, postgres.DeclarationFiringInput) (postgres.DeclarationFiring, bool, error)
	Record(context.Context, Evaluation) error
	Finish(context.Context, Landing, Evaluation) error
	// RecentFirings counts a declaration's logical (non-remint) firings
	// created at or after since -- the rate-ceiling backstop (c93).
	RecentFirings(ctx context.Context, namespaceID, declarationID string, since time.Time) (int, error)
	// SubjectInFlight counts a declaration's canonical firings for one
	// subject whose landing node is still open -- the per-subject
	// concurrency ceiling (c84/h57).
	SubjectInFlight(ctx context.Context, namespaceID, declarationID, subject string) (int, error)
	// DeferSubject queues (or, per the replace rule, re-points) the one
	// remembered entry for a (declaration, subject) whose concurrency cap
	// left no room for event.
	DeferSubject(ctx context.Context, in DeferSubjectInput) error
	// OldestDeferredSubject returns the longest-queued deferred entry for a
	// declaration, across every subject, for DrainSubject to replay.
	OldestDeferredSubject(ctx context.Context, namespaceID, declarationID string) (DeferredSubject, bool, error)
	// DeleteDeferredSubject removes a drained (or superseded) entry.
	DeleteDeferredSubject(ctx context.Context, namespaceID, id string) error
}

// Engine is the declaration firing loop. It holds no per-event state.
type Engine struct {
	backend    Backend
	markers    *MarkerService
	dispatcher Dispatcher
}

// New builds an engine whose marker key is read from cfg.MarkerKeyEnv.
func New(cfg Config, backend Backend, markers MarkerStore, dispatcher Dispatcher) (*Engine, error) {
	if cfg.MarkerKeyEnv == "" || backend == nil || dispatcher == nil {
		return nil, errors.New("declengine: marker key environment variable, backend and dispatcher required")
	}
	m, err := NewMarkerService([]byte(os.Getenv(cfg.MarkerKeyEnv)), markers)
	if err != nil {
		return nil, fmt.Errorf("declengine: %s: %w", cfg.MarkerKeyEnv, err)
	}
	return &Engine{backend: backend, markers: m, dispatcher: dispatcher}, nil
}

// Handle runs the firing loop for one event against every active
// declaration in its namespace. A failure in one declaration is recorded
// and joined into the returned error; it never stops the others.
func (e *Engine) Handle(ctx context.Context, event Event) error {
	if event.NamespaceID == "" || event.ID == "" || event.Kind == "" || event.Node == "" {
		return errors.New("declengine: event identity, kind and node required")
	}
	event.Origin.NamespaceID, event.Origin.EventID = event.NamespaceID, event.ID
	// A reaction can arrive before the worker has bound the artifact its
	// action created; a dispatcher that can bind it first gets the chance.
	if preparer, ok := e.dispatcher.(interface {
		PrepareOrigin(context.Context, OriginEvent, *MarkerService) error
	}); ok {
		if err := preparer.PrepareOrigin(ctx, event.Origin, e.markers); err != nil {
			return err
		}
	}
	parent, err := e.markers.Resolve(ctx, event.Origin)
	if err != nil {
		return err
	}
	var ancestry []Ancestor
	if parent != "" {
		ancestry, err = e.backend.Lineage(ctx, event.NamespaceID, parent)
		if err != nil {
			return err
		}
	}
	active, err := e.backend.Active(ctx, event.NamespaceID)
	if err != nil {
		return err
	}
	var failures []error
	for _, a := range active {
		matched, err := matches(a.Declaration, event)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !matched {
			continue
		}
		if err := e.evaluate(ctx, event, a, parent, ancestry); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) evaluate(ctx context.Context, event Event, a ActiveDeclaration, parent string, ancestors []Ancestor) error {
	evaluation := Evaluation{NamespaceID: event.NamespaceID, EventID: event.ID, DeclarationID: a.ID, VersionID: a.VersionID}
	record := func(outcome, reason string) error {
		evaluation.Outcome, evaluation.Reason = outcome, reason
		return e.backend.Record(ctx, evaluation)
	}
	fail := func(outcome string, err error) error {
		return errors.Join(err, record(outcome, err.Error()))
	}
	if err := record(OutcomeMatched, "active trigger and start node matched"); err != nil {
		return err
	}
	lineage := ResolveLineage(ancestors)
	ok, err := checkLinks(a.Links, lineage)
	if err != nil {
		return fail(OutcomeEvaluationError, err)
	}
	if !ok {
		return record(OutcomeLineageMissing, "a declaration this one must appear after is absent from the causal lineage")
	}
	if err := record(OutcomeLineageChecked, "every required predecessor is in the causal lineage"); err != nil {
		return err
	}
	// Re-entry counts logical firings: ResolveLineage has already collapsed
	// re-mints onto their canonical entry. N re-entries is N+1 appearances.
	count := 0
	for _, ancestor := range lineage {
		if ancestor.DeclarationID == a.ID {
			count++
		}
	}
	if count > a.Declaration.Trigger.ReentryLimit {
		return record(OutcomeLoopLimited, fmt.Sprintf("declaration already appears %d times in its lineage; re-entry limit is %d", count, a.Declaration.Trigger.ReentryLimit))
	}
	// The c93 backstops: hop limit and self-retrigger read only the
	// resolved lineage already in hand; the rate ceiling is the one guard
	// check that needs a backend read. All three share the loop-limited
	// outcome with the re-entry check above -- t13's explain surface reads
	// outcome+reason, not a separate kind per limit.
	if ok, reason := checkHopLimit(a, lineage); !ok {
		return record(OutcomeLoopLimited, reason)
	}
	if ok, reason := checkSelfRetrigger(a, lineage); !ok {
		return record(OutcomeLoopLimited, reason)
	}
	rateOK, reason, err := e.checkRateCeiling(ctx, event.NamespaceID, a)
	if err != nil {
		return fail(OutcomeEvaluationError, err)
	}
	if !rateOK {
		return record(OutcomeLoopLimited, reason)
	}
	ok, err = condition(a.Declaration.Condition, event.Variables, lineage)
	if err != nil {
		return fail(OutcomeConditionError, err)
	}
	if !ok {
		return record(OutcomeConditionFalse, "condition evaluated to false")
	}
	if err := record(OutcomeConditionTrue, "condition evaluated to true"); err != nil {
		return err
	}
	// The subject concurrency cap (c84/h57) is checked only once a firing
	// would otherwise happen: a false condition should never spend a
	// subject's in-flight slot or queue a replay for nothing.
	deferred, err := e.checkSubjectConcurrency(ctx, event, a)
	if err != nil {
		return fail(OutcomeEvaluationError, err)
	}
	if deferred {
		return record(OutcomeDeferred, fmt.Sprintf("per-subject concurrency cap %d reached for subject %q; queued to run when a slot frees", a.Declaration.Trigger.MaxConcurrentSubject, event.Subject))
	}
	// Everything that can reject the firing is checked before it is
	// claimed, so a dispatched action always has a node to land on.
	action, err := renderAction(a.Declaration.Action, event.Variables, lineage)
	if err != nil {
		return fail(OutcomeEvaluationError, err)
	}
	kind, ok := kinds.Action(action.Kind)
	if !ok || len(kind.Produces) == 0 {
		return fail(OutcomeEvaluationError, fmt.Errorf("declengine: unregistered action %q", action.Kind))
	}
	deadline, err := landingDeadline(a.Declaration.LandingNode)
	if err != nil {
		return fail(OutcomeEvaluationError, err)
	}
	in, err := firingInput(event, a, parent)
	if err != nil {
		return fail(OutcomeEvaluationError, err)
	}
	firing, created, err := e.backend.Claim(ctx, in)
	if err != nil {
		return err
	}
	evaluation.FiringID = firing.ID
	if !created {
		return record(OutcomeDuplicate, "this event already fired this declaration")
	}
	marker, err := e.markers.Mint(ctx, event.NamespaceID, firing.ID, string(kind.Produces[0]))
	if err != nil {
		return fail(OutcomeDispatchFailed, err)
	}
	if err := record(OutcomeDispatching, "firing claimed with its component digests pinned"); err != nil {
		return err
	}
	result, err := e.dispatcher.Dispatch(ctx, DispatchRequest{Firing: firing, Declaration: a.Declaration, Action: action, Marker: marker})
	if err != nil {
		return fail(OutcomeDispatchFailed, err)
	}
	if result.ArtifactID != "" {
		if err := e.markers.RecordArtifact(ctx, event.NamespaceID, marker, result.ArtifactID); err != nil {
			return fail(OutcomeDispatchFailed, err)
		}
	}
	// A firing exposes its trigger's variables, overlaid by whatever the
	// action returned synchronously. An asynchronous action's output is read
	// from its worker run when the lineage is resolved.
	vars := map[string]any{}
	for k, v := range event.Variables {
		vars[k] = v
	}
	for k, v := range result.Variables {
		vars[k] = v
	}
	evaluation.Variables = vars
	evaluation.Outcome, evaluation.Reason = OutcomeFired, "action dispatched and landing node "+a.Declaration.LandingNode.Name+" opened"
	return e.backend.Finish(ctx, Landing{NamespaceID: event.NamespaceID, FiringID: firing.ID, Node: a.Declaration.LandingNode, Deadline: deadline}, evaluation)
}

// firingInput pins the exact trigger, condition and action this firing
// evaluated. The digests are over the declared (unrendered) components, so
// two firings of one version always pin the same three digests.
func firingInput(event Event, a ActiveDeclaration, parent string) (postgres.DeclarationFiringInput, error) {
	in := postgres.DeclarationFiringInput{NamespaceID: event.NamespaceID, EventID: event.ID, DeclarationID: a.ID, DeclarationVersion: a.VersionID, ParentFiringID: parent, Subject: event.Subject}
	for _, c := range []struct {
		v   any
		dst *string
	}{{a.Declaration.Trigger, &in.TriggerDigest}, {a.Declaration.Condition, &in.ConditionDigest}, {a.Declaration.Action, &in.ActionDigest}} {
		b, err := contracts.CanonicalJSON(c.v)
		if err != nil {
			return in, err
		}
		*c.dst = contracts.Digest(b)
	}
	return in, nil
}

// landingDeadline parses a node deadline: "none", or a positive Go duration.
func landingDeadline(n decl.Node) (time.Duration, error) {
	if n.Deadline == "none" {
		return 0, nil
	}
	d, err := time.ParseDuration(n.Deadline)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("declengine: landing node %q has invalid deadline %q", n.Name, n.Deadline)
	}
	return d, nil
}

// PostgresBackend adapts the 0059 catalog and the 0060 firing and
// evaluation tables.
type PostgresBackend struct{ Store *postgres.Store }

// Active returns, per declaration, the version its newest activation or
// deactivation names, when that entry is an activation.
func (p PostgresBackend) Active(ctx context.Context, ns string) ([]ActiveDeclaration, error) {
	rows, err := p.Store.Pool().Query(ctx, `SELECT v.declaration_id,v.id,v.body FROM declaration_versions v
 JOIN (SELECT DISTINCT ON (v.declaration_id) v.declaration_id,h.target_version_id,h.kind
 FROM declaration_history h JOIN declaration_versions v ON v.namespace_id=h.namespace_id AND v.id=h.target_version_id
 WHERE h.namespace_id=$1 AND h.kind IN ('activate','deactivate') ORDER BY v.declaration_id,h.seq DESC) h
 ON h.target_version_id=v.id WHERE v.namespace_id=$1 AND h.kind='activate' ORDER BY v.declaration_id`, ns)
	if err != nil {
		return nil, err
	}
	var out []ActiveDeclaration
	for rows.Next() {
		var a ActiveDeclaration
		var body []byte
		if err := rows.Scan(&a.ID, &a.VersionID, &body); err != nil {
			rows.Close()
			return nil, err
		}
		d, err := decl.Parse(body, decl.FormatJSON)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("declengine: active version %s: %w", a.VersionID, err)
		}
		a.Declaration = *d
		out = append(out, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Links, err = p.Store.ListDeclarationLinks(ctx, ns, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Claim records the firing; the 0060 unique index makes it once per event.
func (p PostgresBackend) Claim(ctx context.Context, in postgres.DeclarationFiringInput) (postgres.DeclarationFiring, bool, error) {
	return p.Store.RecordDeclarationFiring(ctx, in)
}

// Record appends one evaluation step.
func (p PostgresBackend) Record(ctx context.Context, e Evaluation) error {
	_, err := p.Store.Pool().Exec(ctx, `INSERT INTO declaration_evaluations(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason,firing_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`,
		store.NewULID(), e.NamespaceID, e.EventID, e.DeclarationID, e.VersionID, e.Outcome, e.Reason, e.FiringID)
	return err
}

// Finish opens the landing node and records the fired evaluation in one
// transaction, at most once per firing.
func (p PostgresBackend) Finish(ctx context.Context, l Landing, evaluation Evaluation) error {
	var deadline *time.Time
	if l.Deadline > 0 {
		t := time.Now().UTC().Add(l.Deadline)
		deadline = &t
	}
	vars, err := json.Marshal(evaluation.Variables)
	if err != nil {
		return err
	}
	tx, err := p.Store.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize completion so a repeated finish never opens two nodes.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "decl-finish:"+l.NamespaceID+":"+l.FiringID); err != nil {
		return err
	}
	var done bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM declaration_evaluations WHERE namespace_id=$1 AND firing_id=$2 AND outcome='fired')`, l.NamespaceID, l.FiringID).Scan(&done); err != nil {
		return err
	}
	if done {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO declaration_nodes(id,namespace_id,opening_firing_id,node_name,deadline) VALUES($1,$2,$3,$4,$5)`, store.NewULID(), l.NamespaceID, l.FiringID, l.Node.Name, deadline); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO declaration_evaluations(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason,firing_id,variables)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		store.NewULID(), evaluation.NamespaceID, evaluation.EventID, evaluation.DeclarationID, evaluation.VersionID, OutcomeFired, evaluation.Reason, evaluation.FiringID, vars); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Lineage walks lineage edges back from parent. Walking edges, rather than
// every row sharing a lineage_id, excludes sibling fan-out branches. UNION
// deduplicates ids, which also terminates a corrupt cycle. Edges point at
// canonical entries, so each logical firing appears once; its variables come
// from the newest fired evaluation among its physical firings, overlaid by
// that firing's completed worker run output.
func (p PostgresBackend) Lineage(ctx context.Context, ns, parent string) ([]Ancestor, error) {
	rows, err := p.Store.Pool().Query(ctx, `WITH RECURSIVE ancestors(id) AS (
 SELECT canonical_firing_id FROM declaration_firings WHERE namespace_id=$1 AND id=$2
 UNION SELECT e.parent_firing_id FROM declaration_lineage_edges e JOIN ancestors a ON e.child_firing_id=a.id WHERE e.namespace_id=$1)
 SELECT f.id,f.canonical_firing_id,f.declaration_id,d.name,COALESCE(ev.variables,'{}'::jsonb),COALESCE(r.output,'{}'::jsonb)
 FROM ancestors a JOIN declaration_firings f ON f.namespace_id=$1 AND f.id=a.id
 JOIN declarations d ON d.namespace_id=f.namespace_id AND d.id=f.declaration_id
 LEFT JOIN LATERAL (SELECT ev.firing_id,ev.variables FROM declaration_firings pf
   JOIN declaration_evaluations ev ON ev.namespace_id=pf.namespace_id AND ev.firing_id=pf.id AND ev.outcome='fired'
   WHERE pf.namespace_id=$1 AND pf.canonical_firing_id=f.id ORDER BY ev.created_at DESC,ev.id DESC LIMIT 1) ev ON true
 LEFT JOIN runs r ON r.namespace_id=$1 AND r.id=ev.firing_id AND r.status='completed'
 ORDER BY f.created_at DESC,f.id DESC`, ns, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ancestor
	for rows.Next() {
		var a Ancestor
		var vars, output []byte
		if err := rows.Scan(&a.FiringID, &a.CanonicalID, &a.DeclarationID, &a.Name, &vars, &output); err != nil {
			return nil, err
		}
		a.Variables = map[string]any{}
		for _, raw := range [][]byte{vars, output} {
			var m map[string]any
			// A run output that is not a JSON object carries no variables.
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			for k, v := range m {
				a.Variables[k] = v
			}
		}
		out = append(out, a)
	}
	return ResolveLineage(out), rows.Err()
}
