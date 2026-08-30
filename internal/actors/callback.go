package actors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/telemetry"
)

// Callback ingest (PRD §13.4).
//
// The path a §13.4 event walks, and why each step is where it is:
//
//  1. Verify the token. Everything after this reads an attempt id that came
//     out of a signature, never one supplied by the caller.
//  2. Load the durable invocation. Between the 202 and this event no process
//     held anything in memory (§12.6), so this row is the only thing that
//     knows which work item, lease owner, fencing token, and attempt the
//     dispatch used. A verified token whose row is not there YET (see
//     lookupInvocation) is retried briefly before it is treated as evidence
//     the attempt never existed.
//  3. Deduplicate by (attempt, event id). Delivery is at-least-once (§20.1),
//     so a repeat is expected traffic, not an error.
//  4. Enforce the monotonic sequence. A reordered or replayed-out-of-band
//     event is recorded as a diagnostic and changes nothing.
//  5. For a terminal event, re-lease the work item under the fencing token
//     recorded at dispatch, then commit through engine.CompleteAttempt. If
//     anything newer has claimed the work in the meantime, both the re-lease
//     and the engine's own fenced guard refuse, and the completion becomes a
//     late diagnostic — §13.4's "completion after cancellation or attempt
//     replacement is recorded as a late diagnostic event but cannot commit
//     workflow state". A re-lease that is not followed by a committed
//     completion — refused or failed — is parked again before returning, so
//     the item is never left leased to work nobody is doing.
//
// Steps 3–5 are separate commits rather than one transaction, because step 5
// is the engine's own §12.5 transaction and this package does not get to open
// it. The consequence is stated and handled rather than hidden: a delivery
// that fails part-way gives back everything it took, in the reverse order it
// took it — the work item's resumed lease, then the sequence mark, then the
// event-id claim.
//
// All three compensations are required, and the live 2026-08-11 incident
// (issue #16) is what each one costs when it is missing. A terminal commit
// that failed used to keep the sequence mark, so the same-id/same-sequence
// redelivery §13.4 mandates was refused as out-of-order forever; and it used
// to keep the resumed lease, so the work item expired into ReclaimExpired and
// re-dispatched a fresh billable session every lease cycle. The claim is
// released LAST because it is the gate: while it is held, no redelivery can
// start reprocessing an event whose mark and lease are still being returned.
//
// The mark therefore records what this ingest PROCESSED, not what it saw.
// That is what keeps §13.4's monotonic rule intact while making failure
// recoverable: an event that changed nothing leaves nothing behind, and a
// genuinely reordered event — a different event id at a sequence that was
// processed — is still refused.
//
// The single asymmetric case is a failure AFTER the engine committed (only
// CloseInvocation can be there): workflow state moved, so the lease is not
// given back and the redelivery the released claim invites lands as a late
// diagnostic. See commitTerminal.

// Diagnostic event types this package appends. They are recorded against the
// run aggregate, like every other engine event, so a run's audit trail
// carries the callbacks that did *not* change anything as well as the ones
// that did — "nothing happened" is only trustworthy if it is written down.
const (
	// TypeCallbackReceived records a non-terminal event (accepted,
	// heartbeat, progress, artifact) that advanced the sequence.
	TypeCallbackReceived = "dev.culture.nodes.actor.callback-received"
	// TypeCallbackDuplicate records an event id already seen for this
	// attempt.
	TypeCallbackDuplicate = "dev.culture.nodes.actor.callback-duplicate"
	// TypeCallbackOutOfOrder records an event whose sequence did not advance
	// the per-attempt high-water mark.
	TypeCallbackOutOfOrder = "dev.culture.nodes.actor.callback-out-of-order"
	// TypeCallbackLate records a terminal event that arrived after the
	// attempt it belongs to was replaced or cancelled. This is §13.4's late
	// diagnostic. It no longer stands alone: since task t11 the report also
	// leaves a superseding attempt record, and this event names it
	// (`superseding_attempt_id`) so the audit trail and the attempts table
	// point at each other.
	TypeCallbackLate = "dev.culture.nodes.actor.callback-late"
	// TypeCallbackRejected records an event this ingest could not act on at
	// all: an unusable payload for a terminal kind, or a `signal` emission
	// that named no event.
	TypeCallbackRejected = "dev.culture.nodes.actor.callback-rejected"
	// TypeSignalEmitted records a mid-execution `signal` emission (design
	// D11): which attempt emitted, under what name and scope, which
	// signal_events fact it appended, and how many waiters and routes the
	// delivery woke. It is its own type rather than a callback-received event
	// because "this node spoke to the rest of the system while still working"
	// is an operational fact an operator should not have to dig out of a
	// generic event's payload — the same reasoning that gave a signal wait its
	// own TypeAttemptWaitingSignal.
	TypeSignalEmitted = "dev.culture.nodes.actor.signal-emitted"
	// TypeCallbackCommitFailed records a terminal event whose commit failed
	// for an infrastructure reason rather than a §13.4 refusal; its `stage`
	// field says how far the commit got, and so whether the engine's own
	// transaction was reached at all. It exists because the live 2026-08-11
	// incident (issue #16) proved the alternative: the error rode the HTTP
	// response back to the bridge and nowhere else, so a run that could not
	// progress carried no recorded reason anywhere an operator looks.
	TypeCallbackCommitFailed = "dev.culture.nodes.actor.callback-commit-failed"
)

