package worker

import (
	"context"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/handover"
)

// The handover-evidence seam (task t10, issue #13).
//
// An agent node's dispatch produces no runners.Result, so
// internal/runners/dispatch.go's buildEvidence — the one thing in this
// codebase that has ever built an `observed` record from a dispatch — cannot
// reach it. Everything an agent reports about its own work arrives as §13.2
// content and is capped at `proposed` by PRD §10.4. That is the correct cap
// and this does not lift it: what it adds is a SECOND record, next to the
// agent's claim, saying what the control plane found when it went and looked.
//
// This lives beside appendHookEvidence (hooks.go) for the same reason that
// one does not ride inside engine.CompletionRequest.LedgerDelta: the delta is
// the NODE's declared ledger permission, and an agent node can never declare
// `observe` at all (internal/compiler/ledger.go). This observation is the
// control plane's own, made after the completion committed, so it is appended
// straight through the ledger — and a failure to append is reported, never
// allowed to unwind an outcome that already landed.
//
// The asynchronous twin is internal/actors/callback.go's own call into the
// same Observer. Production dispatches `always_async`, so the callback path
// is the one that carries this in practice; both exist because both terminal
// paths are real.

// observeHandover measures the ref a completed dispatch claims to have handed
// over, and appends what it measured as observed evidence.
//
// It is called only on a SUCCEEDED completion: a failed attempt's work goes to
// a preserve branch (a different bridge-side mechanism entirely), and a
// handover ref asserts a deliverable. Everything else is delegated to the
// Observer, including the decision to write nothing — see internal/handover's
// package doc for why an unfetchable ref must produce no record rather than a
// record marked unmeasured.
func (w *Worker) observeHandover(ctx context.Context, completion engine.CompletionResult, reported *actors.Handover) {
	if w.opts.Handover == nil || completion.AttemptID == "" {
		return
	}
	ref, ok := reported.ClaimedRef()
	if !ok {
		return
	}
	w.opts.Handover.Observe(ctx, handover.Claim{
		RunID:     completion.RunID,
		NodeRunID: completion.NodeRunID,
		AttemptID: completion.AttemptID,
		Ref:       ref,
	})
}

// HandoverObserver builds the Observer a worker (or an API server's callback
// deps) uses, wiring this package's error reporting into it so a fetch that
// could not happen is visible to an operator without becoming a ledger row.
//
// A nil return is the honest answer to "no remote is configured": a
// deployment that cannot fetch cannot measure, and must therefore record
// nothing at all.
func HandoverObserver(fetcher handover.Fetcher, ledgerAppender handover.Appender, actorID, actorRevision string, onError func(error)) *handover.Observer {
	if fetcher == nil || ledgerAppender == nil || actorID == "" {
		return nil
	}
	return &handover.Observer{
		Fetcher:       fetcher,
		Ledger:        ledgerAppender,
		ActorID:       actorID,
		ActorRevision: actorRevision,
		OnError: func(err error) {
			if onError != nil {
				onError(fmt.Errorf("worker: handover evidence: %w", err))
			}
		},
	}
}
