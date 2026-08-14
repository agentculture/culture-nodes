package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/preflight"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The clarify-then-commit gate at the dispatch site (issue #67, task t14 of
// the upkeep-actors-jira plan; spec claims c18/c53/c54, honesty h10/h36/h39).
//
// WHAT IT IS FOR. Today an actor learns this deployment's operating rules by
// violating them and being corrected afterwards — and the correction usually
// arrives after a session's budget is already spent. Three failures in one
// cycle came from exactly that: a sandbox mode the host cannot actually
// provide, a commit policy the dispatch prompt contradicted, and a deploy
// branch nobody checked. The gate turns that around: before an actor's first
// billable turn, the operating facts its task depends on are handed over as
// a refusal-shaped document, and a second, separate action commits the
// dispatch.
//
// WHAT IT GENERALIZES. deploy/prod/install-secrets.sh already runs this
// protocol for DESTRUCTIVE actions — refuse first, write a file stating what
// breaks, proceed only on an edited verdict, within a window, once per
// confirmation (tests/deploy/destructiveconfirm_test.go). Everything
// load-bearing there is kept here: the composed document holds, it states
// what does not proceed, the acknowledgement is single-use, and it expires.
//
// WHERE THE CHECK LIVES, AND WHY. Between the capacity breaker and dispatch
// pacing — the last stretch where nothing outside the control plane has been
// touched, and specifically BEFORE the pacing slot is consumed: a dispatch
// that is going to be deferred for an acknowledgement must not spend a rate
// slot that exists to keep a provider from refusing us. It is also before
// the pre_run hook and before the endpoint is resolved, because a hook
// executes real code on a real host and that is already an operating cost.
//
// WHY IT DEFERS AND THEN REFUSES. While the window is open the work item is
// DEFERRED — released back to `ready` with available_at pushed forward and
// the dispatch counter given back (postgres.DeferWork), because nothing was
// dispatched. When the window closes with no acknowledgement, the dispatch
// is REFUSED rather than deferred forever: a run silently sitting against a
// bridge that will never answer is the failure mode this gate is supposed to
// prevent, not one it may introduce. The refusal routes under
// engine.OutcomePreflightUnacknowledged, so an author who wants a fallback
// (a different actor, a human, a summarise-and-stop node) declares an edge —
// the same shape task t11 gave `budget_exhausted`, and issue #67's fourth
// open question answered yes.
//
// DEFAULT-OFF, PER ACTOR. The configuration comes from the actor's own
// registration row: `metadata.preflight_gate` enables it,
// `capabilities.preflight` is the surface the bridge advertises, and
// enabling one without the other is refused when the actor is registered
// (internal/preflight.CheckConfiguration, migration 0026's CHECK). A
// registry that cannot answer the configuration question at all — every
// StaticRegistry, i.e. every test and every single-node development
// deployment — leaves every actor ungated. That is what makes merging this
// a no-op for the ten actors registered today.

// Event types the gate emits, carrying §15.1's "dev.culture.nodes." prefix
// like the breaker's own (see breaker.go for why they are declared here
// rather than in internal/events).
const (
	// TypePreflightIssued records a briefing composed and handed over: which
	// actor, which preflight, which ledger record states it, and when the
	// window closes. It is emitted once per briefing, not once per deferral
	// — unlike the breaker's deferrals, this one leaves a durable row
	// (dispatch_preflights) that says exactly why the work is waiting and
	// until when, so repeating the event on every recheck would add noise
	// rather than accountability.
	TypePreflightIssued = "dev.culture.nodes.dispatch.preflight_issued"
	// TypePreflightConsumed records an acknowledgement being spent by the
	// dispatch it authorized — the moment "the actor was told" stops being
	// pending and becomes the reason a session was opened.
	TypePreflightConsumed = "dev.culture.nodes.dispatch.preflight_consumed"
)

// PreflightRefusalClass is the diagnostic class recorded on an attempt
// refused for want of an acknowledgement. It is exported for the same reason
// the event types are: a test and an operator both match on it.
const PreflightRefusalClass = "preflight_unacknowledged"

// PreflightRecheckInterval is how long a gated work item is deferred before
// the gate looks again.
//
// It is deliberately short. A recheck costs one claim and one release — no
// actor call, no session, nothing billable — while the alternative is that
// an acknowledgement that arrived a second after a deferral waits out the
// whole interval before anything happens. The deferral is additionally
// bounded by the window's own close, so the refusal fires when the window
// actually closes rather than up to an interval later.
const PreflightRecheckInterval = 15 * time.Second

