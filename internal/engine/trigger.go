package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TriggerWorkflow is the immutable published definition offered to an event
// delivery. Delivery selects only the newest version of each workflow key.
type TriggerWorkflow struct {
	Digest, SourceFormat, Source string
	IR                           json.RawMessage
}

// TriggeredRun reports a handler that matched, and which run the event
// belongs to as a result.
type TriggeredRun struct {
	WorkflowDigest string `json:"workflow_digest"`
	RunID          string `json:"run_id"`
	// Attached is true when RunID names a run that ALREADY existed for this
	// event's subject (task t15, spec c31/h16) -- the trigger matched but no
	// new run was created; the event was recorded on the existing run
	// instead. False (the default) means RunID is a brand-new run, exactly
	// this field's pre-task-t15 behavior.
	Attached bool `json:"attached,omitempty"`
	// Deferred is true when the workflow's configured MaxConcurrentSubjectRuns
	// ceiling (task t16, spec c36/h21) had no headroom for a NEW subject: no
	// run was created and RunID is empty. DeferredTriggerID names the queued
	// row a later matching event (for the same subject) or a run of this
	// workflow reaching a terminal state (DrainSubjectTriggerQueue, for any
	// subject) will drain into a run.
	Deferred bool `json:"deferred,omitempty"`
	// DeferredTriggerID is set exactly when Deferred is true.
	DeferredTriggerID string `json:"deferred_trigger_id,omitempty"`
}

