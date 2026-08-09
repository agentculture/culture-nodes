package engine

import (
	"errors"
	"fmt"
)

// ErrStaleClaim is returned by CompleteAttempt when the fencing tuple in the
// request no longer matches the work item's current lease: a newer claim took
// the item, the lease was reclaimed, or the item is already completed. It is
// the engine-level surface of PRD §12.4's "late workers cannot commit over a
// newer attempt", and it means *nothing* was written — the whole transaction
// rolled back, so a stale completion cannot leave a partial trace.
//
// Store implementations translate their own stale-claim sentinel into this
// one (wrapping both), so a caller can match on either.
var ErrStaleClaim = errors.New("engine: stale claim: the work item is not leased under this owner, fencing token, and attempt")

// ErrNotFound reports that a referenced row does not exist.
var ErrNotFound = errors.New("engine: not found")

// TerminalNodeRunError is returned when a completion arrives for a node run
// that has already reached a terminal state.
//
// The fencing guard catches almost every such completion first, because
// completing a node run also completes its work item. This error covers what
// fencing cannot: a *fresh* claim against a node run that is already
// finished. Either way nothing is written.
type TerminalNodeRunError struct {
	NodeRunID string
	NodeID    string
	State     NodeRunState
}

func (e *TerminalNodeRunError) Error() string {
	return fmt.Sprintf("engine: node run %s (node %q) is %s and never transitions again",
		e.NodeRunID, e.NodeID, e.State)
}

// Is lets callers match with errors.Is(err, ErrTerminalNodeRun).
func (e *TerminalNodeRunError) Is(target error) bool { return target == ErrTerminalNodeRun }

// ErrTerminalNodeRun is the sentinel every TerminalNodeRunError matches.
var ErrTerminalNodeRun = errors.New("engine: node run is terminal")

// TerminalRunError is returned when a completion arrives for a run that has
// already ended. Like TerminalNodeRunError, it writes nothing.
type TerminalRunError struct {
	RunID string
	State RunState
}

func (e *TerminalRunError) Error() string {
	return fmt.Sprintf("engine: run %s is %s and accepts no further transitions", e.RunID, e.State)
}

// Is lets callers match with errors.Is(err, ErrTerminalRun).
func (e *TerminalRunError) Is(target error) bool { return target == ErrTerminalRun }

// ErrTerminalRun is the sentinel every TerminalRunError matches.
var ErrTerminalRun = errors.New("engine: run is terminal")

// ContractError reports a payload that does not satisfy a declared contract.
// CreateRun returns it for run input; inside CompleteAttempt the same check
// becomes a Rejection instead, because by then there is a node run to record
// the refusal against.
type ContractError struct {
	// What names the contract: "run input", "run output", or
	// "node <id> outcome <name>".
	What   string
	Detail string
}

func (e *ContractError) Error() string {
	return fmt.Sprintf("engine: %s does not satisfy its contract: %s", e.What, e.Detail)
}

// Is lets callers match with errors.Is(err, ErrContract).
func (e *ContractError) Is(target error) bool { return target == ErrContract }

// ErrContract is the sentinel every ContractError matches.
var ErrContract = errors.New("engine: contract violation")

// WorkflowError reports a workflow definition the engine cannot execute: an
// IR it cannot decode, a guard it cannot compile, a schema it cannot build.
// It is always a fault in the definition or in the store, never in the
// completion being processed.
type WorkflowError struct {
	Digest string
	Detail string
}

func (e *WorkflowError) Error() string {
	return fmt.Sprintf("engine: workflow %s cannot be executed: %s", e.Digest, e.Detail)
}

// HumanTaskAlreadyDecidedError is returned by DecideHumanTask when the named
// task's status is no longer pending — either an earlier decision already
// resumed the run, or a racing decision won the atomic status flip
// (MarkHumanTaskDecided) first. Either way nothing is written: a human task
// decides exactly once, and a second decision on it is refused rather than
// resuming the run a second time.
type HumanTaskAlreadyDecidedError struct {
	HumanTaskID string
	// Status is the task's current status ("decided" — pending is the only
	// status this error is never raised for).
	Status string
}

func (e *HumanTaskAlreadyDecidedError) Error() string {
	return fmt.Sprintf("engine: human task %s is already %s and accepts no further decision", e.HumanTaskID, e.Status)
}

// Is lets callers match with errors.Is(err, ErrHumanTaskAlreadyDecided).
func (e *HumanTaskAlreadyDecidedError) Is(target error) bool {
	return target == ErrHumanTaskAlreadyDecided
}

// ErrHumanTaskAlreadyDecided is the sentinel every HumanTaskAlreadyDecidedError matches.
var ErrHumanTaskAlreadyDecided = errors.New("engine: human task is already decided")

// OutcomeNotAllowedError is returned by DecideHumanTask when the decision's
// outcome is not one of the task's allowed_outcomes (PRD §9.9) — the set the
// human was actually shown, taken from human_tasks.request rather than
// re-derived from the live node, so a decision is judged against what was
// presented, not against a possibly-different read of the definition.
type OutcomeNotAllowedError struct {
	HumanTaskID string
	Outcome     string
	Allowed     []string
}

func (e *OutcomeNotAllowedError) Error() string {
	return fmt.Sprintf("engine: human task %s: outcome %q is not one of the task's allowed outcomes %v",
		e.HumanTaskID, e.Outcome, e.Allowed)
}

// Is lets callers match with errors.Is(err, ErrOutcomeNotAllowed).
func (e *OutcomeNotAllowedError) Is(target error) bool { return target == ErrOutcomeNotAllowed }

// ErrOutcomeNotAllowed is the sentinel every OutcomeNotAllowedError matches.
var ErrOutcomeNotAllowed = errors.New("engine: outcome not allowed")

// HumanTaskNotWaitingError is returned by DecideHumanTask on the defensive
// path where a task is still pending but the node run it names is not
// NodeRunWaitingHuman. It should not be reachable through normal use — a
// pending task's node run is put in that state by the same dispatch that
// wrote the task and nothing else moves it except this decision — so seeing
// it means the two rows have drifted apart.
type HumanTaskNotWaitingError struct {
	HumanTaskID string
	NodeRunID   string
	State       NodeRunState
}

func (e *HumanTaskNotWaitingError) Error() string {
	return fmt.Sprintf("engine: human task %s names node run %s, which is %s rather than %s",
		e.HumanTaskID, e.NodeRunID, e.State, NodeRunWaitingHuman)
}