// preflightConfigResolver is the optional registry capability the gate reads
// its per-actor configuration through: the actor's registered capabilities
// and metadata documents, verbatim. DBRegistry implements it; a registry
// that does not (StaticRegistry) leaves every actor ungated.
//
// It returns the two raw documents rather than a parsed configuration on
// purpose: parsing is internal/preflight's job, and a registry that also
// parsed would be a second place for the rules to live.
type preflightConfigResolver interface {
	PreflightConfig(ctx context.Context, ref string) (capabilities, metadata json.RawMessage, err error)
}

// clarifyGate runs the gate for one dispatch. It reports whether the
// dispatch may proceed; when it may not, it has already disposed of the
// claimed work item (deferred or refused) and the caller returns.
func (w *Worker) clarifyGate(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	spec *workflowSpec,
	node *nodeSpec,
	dc DispatchContext,
	session sessionPlan,
) (bool, error) {
	resolver, ok := w.opts.Registry.(preflightConfigResolver)
	if !ok {
		return true, nil
	}
	capabilities, metadata, err := resolver.PreflightConfig(ctx, node.Uses)
	if err != nil {
		// The configuration could not be read. Dispatching would be
		// dispatching ungated on a guess, and failing the attempt would turn
		// a transient database blip into a dead node run, so the lease
		// recovers this claim and another worker asks again.
		return false, fmt.Errorf("worker: read preflight configuration for %q: %w", node.Uses, err)
	}

	gate, err := preflight.ParseGate(metadata)
	if err != nil {
		// A registration that asks for the gate in a shape this control
		// plane cannot read. Both configuration doors refuse this shape
		// (internal/preflight.CheckConfiguration), so a row like it was
		// written around them — and reading it as "ungated" would silently
		// drop protection an operator configured. It is a configuration
		// failure, recorded as one.
		return false, w.failAttempt(ctx, claimed, session.ActorRowID, engine.StatusFailed, "configuration",
			fmt.Sprintf("node %q uses %q, whose registration configures the clarify-then-commit gate "+
				"in a shape this control plane cannot read: %v", node.ID, node.Uses, err))
	}
	if !gate.Enabled {
		return true, nil
	}

	now := w.opts.Now()
	open, found, err := w.db.OpenPreflight(ctx, w.opts.NamespaceID, dc.NodeRunID)
	if err != nil {
		return false, fmt.Errorf("worker: read open preflight for node run %s: %w", dc.NodeRunID, err)
	}

	switch {
	case !found:
		return false, w.issuePreflight(ctx, claimed, spec, node, dc, session, capabilities, gate, now)

	case open.Usable(now):
		consumed, err := w.db.ConsumePreflight(ctx, w.opts.NamespaceID, open.ID, dc.AttemptID, now)
		if err != nil {
			return false, fmt.Errorf("worker: consume preflight %s: %w", open.ID, err)
		}
		if !consumed {
			// Another dispatch spent it between the read and the update.
			// Single-use means single-use: this claim waits, and the next
			// recheck finds no open preflight and composes a fresh one.
			return false, w.deferForPreflight(ctx, claimed, dc, open, now)
		}
		w.recordPreflightConsumed(ctx, dc, node, open)
		return true, nil

	case open.Expired(now):
		// The window closed. Whether it was never answered or answered too
		// long ago, the briefing no longer authorizes anything — a stale
		// acknowledgement authorizing today's dispatch is exactly what the
		// window exists to prevent.
		return false, w.refuseUnacknowledged(ctx, claimed, node, dc, open)

	default:
		// Issued, unanswered, window still open.
		return false, w.deferForPreflight(ctx, claimed, dc, open, now)
	}
}