// PendingInvocation is the durable record of an in-flight asynchronous
// invocation: the fencing tuple the dispatch held, plus the identity an
// operator or a cancellation needs.
type PendingInvocation struct {
	AttemptID    string
	NamespaceID  string
	RunID        string
	NodeRunID    string
	TokenID      string
	NodeID       string
	WorkID       string
	WorkerID     string
	FencingToken int64
	Attempt      int
	ActorRef     string
	// ActorID is the resolved actors-table row id captured at dispatch
	// (actor_invocations.actor_id, migration 0015) — committed into
	// attempts.actor_id so per-actor stats attribute async work. Empty on
	// rows parked by pre-0015 binaries: those complete unattributed.
	ActorID      string
	InvocationID string
	State        string
	LastSequence int64
}

// Invocation states.
const (
	// InvocationWaiting is an invocation the control plane is still waiting
	// on: the work item is parked and no worker holds it.
	InvocationWaiting = "waiting_external"
	// InvocationCompleted is an invocation whose terminal event committed.
	InvocationCompleted = "completed"
	// InvocationSuperseded is an invocation whose terminal event arrived too
	// late to commit anything.
	InvocationSuperseded = "superseded"
	// InvocationCancelled is an invocation cancelled by the control plane.
	InvocationCancelled = "cancelled"
)

// CallbackStore is the durable state callback ingest needs. It is declared
// here, and implemented by internal/store/postgres, so what the ingest
// requires of persistence is readable in one place.
type CallbackStore interface {
	// Invocation loads the durable record for a protocol attempt id,
	// returning an error matching ErrUnknownAttempt when there is none.
	Invocation(ctx context.Context, attemptID string) (PendingInvocation, error)
	// ActorKey resolves an actor row id to its actor_key (task t24 hotfix): the
	// custody check compares identities by key because a bridge's identity row
	// and the dispatched registration row are different rows of one actor.
	ActorKey(ctx context.Context, actorID string) (string, error)

	// ClaimCallbackEvent records (attemptID, eventID) as seen, reporting
	// false when it was already recorded. It is the idempotency boundary for
	// §13.4's "repeated callbacks are idempotent".
	ClaimCallbackEvent(ctx context.Context, inv PendingInvocation, eventID string) (claimed bool, err error)
	// ReleaseCallbackEvent forgets a claim, so a redelivery is processed
	// again. It is used only when processing failed for an infrastructure
	// reason after the claim was taken.
	ReleaseCallbackEvent(ctx context.Context, inv PendingInvocation, eventID string) error

	// AdvanceCallbackSequence raises the per-attempt high-water mark to
	// sequence, reporting false when sequence did not exceed it. It must be
	// a single atomic compare-and-set.
	AdvanceCallbackSequence(ctx context.Context, attemptID string, sequence int64) (advanced bool, err error)
	// RollbackCallbackSequence lowers the high-water mark from sequence back
	// to previous, and only while it still equals sequence — the compensation
	// for an advance whose event was then not processed. Lowering the mark
	// cannot resurrect an already-ingested event: the event-id claim, not the
	// mark, is this ingest's idempotency authority, and every processed event
	// still holds one.
	RollbackCallbackSequence(ctx context.Context, attemptID string, sequence, previous int64) error

	// TouchInvocation records liveness for a non-terminal event.
	TouchInvocation(ctx context.Context, attemptID string, invocationID string, at time.Time) error

	// EmitSignalEvent appends one signal event on the emitting attempt's
	// behalf and delivers it — the same delivery POST /v1alpha1/events
	// performs (fire pending subscriptions, fire active event routes), in one
	// transaction. It commits NOTHING about the emitting attempt: the
	// emitter's own work item, lease, and fencing state are untouched, which
	// is what makes emission non-blocking (design D11).
	EmitSignalEvent(ctx context.Context, inv PendingInvocation, in EmitSignalInput) (EmitSignalResult, error)
	// CloseInvocation moves an invocation out of the waiting state.
	CloseInvocation(ctx context.Context, attemptID, state string) error

	// RecordSupersedingAttempt appends the attempt record a late terminal
	// callback leaves: the facts the actor reported (status, output, usage
	// including the model, termination reason, continuation ref, preserve
	// block), on a NEW row naming the record it corrects. It changes nothing
	// else — no work item, no node run, no ledger — because §13.4's refusal
	// to let a late completion commit workflow state is not what this
	// records. See task t11 and docs/adr/0012-late-callback-supersession.md.
	//
	// It is idempotent per DELIVERY — the (inv.AttemptID, callbackEventID)
	// pair — because callback delivery is at-least-once and this ingest
	// legitimately reprocesses a report whose first pass failed part-way: a
	// second call for the same callback event returns the record the first
	// one appended. Keying on the delivery rather than on the record being
	// corrected is what makes that true of the report that corrects NOTHING
	// as well, which is the flavour of lateness with no other natural key
	// (ADR 0012 §5; before it, that report could be persisted twice).
	RecordSupersedingAttempt(
		ctx context.Context, inv PendingInvocation, callbackEventID string, req engine.CompletionRequest,
	) (SupersedingAttempt, error)

	// ResumeWaitingWork re-leases the parked work item under the fencing
	// tuple recorded at dispatch, so the engine's own fenced completion guard
	// can match. It returns an error matching engine.ErrStaleClaim when the
	// item is no longer parked under that tuple.
	ResumeWaitingWork(ctx context.Context, inv PendingInvocation, lease time.Duration) error
	// ReparkResumedWork undoes ResumeWaitingWork: the work item returns to
	// parked, keeping the same fencing tuple, when the completion it was
	// resumed for did not commit. Without it a failed commit leaves an item
	// leased with nobody working it, and its lease expiry is a re-dispatch
	// signal (issue #16's billable loop).
	//
	// It reports no error when the row is no longer leased under inv's tuple:
	// this is a compensation, not a claim, and "the engine completed it after
	// all" is a legitimate outcome to find.
	ReparkResumedWork(ctx context.Context, inv PendingInvocation) error

	// AppendRunEvent appends one diagnostic event to a run's audit log.
	AppendRunEvent(ctx context.Context, namespaceID, runID, eventType string, data map[string]any) error
}