// DeferredTrigger is one subject-bearing trigger event TriggerEvent could
// not turn into a run because its workflow's configured
// MaxConcurrentSubjectRuns ceiling (task t16, spec c36/h21) had no headroom.
//
// It pins everything createTriggeredRunTx needs to create the run LATER,
// exactly as it would have at match time: the workflow version this event
// matched against (digest, source, and normalized IR -- not just the
// digest, so draining never has to guess which workflow_versions row a
// later republish might have superseded), and the triggering event itself
// (PickupEvent's fields, individually, because PickupEvent is not itself
// storable). A subject can hold at most one queued row at a time -- a
// second matching event for a subject already queued replaces this row's
// event fields with the newer ones rather than creating a sibling entry
// (see TriggerEvent) -- so CreatedAt is this subject's original arrival
// into the queue and is what FIFO drain order (OldestDeferredTrigger) is
// computed from, even after a replace.
type DeferredTrigger struct {
	ID             string
	WorkflowKey    string
	WorkflowDigest string
	SourceFormat   string
	Source         string
	NormalizedIR   json.RawMessage
	Subject        string
	TriggerEventID string
	EventName      string
	EventEmitter   string
	EventPayload   json.RawMessage
	// Attempts counts how many matching events this queue entry has
	// absorbed, starting at 1 -- purely an operator-visible fact (how
	// contested this subject's headroom is), never consulted by any
	// decision.
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeferredTriggerInput is what TriggerEvent writes when it queues (or
// refreshes) one subject's deferred trigger.
type DeferredTriggerInput struct {
	WorkflowKey    string
	WorkflowDigest string
	SourceFormat   string
	Source         string
	NormalizedIR   json.RawMessage
	Subject        string
	TriggerEventID string
	EventName      string
	EventEmitter   string
	EventPayload   json.RawMessage
}

// EventTriggerRunner is the engine slice used by signal delivery.
type EventTriggerRunner interface {
	TriggerEvent(context.Context, Tx, TriggerWorkflow, PickupEvent) ([]TriggeredRun, error)
}

var _ EventTriggerRunner = (*Engine)(nil)

// TriggerEvent evaluates the definition's handlers and creates one run for
// each match. The event payload is the run input. It is called inside the
// signal delivery transaction, after the immutable fact has been appended.
func (e *Engine) TriggerEvent(ctx context.Context, tx Tx, candidate TriggerWorkflow, ev PickupEvent) ([]TriggeredRun, error) {
	wf, err := e.Workflow(candidate.Digest, candidate.IR)
	if err != nil {
		return nil, err
	}
	var out []TriggeredRun
	for _, trigger := range wf.Triggers {
		if trigger.OnEvent != ev.Name {
			continue
		}
		if trigger.Condition != nil {
			value, _, evalErr := trigger.Condition.Eval(map[string]any{
				celVarInput: map[string]any{}, celVarOutput: map[string]any{}, celVarOutcome: "",
				celVarEvent: map[string]any{"name": ev.Name, "emitter": ev.Emitter, "payload": decodeActivation(ev.Payload)},
			})
			if evalErr != nil {
				return nil, fmt.Errorf("trigger condition %q: %w", trigger.When, evalErr)
			}
			ok, err := truthy(value)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if err := validatePayload(wf.InputSchema, ev.Payload); err != nil {
			return nil, &ContractError{What: "triggered run input", Detail: err.Error()}
		}

		// One-active-run-per-subject (task t15, spec c31/h16). Measuring this
		// code at HEAD found no subject/correlation concept anywhere on the
		// inbound event path -- this trigger loop created a new run for every
		// matching event, so a second state-change or comment on the same
		// Jira issue produced a sibling run rather than resuming the one
		// already in flight. The fix lives HERE, the one place that already
		// knows both "this event matches this workflow's trigger" and "a run
		// is about to be created for it" -- attaching anywhere else would mean
		// re-deriving the match.
		//
		// The advisory lock is taken BEFORE the read so two concurrent
		// deliveries for the same subject cannot both see "no active run" and
		// both create one; it uses the same hashtextextended scheme every
		// other advisory lock in this store does (ledger.RunLockKey,
		// DeliverSignalEvent's watermark lock), scoped to
		// (namespace, workflow key, subject) so it can never collide with a
		// run-id lock or another subject's lock.
		//
		// A caller that supplies no subject (every caller that predates this
		// task) gets exactly the old behavior: ev.Subject == "" skips this
		// block entirely, and a run is created every time, as before.
		if ev.Subject != "" {
			// task t16 (spec c36/h21), the cross-issue ceiling built ON this
			// per-issue floor. A configured MaxConcurrentSubjectRuns has to be
			// checked against every OTHER subject's count too, not just this
			// one's -- so two concurrent deliveries for two DIFFERENT fresh
			// subjects must not both read "count below the ceiling" and both
			// create a run, overshooting it. That needs a lock scoped to the
			// WORKFLOW, not the subject, and it is taken FIRST, before the
			// per-subject lock below: DrainSubjectTriggerQueue (complete.go's
			// and humandecision.go's terminal-transition callers) takes the
			// identical two locks in the identical order, so this function and
			// that one can never deadlock against each other by holding them
			// in reverse order. It is taken only when the ceiling is actually
			// configured, so a workflow that never sets it pays for exactly
			// the one advisory lock t15 always took -- byte-for-byte the
			// pre-t16 behavior.
			if wf.Limits.MaxConcurrentSubjectRuns > 0 {
				if err := tx.Lock(ctx, triggerWorkflowLockKey(e.store.NamespaceID(), wf.Name)); err != nil {
					return nil, err
				}
			}
			if err := tx.Lock(ctx, triggerSubjectLockKey(e.store.NamespaceID(), wf.Name, ev.Subject)); err != nil {
				return nil, err
			}
			active, found, err := tx.ActiveRunBySubject(ctx, wf.Name, ev.Subject)
			if err != nil {
				return nil, err
			}
			if found {
				// "The second event's effect is visible on the existing run,
				// not a sibling" (h16): the attaching event is recorded on the
				// EXISTING run's own stream, not merely dropped or logged
				// elsewhere. No new run, token, or node run is created.
				if _, err := tx.AppendEvent(ctx, active.ID, event(TypeTriggerEventAttached, map[string]any{
					"run_id": active.ID, "trigger_event_id": ev.ID, "event_name": ev.Name,
					"emitter": ev.Emitter, "subject": ev.Subject, "workflow_key": wf.Name,
					"workflow_digest": candidate.Digest,
				})); err != nil {
					return nil, err
				}
				out = append(out, TriggeredRun{WorkflowDigest: candidate.Digest, RunID: active.ID, Attached: true})
				continue
			}

			if wf.Limits.MaxConcurrentSubjectRuns > 0 {
				deferredIn := DeferredTriggerInput{
					WorkflowKey: wf.Name, WorkflowDigest: candidate.Digest,
					SourceFormat: candidate.SourceFormat, Source: candidate.Source, NormalizedIR: candidate.IR,
					Subject: ev.Subject, TriggerEventID: ev.ID, EventName: ev.Name,
					EventEmitter: ev.Emitter, EventPayload: ev.Payload,
				}
				// Checked BEFORE the count, and unconditionally on the count's
				// answer: a subject already queued must never also fall
				// through to createTriggeredRunTx just because some OTHER
				// subject's completion happened to bring the count back under
				// the ceiling in the meantime. That would leave a stale queue
				// row that later drains into a SECOND run for a subject that
				// by then already has one -- exactly the sibling-run bug t15
				// fixed, reintroduced through the queue. Refreshing here keeps
				// the invariant this whole feature leans on: a subject holds
				// an active run XOR a queued entry, never both, never
				// neither-then-both.
				already, isQueued, err := tx.FindDeferredTrigger(ctx, wf.Name, ev.Subject)
				if err != nil {
					return nil, err
				}
				if isQueued {
					if err := tx.TouchDeferredTrigger(ctx, already.ID, deferredIn); err != nil {
						return nil, err
					}
					out = append(out, TriggeredRun{WorkflowDigest: candidate.Digest, Deferred: true, DeferredTriggerID: already.ID})
					continue
				}

				count, err := tx.ActiveSubjectRunCount(ctx, wf.Name)
				if err != nil {
					return nil, err
				}
				if count >= wf.Limits.MaxConcurrentSubjectRuns {
					row, err := tx.InsertDeferredTrigger(ctx, deferredIn)
					if err != nil {
						return nil, err
					}
					out = append(out, TriggeredRun{WorkflowDigest: candidate.Digest, Deferred: true, DeferredTriggerID: row.ID})
					continue
				}
			}
		}

		run, err := e.createTriggeredRunTx(ctx, tx, wf, candidate, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, TriggeredRun{WorkflowDigest: candidate.Digest, RunID: run.ID})
	}
	return out, nil
}

// triggerSubjectLockKey is the advisory-lock key TriggerEvent takes before
// deciding whether an event's subject already has an active run. It is
// scoped to the namespace and workflow key (not just the subject) so a
// subject string that happens to collide across two different workflows, or
// two different namespaces, cannot make one workflow's dedup decision block
// on -- or be satisfied by -- another's run.
func triggerSubjectLockKey(namespaceID, workflowKey, subject string) string {
	return "trigger-subject:" + namespaceID + ":" + workflowKey + ":" + subject
}

// triggerWorkflowLockKey is task t16's coarser sibling of
// triggerSubjectLockKey: it serializes every subject's trigger decision for
// one workflow against every other subject's, which is what makes a
// cross-subject ceiling (MaxConcurrentSubjectRuns) enforceable at all --
// see TriggerEvent's doc comment at its call site for the deadlock-ordering
// argument. Only ever taken while the ceiling is configured, so an
// installation that does not use it never pays for the extra lock.
func triggerWorkflowLockKey(namespaceID, workflowKey string) string {
	return "trigger-workflow:" + namespaceID + ":" + workflowKey
}

// DrainSubjectTriggerQueue is task t16's other half (spec c36/h21): called
// once, inside the SAME §12.5 transaction that just recorded a
// subject-bearing run reaching a terminal state, it tries to turn the
// workflow's longest-queued deferred trigger -- the entry TriggerEvent
// wrote when MaxConcurrentSubjectRuns had no headroom for a NEW subject --
// into a run, now that this termination has freed exactly one slot.
//
// Every terminal-transition call site in this package (completion.cancel /
// completeRun / failRun / failBound, humanTaskDecision's equivalents) calls
// this unconditionally rather than guarding the call site, so a call site
// added later cannot silently forget to drain. The guard lives HERE
// instead: a run with no subject, or a workflow with no configured
// ceiling, changed nothing about subject-run capacity, and both are
// no-ops.
//
// It drains AT MOST ONE entry, because one terminal subject-bearing run
// frees exactly one slot -- draining more would let the active count run
// past the ceiling the same way skipping the check on creation would.
//
// It does not re-check ActiveRunBySubject for the popped subject before
// creating its run. That is deliberate, not an oversight: TriggerEvent
// only ever writes or refreshes a deferred-trigger row for a subject that,
// at that moment and under the same workflow lock this function also
// takes, had NEITHER an active run NOR an existing queue entry (see its
// own doc comment) -- so "a subject holds an active run XOR a queued
// entry, never both" is an invariant established there, not an assumption
// made here, and the workflow lock is what keeps a concurrent TriggerEvent
// call from being able to violate it between the pop and the create below.
func (e *Engine) DrainSubjectTriggerQueue(ctx context.Context, tx Tx, wf *Workflow, run Run) error {
	if run.Subject == "" || wf.Limits.MaxConcurrentSubjectRuns <= 0 {
		return nil
	}
	if err := tx.Lock(ctx, triggerWorkflowLockKey(e.store.NamespaceID(), wf.Name)); err != nil {
		return err
	}
	deferred, found, err := tx.OldestDeferredTrigger(ctx, wf.Name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := tx.DeleteDeferredTrigger(ctx, deferred.ID); err != nil {
		return err
	}
	// The subject lock too, in the SAME order TriggerEvent takes it
	// (workflow lock already held, subject lock second): this call and
	// TriggerEvent are provably the same locking protocol, not two
	// protocols that merely happen to agree today.
	if err := tx.Lock(ctx, triggerSubjectLockKey(e.store.NamespaceID(), wf.Name, deferred.Subject)); err != nil {
		return err
	}

	// The deferred row pins the EXACT workflow version its triggering event
	// matched against, which may differ from the terminating run's own wf if
	// a republish landed while this entry waited -- so the run this creates
	// is loaded and pinned from the queue row, never from the caller's wf.
	dwf, err := e.Workflow(deferred.WorkflowDigest, deferred.NormalizedIR)
	if err != nil {
		return fmt.Errorf("engine: drain subject trigger queue: load workflow version %s: %w", deferred.WorkflowDigest, err)
	}
	candidate := TriggerWorkflow{
		Digest: deferred.WorkflowDigest, SourceFormat: deferred.SourceFormat,
		Source: deferred.Source, IR: deferred.NormalizedIR,
	}
	fact := PickupEvent{
		ID: deferred.TriggerEventID, Name: deferred.EventName, Emitter: deferred.EventEmitter,
		Payload: deferred.EventPayload, Subject: deferred.Subject,
	}
	newRun, err := e.createTriggeredRunTx(ctx, tx, dwf, candidate, fact)
	if err != nil {
		return err
	}
	// Evidence that this run started from the queue rather than at first
	// match, on the run's OWN stream beside everything else that happened to
	// it -- there was no run to record this against while it waited, so the
	// deferred_triggers row (and its created_at) was the only trace until
	// now.
	if _, err := tx.AppendEvent(ctx, newRun.ID, event(TypeTriggerQueueDrained, map[string]any{
		"run_id": newRun.ID, "deferred_trigger_id": deferred.ID, "subject": deferred.Subject,
		"workflow_key": wf.Name, "queued_at": deferred.CreatedAt, "queued_attempts": deferred.Attempts,
		"freed_by_run_id": run.ID,
	})); err != nil {
		return err
	}
	return nil
}

func (e *Engine) createTriggeredRunTx(ctx context.Context, tx Tx, wf *Workflow, candidate TriggerWorkflow, ev PickupEvent) (Run, error) {
	now := e.now().UTC()
	// Affinity is resolved HERE, once, at run creation -- not per dispatch.
	// The triggering event is the only thing the conditions are written
	// against, and it exists exactly once in a run's life. Resolving it later
	// would mean re-reading an event that may since have been superseded, and
	// would make the recorded routing a derivation rather than a decision.
	affinity, err := wf.ResolveAffinity(ev)
	if err != nil {
		return Run{}, &ContractError{What: "triggered run actor affinity", Detail: err.Error()}
	}
	run := Run{ID: e.newID(), NamespaceID: e.store.NamespaceID(), WorkflowDigest: candidate.Digest,
		State: RunRunning, Input: jsonOrNull(ev.Payload), CreatedAt: now, UpdatedAt: now,
		ActorAffinity: affinityJSON(affinity), Subject: ev.Subject}
	format := candidate.SourceFormat
	if format != string(compiler.FormatJSON) {
		format = string(compiler.FormatYAML)
	}
	versionID, err := tx.EnsureWorkflowVersion(ctx, WorkflowVersionInput{WorkflowKey: wf.Name,
		SourceFormat: format, Source: candidate.Source, NormalizedIR: candidate.IR, ContentDigest: candidate.Digest})
	if err != nil {
		return Run{}, err
	}
	run.WorkflowVersionID = versionID
	if err := tx.InsertRun(ctx, run); err != nil {
		return Run{}, err
	}
	if err := tx.Lock(ctx, ledger.RunLockKey(run.ID)); err != nil {
		return Run{}, err
	}
	token := Token{ID: e.newID(), NamespaceID: run.NamespaceID, RunID: run.ID, NodeID: wf.Entry, State: TokenActive, CreatedAt: now}
	if err := tx.InsertToken(ctx, token); err != nil {
		return Run{}, err
	}
	entry := wf.Nodes[wf.Entry]
	nr := NodeRun{ID: e.newID(), NamespaceID: run.NamespaceID, RunID: run.ID, TokenID: token.ID,
		NodeID: wf.Entry, State: dispatchState(entry.Kind), VisitCount: 1, CreatedAt: now, UpdatedAt: now}
	if err := tx.InsertNodeRun(ctx, nr); err != nil {
		return Run{}, err
	}
	if err := e.materializeEventRoutes(ctx, tx, wf, run, now); err != nil {
		return Run{}, err
	}
	workID, humanTaskID, err := e.dispatchNode(ctx, tx, entry, run, nr, "", "", now)
	if err != nil {
		return Run{}, err
	}
	if _, err = tx.AppendEvent(ctx, run.ID, event(TypeRunCreated, map[string]any{
		"run_id": run.ID, "workflow_version_id": versionID, "workflow_digest": candidate.Digest,
		"workflow_key": wf.Name, "entry": wf.Entry, "trigger_event_id": ev.ID,
		// The resolved routing goes into the run's own event stream as well
		// as its column: the column is what a query joins on, the event is
		// what a reader of the run sees in order beside everything else that
		// happened. Omitted entirely when nothing resolved.
		"actor_affinity": affinity,
	})); err != nil {
		return Run{}, err
	}
	if humanTaskID != "" {
		_, err = tx.AppendEvent(ctx, run.ID, event(TypeHumanTaskCreated, map[string]any{"run_id": run.ID, "node_run_id": nr.ID, "node_id": nr.NodeID, "token_id": token.ID, "human_task_id": humanTaskID, "visit": 1}))
	} else {
		_, err = tx.AppendEvent(ctx, run.ID, event(TypeNodeRunReady, map[string]any{"run_id": run.ID, "node_run_id": nr.ID, "node_id": nr.NodeID, "token_id": token.ID, "work_id": workID, "visit": 1}))
	}
	return run, err
}