// issuePreflight composes a briefing, appends it as a derived ledger record,
// records the durable row, and defers the work item.
//
// Order matters and is the opposite of convenient: the RECORD is appended
// before the row exists, so a crash in between leaves an unreferenced record
// (a briefing that was composed and never handed over — harmless, and the
// next claim composes another) rather than a row pointing at evidence that
// does not exist. The row is the authorization; it may never be the more
// trusted of the two.
func (w *Worker) issuePreflight(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	spec *workflowSpec,
	node *nodeSpec,
	dc DispatchContext,
	session sessionPlan,
	capabilities json.RawMessage,
	gate preflight.Gate,
	now time.Time,
) error {
	surface, ok, err := preflight.ParseSurface(capabilities)
	if err != nil || !ok {
		// The gate is enabled against an actor whose advertised surface has
		// gone missing or unreadable since it was registered. Composing a
		// briefing out of nothing would tell the actor nothing while still
		// costing it a round trip, so this is refused as the configuration
		// failure it is — and named as one, because the fix is a
		// registration, not a retry.
		detail := fmt.Sprintf("node %q uses %q, whose gate is enabled but which advertises no readable "+
			"preflight capability surface", node.ID, node.Uses)
		if err != nil {
			detail = fmt.Sprintf("%s: %v", detail, err)
		}
		return w.failAttempt(ctx, claimed, session.ActorRowID, engine.StatusFailed, "configuration", detail)
	}

	doc := preflight.Compose(surface, preflight.Task{
		RunID:          dc.RunID,
		NodeRunID:      dc.NodeRunID,
		NodeID:         node.ID,
		NodeKind:       node.Kind,
		ActorRef:       node.Uses,
		ActorKey:       actorKeyOf(node.Uses),
		ActorID:        session.ActorRowID,
		WorkflowName:   spec.Name,
		WorkflowDigest: spec.Digest,
		ContractDigest: node.ContractDigest,
		Outcomes:       node.Outcomes,
		Deadline:       deadlinePointer(dc.Deadline),
	}, now, gate.Window())

	record, err := preflight.NewPreflightRecord(doc, w.dispatchGateActorID())
	if err != nil {
		return fmt.Errorf("worker: compose preflight for node run %s: %w", dc.NodeRunID, err)
	}
	appended, err := w.ledger.Append(ctx, record)
	if err != nil {
		// The commonest cause by far is a producer identity nobody
		// registered: ledger_records.origin_actor_id has a real foreign key
		// to actors(id), so the gate's own identity is a registration
		// obligation exactly like the hook runner's. It is reported as a
		// CONFIGURATION failure rather than returned, because returning it
		// would leave the lease to expire and the next worker to fail the
		// same way forever, with the reason visible only in a log.
		return w.failAttempt(ctx, claimed, session.ActorRowID, engine.StatusFailed, "configuration",
			fmt.Sprintf("node %q has the clarify-then-commit gate enabled, but its preflight record could "+
				"not be appended: %v. The gate's producer identity %q must be a registered actor "+
				"(ledger_records.origin_actor_id references actors(id)); register it with kind `engine` "+
				"and no endpoint, or set the worker's DispatchGateActorID to an identity that is",
				node.ID, err, w.dispatchGateActorID()))
	}

	row, err := w.db.IssuePreflight(ctx, postgres.IssuePreflightInput{
		NamespaceID:  w.opts.NamespaceID,
		RunID:        dc.RunID,
		NodeRunID:    dc.NodeRunID,
		NodeID:       node.ID,
		ActorKey:     actorKeyOf(node.Uses),
		ActorID:      session.ActorRowID,
		RecordID:     appended.ID,
		RecordDigest: appended.ContentDigest,
		IssuedAt:     now,
		ExpiresAt:    doc.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("worker: issue preflight for node run %s: %w", dc.NodeRunID, err)
	}

	data := map[string]any{
		"run_id":       dc.RunID,
		"node_run_id":  dc.NodeRunID,
		"node_id":      node.ID,
		"work_id":      claimed.ID,
		"actor_key":    row.ActorKey,
		"actor_ref":    node.Uses,
		"preflight_id": row.ID,
		"record_id":    row.RecordID,
		"expires_at":   row.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"acknowledge":  fmt.Sprintf("POST /v1alpha1/preflights/%s/acknowledge", row.ID),
	}
	if row.ActorID != "" {
		data["actor_id"] = row.ActorID
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypePreflightIssued, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for work %s: %w", TypePreflightIssued, claimed.ID, err))
	}

	return w.deferForPreflight(ctx, claimed, dc, row, now)
}

// deferForPreflight releases a claimed work item without dispatching it,
// because its briefing has not been acknowledged yet.
//
// Like the breaker's deferral it gives back the dispatch counter — nothing
// was dispatched — and unlike it, it emits no event: TypePreflightIssued
// already said what is being waited for, and the dispatch_preflights row
// says it durably. See PreflightRecheckInterval for the interval, which is
// additionally clamped to the window's own close so the refusal fires when
// the window actually closes.
func (w *Worker) deferForPreflight(
	ctx context.Context, claimed postgres.ClaimedWork, dc DispatchContext, row postgres.Preflight, now time.Time,
) error {
	availableAt := now.Add(PreflightRecheckInterval)
	if row.ExpiresAt.Before(availableAt) {
		availableAt = row.ExpiresAt
	}

	err := w.db.DeferWork(ctx, claimed.ID, w.opts.WorkerID, claimed.FencingToken, int(claimed.Attempt), availableAt)
	if err != nil {
		if errors.Is(err, postgres.ErrStaleClaim) {
			// Somebody else holds the item and will make the same decision
			// against the same durable row.
			return nil
		}
		return fmt.Errorf("worker: defer work %s awaiting preflight %s: %w", claimed.ID, row.ID, err)
	}
	return nil
}

// refuseUnacknowledged ends a dispatch whose briefing was never
// acknowledged inside its window.
//
// It mirrors refuseUnfunded exactly, because it is the same kind of event: a
// refusal the CONTROL PLANE produced before dispatching anything, carrying a
// domain name the author can route rather than a technical status they
// cannot. The attempt is recorded unattributed ("" -> NULL actor_id) for the
// same reason: no actor did anything here, and which actor it was addressed
// to belongs in the detail, where it is a fact about the refusal rather than
// a mark against the actor's record.
func (w *Worker) refuseUnacknowledged(
	ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext,
	row postgres.Preflight,
) error {
	detail := fmt.Sprintf(
		"node %q uses %q, whose dispatch preflight %s (ledger record %s) was issued at %s and expired at %s "+
			"without an acknowledgement; the actor was never invoked",
		node.ID, node.Uses, row.ID, row.RecordID,
		row.IssuedAt.UTC().Format(time.RFC3339), row.ExpiresAt.UTC().Format(time.RFC3339))
	if row.Acknowledged() {
		detail = fmt.Sprintf(
			"node %q uses %q, whose dispatch preflight %s was acknowledged at %s but expired at %s before it "+
				"could authorize a dispatch; a stale acknowledgement authorizes nothing",
			node.ID, node.Uses, row.ID,
			row.AcknowledgedAt.UTC().Format(time.RFC3339), row.ExpiresAt.UTC().Format(time.RFC3339))
	}

	_, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus:     engine.StatusPolicyDenied,
		RefusalOutcome: engine.OutcomePreflightUnacknowledged,
		Output:         diagnosticOutput(PreflightRefusalClass, detail),
	})
	if err != nil {
		if isStale(err) {
			return nil
		}
		return err
	}

	data := map[string]any{
		"run_id":       dc.RunID,
		"node_run_id":  dc.NodeRunID,
		"node_id":      node.ID,
		"attempt_id":   dc.AttemptID,
		"work_id":      claimed.ID,
		"actor_ref":    node.Uses,
		"actor_key":    row.ActorKey,
		"preflight_id": row.ID,
		"outcome":      engine.OutcomePreflightUnacknowledged,
		"detail":       detail,
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeDispatchRefused, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for work %s: %w", TypeDispatchRefused, claimed.ID, err))
	}
	return nil
}