// EmitSignalInput is one mid-execution emission, as the ingest resolved it:
// the name and body the actor asked for, the emitter the ingest DERIVED from
// the verified attempt, and the run scope (empty means namespace-wide).
type EmitSignalInput struct {
	Name    string
	Payload json.RawMessage
	Emitter string
	RunID   string
}

// EmitSignalResult is what one emission's delivery did. Both counts are
// reported so the audit event can say whether anything was listening — an
// emission nobody heard is a legitimate outcome, and a silent one would be
// indistinguishable from a broken route.
type EmitSignalResult struct {
	EventID string
	// Resumed is how many parked signal subscriptions the delivery fired.
	Resumed int
	// PickedUp is how many event routes created a token (design D9).
	PickedUp int
}

// Completer is the slice of the engine callback ingest needs. It is an
// interface so a test can prove the late-completion path without a second
// engine, and so this package depends on one method rather than on the whole
// engine surface.
type Completer interface {
	CompleteAttempt(ctx context.Context, req engine.CompletionRequest) (engine.CompletionResult, error)
}

// ErrUnknownAttempt reports a callback for an attempt with no durable
// invocation: either it never existed, or it was cleaned up.
var ErrUnknownAttempt = errors.New("actors: no in-flight invocation for this attempt")

// CallbackDeps is everything HandleCallback needs.
type CallbackDeps struct {
	// Store is the durable state.
	Store CallbackStore
	// Engine commits terminal events.
	Engine Completer
	// Signer verifies the attempt-scoped token.
	Signer *TokenSigner
	// ResumeLease is how long the work item is re-leased for while a
	// terminal event commits. It only has to outlive one CompleteAttempt
	// transaction; a minute is generous.
	ResumeLease time.Duration
	// Now is the clock, defaulting to time.Now().UTC().
	Now func() time.Time
	// InvocationLookupRetries bounds how many times step 2 re-reads
	// Store.Invocation for a just-verified attempt before concluding
	// ErrUnknownAttempt is the honest answer. See lookupInvocation's doc for
	// why a verified token warrants a retry at all. Defaults to
	// DefaultInvocationLookupRetries.
	InvocationLookupRetries int
	// InvocationLookupDelay is the pause between those re-reads. Defaults to
	// DefaultInvocationLookupDelay.
	InvocationLookupDelay time.Duration

	// Handover fetches a handed-over git ref reported on a `completed`
	// event and records what it measured as observed evidence (task t10,
	// issue #13). Nil — the default, and every deployment that has
	// configured no remote to fetch from — observes nothing: see
	// handover.go in this package, and internal/handover's package doc for
	// why a control plane that cannot look must write no record at all.
	Handover *handover.Observer

	// Telemetry instruments the callback ingest seam (task t19,
	// HandleCallback) through internal/telemetry. The zero value, a nil
	// *telemetry.Provider, is a safe no-op — every telemetry.Provider
	// method tolerates a nil receiver — so a CallbackDeps built without
	// setting this field (every existing caller, every existing test)
	// behaves exactly as it did before this field existed.
	Telemetry *telemetry.Provider
}

// DefaultResumeLease is the lease taken while a terminal callback commits.
const DefaultResumeLease = time.Minute

// DefaultInvocationLookupRetries and DefaultInvocationLookupDelay bound
// lookupInvocation's total wait to 200ms — long enough to survive one
// racing StartAsyncWait commit, short enough that a genuinely unknown
// attempt is still reported promptly.
const (
	DefaultInvocationLookupRetries = 8
	DefaultInvocationLookupDelay   = 25 * time.Millisecond
)

func (d CallbackDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d CallbackDeps) resumeLease() time.Duration {
	if d.ResumeLease > 0 {
		return d.ResumeLease
	}
	return DefaultResumeLease
}

func (d CallbackDeps) invocationLookupRetries() int {
	if d.InvocationLookupRetries > 0 {
		return d.InvocationLookupRetries
	}
	return DefaultInvocationLookupRetries
}

func (d CallbackDeps) invocationLookupDelay() time.Duration {
	if d.InvocationLookupDelay > 0 {
		return d.InvocationLookupDelay
	}
	return DefaultInvocationLookupDelay
}

