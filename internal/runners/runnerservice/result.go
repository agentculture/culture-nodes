package runnerservice

import (
	"errors"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// Terminal results this service has to build itself, and the honesty rules
// they follow.
//
// Two situations leave the service holding an operation it has already
// answered 202 for, with no result from the runner:
//
//  1. The wrapped runner refused the operation after acceptance (a
//     *runners.DispatchError), or failed in a way it could not classify.
//  2. The process restarted while the operation was in flight, so the runner
//     that held it is gone and nothing observed the outcome.
//
// In both, the status must reach a terminal state — a caller cannot poll
// forever, and reporting `running` for an operation this process no longer
// holds would be false. A terminal state requires a result, so the service
// builds one. What keeps that from being a fabrication is the result
// document's own honesty block: every observation says measured:false,
// complete:false with a note explaining why, there is no exit (nothing
// exited), changes.complete is false (nothing was compared), and the error
// names the kind. The only facts stated positively are the operation's own
// pins — its runner revision, image digest, policy digest — which are read
// off the submitted document, not measured.
//
// This is the one place where the protocol document's "a runner must not
// synthesize [a result] either" needs reading in context. That sentence
// governs the dispatch response: a transport, auth, or refusal failure at the
// HTTP boundary is an error status with no result, never a fabricated failed
// one — and this service holds to that for every refusal it can decide
// synchronously (see handleExecute). What it cannot do is un-send a 202. Once
// sent, the schema's own `rejected` state — described there as an operation
// that need not report an exit because it did not run — is the honest
// terminal answer, and a result that claims nothing is not a fabrication.
// The alternative, leaving the status non-terminal until the caller's
// waiting_external deadline expires, tells the caller strictly less and
// leaves a lie ("running") on the wire while it does so.

// unmeasuredResult builds a terminal result for an operation this service did
// not observe executing.
func unmeasuredResult(record Record, state runners.State, kind runners.ErrorKind, message, note string,
	started, finished time.Time) runners.Result {
	observation := runners.Observation{Measured: false, Complete: false, Note: note}
	return runners.Result{
		OperationID: record.OperationID,
		State:       state,
		Timing: runners.Timing{
			StartedAt:  started.UTC(),
			FinishedAt: finished.UTC(),
			DurationMs: durationMs(started, finished),
		},
		Environment: runners.Environment{
			RunnerRevision: record.Replay.RunnerRevision,
			ImageDigest:    record.Replay.ImageDigest,
			InputDigest:    record.Replay.InputDigest,
			PolicyDigest:   record.Replay.PolicyDigest,
		},
		Changes: runners.Changes{Complete: false},
		Observations: runners.Observations{
			ExitStatus:    observation,
			ChangedPaths:  observation,
			Logs:          observation,
			ResourceUsage: observation,
		},
		Error: &runners.ResultError{Kind: kind, Retryable: kind.Retryable(), Message: message},
	}
}

// refusalResult is the terminal result for a runner refusal or failure the
// service learned about only after it had answered 202.
func refusalResult(record Record, err error, started, finished time.Time) (runners.State, runners.Result) {
	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		// A Runner that returns neither a Result nor a *DispatchError has
		// broken its own contract, and the service genuinely does not know
		// whether anything ran. runner_unavailable is the retryable, honest
		// classification for "this runner did not tell us".
		return runners.StateFailed, unmeasuredResult(record, runners.StateFailed,
			runners.ErrorRunnerUnavailable,
			"the runner returned an unclassified error: "+err.Error(),
			"the runner returned neither a result nor a classified dispatch error, so nothing about this "+
				"operation's execution was observed",
			started, finished)
	}

	state := stateForErrorKind(dispatchErr.Kind)
	return state, unmeasuredResult(record, state, dispatchErr.Kind, dispatchErr.Error(),
		"the runner refused or failed the operation after it had been accepted; no execution was observed",
		started, finished)
}

// interruptedResult is the terminal result for an operation that was still in
// flight when this process stopped holding it.
//
// It is `failed` with a retryable runner_unavailable, and the note says
// exactly what is and is not known. In particular it does NOT claim the work
// did not happen: a container started by a process that then died may well
// have kept running and had its side effects. What the service can say
// honestly is that it stopped observing at a given instant and never learned
// the outcome, so the timing's finished_at is the moment the loss was
// recorded rather than a moment anything was measured.
func interruptedResult(record Record, now time.Time) (runners.State, runners.Result) {
	started := record.AcceptedAt
	if record.StartedAt != nil {
		started = *record.StartedAt
	}
	return runners.StateFailed, unmeasuredResult(record, runners.StateFailed, runners.ErrorRunnerUnavailable,
		"the runner service restarted while this operation was in flight; its outcome was never observed",
		"the runner service stopped holding this operation when its process ended, and finished_at is the "+
			"instant that loss was recorded, not an instant anything was measured. Side effects of a partially "+
			"executed operation may still have occurred",
		started, now)
}

// stateForErrorKind maps a dispatch failure onto the terminal state that
// describes it, using the result schema's own vocabulary. Refusals are
// `rejected` — the schema's state for an operation that did not run —
// while a timeout and a cancellation keep their own names.
func stateForErrorKind(kind runners.ErrorKind) runners.State {
	switch kind {
	case runners.ErrorTimeout:
		return runners.StateTimedOut
	case runners.ErrorCancellation:
		return runners.StateCancelled
	case runners.ErrorRejectedInput, runners.ErrorAuthOrPolicy, runners.ErrorContractFailure:
		return runners.StateRejected
	default:
		return runners.StateFailed
	}
}

// replayOf reads the pins a result must report off the operation document.
// They are statements about what was asked for, not about what happened, so a
// result that measured nothing may still carry them.
func replayOf(op runners.Operation) (Replay, error) {
	policyDigest, err := contracts.DigestValue(op.Policy)
	if err != nil {
		return Replay{}, err
	}
	replay := Replay{
		RunnerRevision: op.RunnerRevision,
		ImageDigest:    op.Execution.ImageDigest,
		PolicyDigest:   policyDigest,
	}
	if op.Workspace != nil {
		digest := op.Workspace.SourceDigest
		replay.InputDigest = &digest
	}
	return replay, nil
}

func durationMs(started, finished time.Time) int {
	elapsed := finished.Sub(started).Milliseconds()
	if elapsed < 0 {
		return 0
	}
	return int(elapsed)
}
