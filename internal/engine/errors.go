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
