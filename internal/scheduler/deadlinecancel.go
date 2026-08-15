package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Propagating an expired deadline to the in-flight actor session it timed out
// (task t9's cancel, task t12's event).
//
// This is the THIRD place in the system that asks an actor to stop, and it is
// deliberately built as the sibling of the other two rather than as a
// variation on them: internal/api/cancelpropagate.go (an operator cancelled
// the run) and internal/worker/branchcancel.go (a barrier reaped a losing
// branch). All three send the same best-effort §13.6 request, all three
// record what they attempted whether or not it landed, and all three keep
// that work out of the transaction that committed the state. What separates
// them is WHO DECIDED — and both siblings' comments say in so many words that
// origin, not mechanism, is the axis. The event type below follows that.
//
// The one thing this file does that neither sibling has to: it runs from a
// process whose durable state is already committed AND whose caller is a
// singleton serial tick loop, so it is invoked on its own goroutine (see
// fireOne) and nothing it does can be allowed to fail a timer.

var deadlineActorClient = actors.NewClient()

const deadlineCancelTimeout = 30 * time.Second

// TypeDeadlineCancelRequested records that an expired deadline asked an actor
// to stop an in-flight invocation (task t12, spec claim c46). It is the THIRD
// cancel-requested type, beside internal/api's
// dev.culture.nodes.actor.cancel-requested and internal/worker's
// dev.culture.nodes.branch.cancel-requested.
//
// The axis those two are separated on is ORIGIN, and this one follows it
// rather than adding a second axis. Read their comments: the API's type is
// distinct from run.cancelled because one is what the control plane SENT and
// the other what it DID; the worker's is distinct from branch.cancelled for
// exactly the same reason. Neither is distinguished by MECHANISM — all three
// send the identical §13.6 POST .../cancel, all three are best-effort, all
// three record the attempt whether or not it landed. What differs is who
// decided: an operator cancelling a run, a barrier reaping a losing sibling,
// and — here — a wall clock the workflow author declared running out.
//
// Reusing either existing type would make "did the timeout stop this session,
// or did an operator?" unanswerable from the event stream, and that question
// is precisely what a run has to answer on its own for #87's
// self-explaining-run acceptance. The name's first segment is the origin, the
// way `branch.` is, because that is the thing a reader needs to tell apart.
//
// It lives here rather than in internal/engine's event list for the same
// reason the other two live beside their emitters: the engine does not send
// this, the scheduler does, and internal/engine/events.go's own doc comment
// scopes that list to the engine's vocabulary. There is no shared
// cancel-requested home to put it in, and inventing one would spread a single
// origin's event across two packages.
const TypeDeadlineCancelRequested = "dev.culture.nodes.deadline.cancel-requested"

// registryFor mirrors engineFor: timers are deployment-wide while actor
// registrations are namespace-scoped, so one scheduler lazily retains one
// registry for every namespace whose deadline it has processed.
func (sch *Scheduler) registryFor(namespaceID string) (worker.Registry, error) {
	sch.registryMu.Lock()
	defer sch.registryMu.Unlock()
	if registry, ok := sch.registries[namespaceID]; ok {
		return registry, nil
	}
	registry, err := worker.NewDBRegistry(sch.db, namespaceID)
	if err != nil {
		return nil, err
	}
	if sch.registries == nil {
		sch.registries = make(map[string]worker.Registry)
	}
	sch.registries[namespaceID] = registry
	return registry, nil
}

// cancelDeadlineInvocation asks the actor behind an expired deadline to stop,
// then records exactly one TypeDeadlineCancelRequested event whatever
// happened -- including when nothing was sent at all.
//
// The shape is deliberately the same as its two siblings
// (internal/api.cancelOneInvocation and internal/worker.cancelReapedInvocation):
// the same outcome vocabulary (sent / failed / skipped), the same payload
// keys, one event per invocation, and no error ever returned. An
// attempted-but-failed cancel is still evidence worth keeping -- more so
// here than anywhere else, because a deadline cancel that never landed is the
// difference between a session that stopped and one still burning tokens in
// the dark, and the attempt row already says `timed_out` either way.
//
// It runs post-commit and off the tick loop (see fireOne), so nothing here
// can stall a timer or change what was committed.
func (sch *Scheduler) cancelDeadlineInvocation(ctx context.Context, inv actors.PendingInvocation) {
	var outcome, detail string

	switch {
	case inv.ActorRef == "":
		outcome, detail = "skipped", "invocation names no actor_ref to resolve"
	case inv.InvocationID == "":
		outcome, detail = "skipped", "invocation has no actor-assigned invocation_id to cancel"
	default:
		registry, err := sch.registryFor(inv.NamespaceID)
		if err != nil {
			outcome, detail = "failed", fmt.Sprintf("build registry for namespace %s: %v", inv.NamespaceID, err)
			break
		}
		endpoint, err := registry.Resolve(ctx, inv.ActorRef)
		if err != nil {
			outcome, detail = "failed", fmt.Sprintf("resolve actor %q: %v", inv.ActorRef, err)
			break
		}
		cancelCtx, cancel := context.WithTimeout(ctx, deadlineCancelTimeout)
		cancelErr := deadlineActorClient.Cancel(cancelCtx, endpoint, inv.InvocationID,
			fmt.Sprintf("deadline expired for attempt %s", inv.AttemptID))
		cancel()
		if cancelErr != nil {
			outcome, detail = "failed", cancelErr.Error()
		} else {
			outcome = "sent"
		}
	}

	sch.recordDeadlineCancel(ctx, inv, outcome, detail)
}

// recordDeadlineCancel appends the one audit event cancelDeadlineInvocation
// promises. Best-effort in both directions: the timer transaction committed
// long ago, and a diagnostic that failed to append must not surface anywhere
// a tick could see it.
func (sch *Scheduler) recordDeadlineCancel(ctx context.Context, inv actors.PendingInvocation, outcome, detail string) {
	if inv.RunID == "" {
		// Nowhere to record against: the event log is keyed by run. applyEffect
		// already filters this case out, so reaching here means a direct
		// caller (a test) handed over an invocation with no run.
		return
	}
	data, err := json.Marshal(map[string]any{
		"run_id":        inv.RunID,
		"node_run_id":   inv.NodeRunID,
		"node_id":       inv.NodeID,
		"attempt_id":    inv.AttemptID,
		"actor_ref":     inv.ActorRef,
		"invocation_id": inv.InvocationID,
		"outcome":       outcome,
		"detail":        detail,
	})
	if err != nil {
		data = []byte(`{}`)
	}
	_, _ = sch.db.InsertEvent(ctx, postgres.InsertEventInput{
		NamespaceID:   inv.NamespaceID,
		AggregateType: "run",
		AggregateID:   inv.RunID,
		EventType:     TypeDeadlineCancelRequested,
		Data:          data,
	})
}