// lookupInvocation is step 2: load the durable invocation record a
// dispatch's park (postgres.StartAsyncWait) wrote.
//
// A token that verifies (step 1) can only have been minted by a worker that
// was actively dispatching this exact attempt — internal/worker/dispatch.go
// mints it before invoking the actor — so a lookup that finds nothing YET is
// not proof the attempt never existed; it may simply be racing that same
// worker's own not-yet-committed park write. A synchronous actor never
// triggers this (no invocation row is ever expected for one). An
// asynchronous one that reports back near-instantly can: §13.3 lets an actor
// answer 202 and call back moments later, and an actor with negligible real
// work behind its acceptance (a mock backend, for instance) can win that
// race often enough to matter — see docs/deliveries/2026-08-08-culture-
// nodes-app-design.md's "run.output observed null for the live smoke's
// end-node binding", which this retry is the fix for: without it, the first
// (and, for an actor that treats 404 as permanent, only) callback attempt is
// refused, the work item is left parked in waiting_external forever, and the
// run's output never resolves.
//
// Retrying briefly closes that race without weakening ErrUnknownAttempt's
// meaning: an attempt whose token was never minted by this deployment's
// signer never reaches here (step 1 already refused it), and an attempt
// that genuinely has no invocation record even after the wait still, in the
// end, reports exactly that.
func (d CallbackDeps) lookupInvocation(ctx context.Context, attemptID string) (PendingInvocation, error) {
	attempts := d.invocationLookupRetries()
	delay := d.invocationLookupDelay()

	var (
		inv PendingInvocation
		err error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		inv, err = d.Store.Invocation(ctx, attemptID)
		if err == nil || !errors.Is(err, ErrUnknownAttempt) || attempt == attempts {
			return inv, err
		}
		select {
		case <-ctx.Done():
			return PendingInvocation{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return inv, err
}

// CallbackDisposition is what an ingested event did.
type CallbackDisposition string

// The dispositions. Exactly one applies to any handled event.
const (
	// DispositionRecorded is a non-terminal event that advanced the sequence.
	DispositionRecorded CallbackDisposition = "recorded"
	// DispositionDuplicate is an event id already seen for this attempt.
	DispositionDuplicate CallbackDisposition = "duplicate"
	// DispositionOutOfOrder is an event whose sequence did not advance.
	DispositionOutOfOrder CallbackDisposition = "out_of_order"
	// DispositionCommitted is a terminal event that committed engine state.
	DispositionCommitted CallbackDisposition = "committed"
	// DispositionLate is a terminal event that arrived after its attempt was
	// replaced or cancelled: recorded, committed nothing.
	DispositionLate CallbackDisposition = "late"
	// DispositionRejected is a terminal event whose payload could not be
	// acted on.
	DispositionRejected CallbackDisposition = "rejected"
)

// CommittedState reports whether the disposition changed workflow state.
func (d CallbackDisposition) CommittedState() bool { return d == DispositionCommitted }

// CallbackResult is what HandleCallback did with one event.
type CallbackResult struct {
	AttemptID   string
	Disposition CallbackDisposition
	// Completion is the engine's committed result, set only when Disposition
	// is DispositionCommitted.
	Completion *engine.CompletionResult
	// Diagnostic explains a non-committing disposition.
	Diagnostic string
	// SupersedingAttemptID is the attempt record a late terminal callback
	// left (task t11), set only when Disposition is DispositionLate. It is
	// how a caller — an HTTP handler answering the bridge, a test — can name
	// the row the report landed on without going back to the database.
	SupersedingAttemptID string
}

// HandleCallback ingests one PRD §13.4 event.
//
// It returns an error only for a failure the caller must act on: a refused
// token (*TokenError), an unknown attempt (ErrUnknownAttempt), a malformed
// event, or an infrastructure failure. Everything the protocol expects to
// happen — a duplicate, a reordering, a late completion — is a successful
// call with a disposition that says so, because those are outcomes of the
// protocol working, not failures of it.
func HandleCallback(ctx context.Context, deps CallbackDeps, attemptToken string, ev CallbackEvent) (result CallbackResult, err error) {
	switch {
	case deps.Store == nil:
		return CallbackResult{}, errors.New("actors: HandleCallback requires a store")
	case deps.Signer == nil:
		return CallbackResult{}, errors.New("actors: HandleCallback requires a token signer")
	case ev.EventID == "":
		return CallbackResult{}, errors.New("actors: callback event requires an event_id")
	case !ev.Kind.Valid():
		return CallbackResult{}, fmt.Errorf("actors: callback event kind %q is not one of PRD §13.4's kinds", ev.Kind)
	case ev.Sequence <= 0:
		return CallbackResult{}, errors.New("actors: callback event requires a positive sequence")
	}

	// Task t19's actor-callback seam: one span and one metric recording per
	// ingested event, wrapping every disposition (duplicate, out-of-order,
	// recorded, committed, late, rejected) and every failure path below. It
	// starts only after the request-shape validation above — there is no
	// attempt context yet for a malformed request to report against — and
	// inv is declared here, not with the lookup below, so the deferred End
	// can read whatever lookupInvocation managed to resolve even on a path
	// that returns before every field is known.
	var inv PendingInvocation
	ctx, op := deps.Telemetry.Start(ctx, telemetry.SeamActorCallback)
	defer func() {
		op.End(ctx, err == nil,
			telemetry.RunID(inv.RunID),
			telemetry.NodeID(inv.NodeID),
			telemetry.AttemptID(result.AttemptID),
			telemetry.Disposition(string(result.Disposition)),
		)
	}()

	// ---- 1. the token names the attempt ----
	attemptID, err := deps.Signer.Verify(attemptToken)
	if err != nil {
		return CallbackResult{}, err
	}

	// ---- 2. the durable invocation ----
	inv, err = deps.lookupInvocation(ctx, attemptID)
	if err != nil {
		return CallbackResult{AttemptID: attemptID}, err
	}

	// ---- 3. idempotency by event id ----
	claimed, err := deps.Store.ClaimCallbackEvent(ctx, inv, ev.EventID)
	if err != nil {
		return CallbackResult{AttemptID: attemptID}, err
	}
	if !claimed {
		diagnostic := fmt.Sprintf("event %s was already ingested for attempt %s", ev.EventID, attemptID)
		deps.record(ctx, inv, TypeCallbackDuplicate, ev, diagnostic)
		return CallbackResult{AttemptID: attemptID, Disposition: DispositionDuplicate, Diagnostic: diagnostic}, nil
	}

	result, err = handleClaimed(ctx, deps, inv, ev)
	if err != nil {
		// The claim was taken but the event was not processed. Give it back
		// last, after handleClaimed has already returned the mark and the
		// lease, so the actor's redelivery is neither mistaken for a duplicate
		// of work that never happened nor let in while the compensations are
		// still running. A failed release must not mask the original error,
		// but it must not vanish either — an unreleased claim turns the
		// redelivery into a permanent duplicate, so the failure is recorded
		// where the out-of-order and duplicate diagnostics already live.
		if relErr := deps.Store.ReleaseCallbackEvent(ctx, inv, ev.EventID); relErr != nil {
			deps.record(ctx, inv, TypeCallbackCommitFailed, ev, fmt.Sprintf(
				"compensation failed: event-id claim for %s was not released (%v); its redelivery will be refused as a duplicate until the claim is cleared",
				ev.EventID, relErr))
		}
		return CallbackResult{AttemptID: attemptID}, err
	}
	return result, nil
}

func handleClaimed(ctx context.Context, deps CallbackDeps, inv PendingInvocation, ev CallbackEvent) (CallbackResult, error) {
	// ---- 4. monotonic per-attempt sequence ----
	advanced, err := deps.Store.AdvanceCallbackSequence(ctx, inv.AttemptID, ev.Sequence)
	if err != nil {
		return CallbackResult{}, err
	}
	if !advanced {
		diagnostic := fmt.Sprintf(
			"event %s carries sequence %d, which does not advance attempt %s past %d; recorded, no state change",
			ev.EventID, ev.Sequence, inv.AttemptID, inv.LastSequence)
		deps.record(ctx, inv, TypeCallbackOutOfOrder, ev, diagnostic)
		return CallbackResult{AttemptID: inv.AttemptID, Disposition: DispositionOutOfOrder, Diagnostic: diagnostic}, nil
	}

	result, err := processAdvanced(ctx, deps, inv, ev)
	if err != nil {
		// The mark was raised for an event this ingest did not process, so it
		// is lowered again — otherwise the redelivery of THIS event carries a
		// sequence the ratchet has already consumed and is refused forever.
		// Conditional on the mark still being ours, so a concurrent delivery
		// that legitimately moved it on is left alone. A failed rollback is
		// exactly the permanent-block bug this compensation exists to
		// prevent, so it is recorded rather than swallowed (without masking
		// the original error).
		if rbErr := deps.Store.RollbackCallbackSequence(ctx, inv.AttemptID, ev.Sequence, inv.LastSequence); rbErr != nil {
			deps.record(ctx, inv, TypeCallbackCommitFailed, ev, fmt.Sprintf(
				"compensation failed: sequence mark %d was not rolled back to %d (%v); this event's redelivery will be refused out-of-order until the mark is corrected",
				ev.Sequence, inv.LastSequence, rbErr))
		}
		return CallbackResult{}, err
	}
	return result, nil
}

// processAdvanced is everything a delivery does once it owns both the event-id
// claim and the sequence mark. Every error return from here is compensated by
// its caller.
func processAdvanced(ctx context.Context, deps CallbackDeps, inv PendingInvocation, ev CallbackEvent) (CallbackResult, error) {
	if ev.Kind == EventSignal {
		return emitSignal(ctx, deps, inv, ev)
	}
	if !ev.Kind.Terminal() {
		invocationID := ""
		if ev.Kind == EventAccepted {
			var payload AcceptedPayload
			if len(ev.Payload) > 0 {
				_ = json.Unmarshal(ev.Payload, &payload)
			}
			invocationID = payload.InvocationID
		}
		if err := deps.Store.TouchInvocation(ctx, inv.AttemptID, invocationID, deps.now()); err != nil {
			return CallbackResult{}, err
		}
		// A plain heartbeat is deliberately NOT written to the run's audit
		// log. An actor with a 30-second heartbeat and an hour of work would
		// otherwise contribute a hundred events that say nothing except that
		// it is still alive — which the invocation row's updated_at already
		// says, in one place, without diluting a run's transition history.
		// Everything else non-terminal is recorded: accepted names the
		// invocation, progress and artifact carry content.
		if ev.Kind != EventHeartbeat {
			deps.record(ctx, inv, TypeCallbackReceived, ev, "")
		}
		return CallbackResult{AttemptID: inv.AttemptID, Disposition: DispositionRecorded}, nil
	}

	return commitTerminal(ctx, deps, inv, ev)
}

// emitSignal is the non-blocking mid-execution emission (issue #43 task t21,
// design D11): the attempt asks the control plane to append a named fact and
// deliver it, and keeps working.
//
// Nothing here touches the emitter's own state. There is no resume, no
// re-lease, no completion and no repark — the invocation stays exactly as
// parked as it was, and its own terminal event, whenever it comes, commits
// through commitTerminal in a separate transaction that simply queues behind
// this delivery on the run's advisory lock. That is what "non-blocking" means
// structurally rather than aspirationally: there is no state for a failed
// emission to have half-changed.
//
// Idempotency comes free from the surrounding ingest: HandleCallback's step-3
// event-id claim means a redelivered emission never reaches here twice, so one
// `signal` event id appends exactly one fact however many times the bridge
// retries it.
//
// The emitter is DERIVED, never read from the payload — see signalEmitter.
//
// Design open item O1, resolved and recorded rather than left hanging. Every
// actor dispatch is minted a callback token, synchronous or not
// (internal/worker/dispatch.go's callbackFor, "filled in even for a dispatch
// that turns out to be synchronous"), so the credential is never the missing
// piece. What emission additionally needs is step 2's durable invocation row,
// and only the §12.6 async park writes one — so an actor that answered 200
// inline cannot emit today, and gets ErrUnknownAttempt. That is not a live
// gap for this deployment: every bridge shipped under adapters/ answers 202
// and is parked, which is also the only shape in which "emit and keep
// working" is meaningful at protocol level. The
// token's 15-minute TTL is the real constraint on a long session, and
// emission inherits whatever re-issue path async completion already uses
// rather than inventing a second one.
//
// All-backends note (repo rule): the engine-side surface is one callback
// kind, but a backend only HAS this feature once its bridge exposes an emit
// affordance to its session. That is adapter work outside this task's engine
// scope, and it is named here rather than left for one backend to quietly
// become the only emitter.
func emitSignal(ctx context.Context, deps CallbackDeps, inv PendingInvocation, ev CallbackEvent) (CallbackResult, error) {
	var payload SignalPayload
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return deps.rejectSignal(ctx, inv, ev, fmt.Sprintf(
				"signal event %s carries a payload that is not {name, payload?, scope?}: %v", ev.EventID, err))
		}
	}
	if payload.Name == "" {
		return deps.rejectSignal(ctx, inv, ev, fmt.Sprintf(
			"signal event %s names no event to emit; set payload.name", ev.EventID))
	}
	switch payload.Scope {
	case "", SignalScopeRun, SignalScopeNamespace:
	default:
		return deps.rejectSignal(ctx, inv, ev, fmt.Sprintf(
			"signal event %s declares scope %q; the scopes are %q and %q",
			ev.EventID, payload.Scope, SignalScopeRun, SignalScopeNamespace))
	}

	// Run scope is the default: an emission is a fact about the run that
	// emitted it unless the actor explicitly asks to speak to the namespace.
	scopedRun := inv.RunID
	if payload.Scope == SignalScopeNamespace {
		scopedRun = ""
	}

	emitted, err := deps.Store.EmitSignalEvent(ctx, inv, EmitSignalInput{
		Name:    payload.Name,
		Payload: payload.Payload,
		Emitter: signalEmitter(inv),
		RunID:   scopedRun,
	})
	if err != nil {
		// An infrastructure failure. The caller compensates the sequence mark
		// and the event-id claim, so the bridge's redelivery emits once —
		// exactly the discipline a terminal commit failure follows.
		deps.commitFailed(ctx, inv, ev, StageEmitSignal, err)
		return CallbackResult{}, err
	}

	// Liveness: an actor that emitted is an actor that is still working, which
	// is precisely the claim this kind exists to make.
	if err := deps.Store.TouchInvocation(ctx, inv.AttemptID, "", deps.now()); err != nil {
		return CallbackResult{}, err
	}

	deps.recordDetail(ctx, inv, TypeSignalEmitted, ev, "", map[string]any{
		"event_id":     emitted.EventID,
		"signal_name":  payload.Name,
		"signal_scope": scopeOrDefault(payload.Scope),
		"emitter":      signalEmitter(inv),
		"resumed":      emitted.Resumed,
		"picked_up":    emitted.PickedUp,
	})
	return CallbackResult{AttemptID: inv.AttemptID, Disposition: DispositionRecorded}, nil
}

func scopeOrDefault(scope string) string {
	if scope == "" {
		return SignalScopeRun
	}
	return scope
}

// signalEmitter is the attribution a mid-execution emission carries, built
// from the VERIFIED invocation record rather than from anything the caller
// sent. §10.4 makes an emitter attribution for operators and never an
// authority claim, but an attribution an actor could forge would not even be
// that: the whole point of routing emission through the attempt-scoped token
// (rather than the namespace-wide event bearer) is that an emission names the
// attempt it actually came from.
func signalEmitter(inv PendingInvocation) string {
	who := inv.ActorRef
	if who == "" {
		who = inv.ActorID
	}
	if who == "" {
		return "node:" + inv.NodeID + "/attempt:" + inv.AttemptID
	}
	return "node:" + inv.NodeID + "/actor:" + who
}

// rejectSignal records an emission this ingest cannot act on. It is a
// DispositionRejected rather than an error for the same reason a rejected
// terminal payload is: the protocol worked, the request did not, and the
// emitting attempt must not be told to retry forever. Crucially it changes
// nothing about the attempt — a malformed emission does not fail the session
// that is still running.
func (d CallbackDeps) rejectSignal(ctx context.Context, inv PendingInvocation, ev CallbackEvent, diagnostic string) (CallbackResult, error) {
	d.record(ctx, inv, TypeCallbackRejected, ev, diagnostic)
	return CallbackResult{AttemptID: inv.AttemptID, Disposition: DispositionRejected, Diagnostic: diagnostic}, nil
}

// commitTerminal is step 5: resume the parked work item under the dispatch's
// fencing tuple and commit through the engine.
func commitTerminal(ctx context.Context, deps CallbackDeps, inv PendingInvocation, ev CallbackEvent) (CallbackResult, error) {
	if deps.Engine == nil {
		return CallbackResult{}, errors.New("actors: HandleCallback requires an engine to commit a terminal event")
	}

	req, diagnostic := completionFor(ctx, deps.Store, inv, ev)
	if diagnostic != "" {
		deps.record(ctx, inv, TypeCallbackRejected, ev, diagnostic)
		return CallbackResult{AttemptID: inv.AttemptID, Disposition: DispositionRejected, Diagnostic: diagnostic}, nil
	}

	if err := deps.Store.ResumeWaitingWork(ctx, inv, deps.resumeLease()); err != nil {
		if errors.Is(err, engine.ErrStaleClaim) {
			return deps.late(ctx, inv, ev, req,
				fmt.Sprintf("attempt %s is no longer parked under fencing token %d attempt %d; the work was reclaimed, cancelled, or already completed",
					inv.AttemptID, inv.FencingToken, inv.Attempt))
		}
		// Nothing to repark: the item was never resumed.
		deps.commitFailed(ctx, inv, ev, StageResume, err)
		return CallbackResult{}, err
	}

	completion, err := deps.Engine.CompleteAttempt(ctx, req)
	if err != nil {
		// The item is leased to a completion that did not happen, and no
		// worker is working it. Park it again whatever the reason. After an
		// infrastructure failure that is exactly where the redelivery expects
		// to find it; after a refusal the node run or run is already terminal,
		// so the item is a leftover for cancellation to reap and parking it
		// only stops it being dispatched again. Leaving it leased is what
		// turned one bad actor_id into a billable session per lease cycle
		// (issue #16).
		deps.repark(ctx, inv)
		if errors.Is(err, engine.ErrStaleClaim) ||
			errors.Is(err, engine.ErrTerminalNodeRun) || errors.Is(err, engine.ErrTerminalRun) {
			// The engine's own fenced guard refused. This is the same
			// §13.4 outcome as a failed resume and gets the same treatment;
			// nothing was written, because the whole §12.5 transaction rolled
			// back.
			return deps.late(ctx, inv, ev, req,
				fmt.Sprintf("engine refused the late completion of attempt %s: %v", inv.AttemptID, err))
		}
		deps.commitFailed(ctx, inv, ev, StageComplete, err)
		return CallbackResult{}, err
	}

	if err := deps.Store.CloseInvocation(ctx, inv.AttemptID, InvocationCompleted); err != nil {
		// Deliberately no repark: the §12.5 transaction committed, the work
		// item is completed, and only the invocation row's own bookkeeping is
		// behind. The redelivery this error invites lands as a late
		// diagnostic, which is the truthful record of it.
		deps.commitFailed(ctx, inv, ev, StageClose, err)
		return CallbackResult{}, err
	}
	// Task t10 (issue #13): the outcome is durable and the invocation is
	// closed; now go and read the ref this event claims was handed over. It
	// runs last, and its failure is never propagated, because an observation
	// is a second fact about an attempt whose completion already committed —
	// see handover.go in this package.
	deps.observeHandover(ctx, completion, ev)
	return CallbackResult{
		AttemptID:   inv.AttemptID,
		Disposition: DispositionCommitted,
		Completion:  &completion,
	}, nil
}

// The stages of step 5, named on a TypeCallbackCommitFailed event so an
// operator reading it knows how far the commit got — in particular whether
// the engine was reached at all, which decides whether the run's own state
// could have moved.
const (
	// StageResume is a failure to re-lease the parked work item.
	StageResume = "resume_waiting_work"
	// StageComplete is a failure inside the engine's §12.5 transaction, which
	// therefore rolled back entirely.
	StageComplete = "complete_attempt"
	// StageClose is a failure to retire the invocation row AFTER the engine
	// committed: workflow state moved, bookkeeping did not.
	StageClose = "close_invocation"
	// StageEmitSignal is a failure to append and deliver a mid-execution
	// `signal` emission. Unlike the three above it says nothing about the
	// emitting attempt, which was never touched: the fact was not written, so
	// the redelivery the compensated claim invites writes it exactly once.
	StageEmitSignal = "emit_signal_event"
	// StageSupersede is a failure to append the superseding attempt record a
	// late terminal callback leaves (task t11). Like StageEmitSignal it says
	// nothing about workflow state — §13.4 had already refused this report a
	// commit — only that the facts it carried were not recorded, which is
	// precisely why it is worth a compensated redelivery rather than a
	// shrug.
	StageSupersede = "record_superseding_attempt"
)

// commitFailed records why a terminal event did not commit. It is recorded
// for infrastructure failures only: a §13.4 refusal is not a failure and gets
// TypeCallbackLate instead.
func (d CallbackDeps) commitFailed(ctx context.Context, inv PendingInvocation, ev CallbackEvent, stage string, cause error) {
	d.recordDetail(ctx, inv, TypeCallbackCommitFailed, ev,
		fmt.Sprintf("terminal event %s for attempt %s failed at %s: %v", ev.EventID, inv.AttemptID, stage, cause),
		map[string]any{"stage": stage, "error": cause.Error()})
}

// repark returns a resumed work item to the parked state, best-effort. A
// failed compensation must not replace the original cause in what the actor
// and the audit log are told.
func (d CallbackDeps) repark(ctx context.Context, inv PendingInvocation) {
	_ = d.Store.ReparkResumedWork(ctx, inv)
}

// record appends a diagnostic event, best-effort. A failure to write an audit
// line must not turn a correctly-handled callback into an error the actor
// will retry forever; the returned dispositions already tell the caller what
// happened.
func (d CallbackDeps) record(ctx context.Context, inv PendingInvocation, eventType string, ev CallbackEvent, diagnostic string) {
	d.recordDetail(ctx, inv, eventType, ev, diagnostic, nil)
}

// recordDetail is record with event-type-specific fields merged in. They are
// merged rather than nested so a consumer reading events does not need to know
// which type carries a sub-object.
func (d CallbackDeps) recordDetail(
	ctx context.Context, inv PendingInvocation, eventType string, ev CallbackEvent,
	diagnostic string, extra map[string]any,
) {
	data := map[string]any{
		"run_id":      inv.RunID,
		"node_run_id": inv.NodeRunID,
		"node_id":     inv.NodeID,
		"attempt_id":  inv.AttemptID,
		"event_id":    ev.EventID,
		"sequence":    ev.Sequence,
		"kind":        string(ev.Kind),
	}
	if inv.InvocationID != "" {
		data["invocation_id"] = inv.InvocationID
	}
	if diagnostic != "" {
		data["detail"] = diagnostic
	}
	for k, v := range extra {
		data[k] = v
	}
	_ = d.Store.AppendRunEvent(ctx, inv.NamespaceID, inv.RunID, eventType, data)
}

// completionFor turns a terminal §13.4 event into the engine completion it
// claims. The second return value is non-empty when the payload cannot be
// acted on at all, in which case no completion is attempted.
//
// Note what is *not* checked here: whether the outcome is one the node
// declares, and whether the output satisfies its schema. Both are the
// engine's job (§12.5 step 2), and duplicating them would create a second
// place for the answer to differ from the authoritative one.
func completionFor(ctx context.Context, store CallbackStore, inv PendingInvocation, ev CallbackEvent) (engine.CompletionRequest, string) {
	req := engine.CompletionRequest{
		WorkID:       inv.WorkID,
		WorkerID:     inv.WorkerID,
		FencingToken: inv.FencingToken,
		Attempt:      inv.Attempt,
		ActorID:      inv.ActorID,
	}

	switch ev.Kind {
	case EventCompleted, EventBlocked:
		var payload CompletedPayload
		if len(ev.Payload) > 0 {
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return req, fmt.Sprintf("%s event %s carries a payload that is not a §13.2 result body: %v", ev.Kind, ev.EventID, err)
			}
		}
		outcome := payload.Outcome
		if outcome == "" && ev.Kind == EventBlocked {
			// §13.4 names `blocked` as an event kind; the domain outcome it
			// routes as is `blocked` unless the actor said otherwise. Whether
			// the node declares that outcome is the engine's call.
			outcome = "blocked"
		}
		if outcome == "" {
			return req, fmt.Sprintf("completed event %s declares no domain outcome", ev.EventID)
		}
		req.TechStatus = engine.StatusSucceeded
		req.Outcome = outcome
		req.Output = MergeWorkspaceMeasured(payload.Output, payload.WorkspaceMeasured)
		if payload.LedgerDelta != nil {
			req.LedgerDelta = append([]ledger.Record(nil), payload.LedgerDelta.Records...)
		}
		req.Usage = payload.Usage.ToEngine()
		req.TerminationReason = payload.TerminationReason
		req.ContinuationRef = payload.ContinuationRef
		if detail := originActorMismatch(ctx, store, req.LedgerDelta, inv.ActorID); detail != "" {
			req.TechStatus = engine.StatusContractRejected
			req.Outcome = ""
			req.Output = identityDiagnostic(detail)
			req.LedgerDelta = nil
			req.RetryRefusal = detail
			return req, ""
		}
		// The handle for continuing this conversation, reported by an actor
		// that finished late. Persisting it here is what makes continuation
		// reachable on the path long sessions actually take (ADR 0010 §2);
		// absent stays absent, and nothing invents one.
		return req, ""

	case EventFailed:
		var payload FailedPayload
		if len(ev.Payload) > 0 {
			_ = json.Unmarshal(ev.Payload, &payload)
		}
		class := payload.Class
		if !class.Valid() {
			class = ClassExecution
		}
		req.TechStatus = TechStatusFor(class)
		if req.TechStatus == engine.StatusTimedOut {
			// A §13.4 terminal event is the actor reporting that ITS
			// invocation is over — the one timeout origin that leaves no
			// session to fence a retry against (task t10). Every other
			// producer of timed_out either is the control plane's own
			// wall-clock verdict or cannot say, and neither is retried.
			req.TimeoutOrigin = engine.TimeoutOriginActor
		}
		// The failure diagnostic and the workspace measurement are different
		// facts about the same attempt: the session failed, AND the bridge
		// measured (or honestly could not measure) what it left behind.
		req.Output = MergeWorkspaceMeasured(
			failureOutput(class, payload.Message, payload.Detail), payload.WorkspaceMeasured)
		req.Usage = payload.Usage.ToEngine()
		// The provider's own reason for ending the turn, which is not the
		// §13.5 class the control plane assigned it and can arrive with no
		// usage block at all (ADR 0009).
		req.TerminationReason = payload.TerminationReason
		// The bridge's own report of what preserve-on-failure did (task
		// t25/t26), nil unless it actually committed a branch — see
		// Preserve.ToEngine's gating.
		req.Preserve = payload.Preserve.ToEngine()
		return req, ""
	}

	return req, fmt.Sprintf("event kind %q is not terminal", ev.Kind)
}

// failureOutput is the diagnostic body recorded on a failed attempt.
//
// CompletionRequest has no diagnostic field, and the engine stores Output on
// the attempt row whatever the status is, so this is where a failure's reason
// becomes durable and readable. It is a small, fixed shape: an attempt result
// is not a log sink.
func failureOutput(class ErrorClass, message, detail string) json.RawMessage {
	payload := struct {
		Error struct {
			Class   string `json:"class"`
			Message string `json:"message,omitempty"`
			Detail  string `json:"detail,omitempty"`
		} `json:"error"`
	}{}
	payload.Error.Class = string(class)
	payload.Error.Message = message
	payload.Error.Detail = detail
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{"error":{"class":"execution"}}`)
	}
	return encoded
}