// recordPreflightConsumed announces that an acknowledgement was spent. It is
// best-effort: the consumption itself already committed, and a missing event
// must never unwind a dispatch that is about to happen.
func (w *Worker) recordPreflightConsumed(
	ctx context.Context, dc DispatchContext, node *nodeSpec, row postgres.Preflight,
) {
	data := map[string]any{
		"run_id":                    dc.RunID,
		"node_run_id":               dc.NodeRunID,
		"node_id":                   node.ID,
		"attempt_id":                dc.AttemptID,
		"actor_key":                 row.ActorKey,
		"actor_ref":                 node.Uses,
		"preflight_id":              row.ID,
		"record_id":                 row.RecordID,
		"acknowledgement_record_id": row.AcknowledgementRecordID,
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypePreflightConsumed, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for preflight %s: %w", TypePreflightConsumed, row.ID, err))
	}
}

// dispatchGateActorID is the producer identity derived preflights are
// attributed to: the deployment's own, or the documented default.
func (w *Worker) dispatchGateActorID() string {
	if w.opts.DispatchGateActorID != "" {
		return w.opts.DispatchGateActorID
	}
	return preflight.DispatchGateActorID
}

// deadlinePointer converts the dispatch context's zero-means-absent deadline
// into the optional one the preflight document carries, so "this node
// declared no deadline" stays absent rather than becoming the epoch.
func deadlinePointer(deadline time.Time) *time.Time {
	if deadline.IsZero() {
		return nil
	}
	utc := deadline.UTC()
	return &utc
}
