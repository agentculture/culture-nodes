package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Notices (task t45, issue #332 follow-up): a human task that ASKS nothing.
//
// Two kinds of human task are raised by the control plane about a run rather
// than by an approval node inside it:
//
//   - trigger_remint_exhausted (internal/store/postgres/remint.go): a
//     trigger-created run failed technically and its bounded re-mints are
//     spent — the control plane will not try again on its own;
//   - schedule_failing (internal/store/postgres/schedulebackoff.go): a
//     schedule has failed N times running with the same reason.
//
// Neither parks a node run and neither resumes one when it is answered: the
// run they name is the EVIDENCE — already failed and terminal — not the
// subject. Before t45 both were written with no allowed_outcomes, and
// DecideHumanTask accepts only an outcome in that set, so a person could see
// them and never clear them (the owner's inbox held ten).
//
// The one answer a notice takes is `acknowledged`: "I have read this." It is
// recorded exactly like any other human decision — a proposed `decision`
// record confirmed through the stale-guarded human review transaction, so
// ledger.CommitReview's reviewer_must_be_human rule applies unchanged — and
// it moves nothing: no node run transitions, no token is consumed, no run is
// resumed, no re-mint is scheduled.
//
// Legacy rows are handled in code, not by a data migration: a notice kind
// whose request declares no outcomes is read as declaring
// [acknowledged] (HumanTaskAllowedOutcomes). The stored request stays the
// verbatim record of what was written at the time, which is the property
// allowed_outcomes has everywhere else in this package, and every surface —
// engine check, API views, fan-out text — reads the same one function, so
// there is no second place for the rule to drift.

const (
	// OutcomeAcknowledged is the one outcome a notice accepts.
	OutcomeAcknowledged = "acknowledged"

	// HumanTaskKindTriggerRemintExhausted is raised when a trigger-created
	// run's bounded re-mints are spent.
	HumanTaskKindTriggerRemintExhausted = "trigger_remint_exhausted"
	// HumanTaskKindScheduleFailing is raised when a schedule keeps failing.
	HumanTaskKindScheduleFailing = "schedule_failing"
)

// IsNoticeKind reports whether kind is a human task that informs rather than
// asks — decided only by acknowledging it, with no effect on any run.
func IsNoticeKind(kind string) bool {
	return kind == HumanTaskKindTriggerRemintExhausted || kind == HumanTaskKindScheduleFailing
}

// NoticeAllowedOutcomes is the outcome set a notice is created with. A fresh
// slice on every call, so no caller can mutate another's.
func NoticeAllowedOutcomes() []string { return []string{OutcomeAcknowledged} }

// effectiveAllowedOutcomes is the one rule: the declared set, except that a
// notice declaring none (a row written before t45) declares [acknowledged].
func effectiveAllowedOutcomes(kind string, declared []string) []string {
	if len(declared) == 0 && IsNoticeKind(kind) {
		return NoticeAllowedOutcomes()
	}
	return declared
}

// HumanTaskAllowedOutcomes is the task's allowed outcome set as every
// surface must present it and as DecideHumanTask judges it: the request's
// allowed_outcomes, with a legacy notice read as [acknowledged]. A request
// that will not parse declares nothing (a notice still gets its fallback).
func HumanTaskAllowedOutcomes(task HumanTask) []string {
	var request humanTaskRequest
	if len(task.Request) > 0 {
		_ = json.Unmarshal(task.Request, &request)
	}
	return effectiveAllowedOutcomes(task.Kind, request.AllowedOutcomes)
}

// decideNotice acknowledges a notice. It shares guard-like locking, the
// outcome check, recordDecision and markDecided with an approval decision,
// and deliberately none of transition: the run behind a notice is terminal,
// and a notice's node run (remint.go names the failed one) is not waiting on
// anyone, so there is nothing to route and guard's waiting_human check would
// refuse it. Returns handled=false for every non-notice kind.
func (d *humanTaskDecision) decideNotice(ctx context.Context) (bool, error) {
	task, err := d.tx.GetHumanTask(ctx, d.req.HumanTaskID)
	if err != nil {
		return false, err
	}
	if !IsNoticeKind(task.Kind) {
		return false, nil
	}
	if d.expiry != nil {
		// Nothing the world does answers "I have read this".
		return true, &OutcomeNotAllowedError{HumanTaskID: task.ID, Outcome: d.req.Outcome}
	}
	if task.RunID == "" {
		return true, errors.New("engine: notice " + task.ID + " names no run to record its acknowledgement against")
	}
	// The run's ledger lock, as guard takes it: recordDecision's
	// check-then-append on the run's ledger version is race-free only under it.
	if err := d.tx.Lock(ctx, ledger.RunLockKey(task.RunID)); err != nil {
		return true, err
	}
	if task, err = d.tx.GetHumanTask(ctx, task.ID); err != nil {
		return true, err
	}
	if task.Status != HumanTaskStatusPending {
		return true, &HumanTaskAlreadyDecidedError{HumanTaskID: task.ID, Status: task.Status}
	}
	run, err := d.tx.Run(ctx, task.RunID)
	if err != nil {
		return true, err
	}
	d.task = task
	d.run = run
	// Only the id is read (the decision record's node_run_id); the node run
	// itself is neither loaded nor touched.
	d.nodeRun = NodeRun{ID: task.NodeRunID, RunID: run.ID}
	if err := d.checkOutcome(); err != nil {
		return true, err
	}
	d.outcome = d.req.Outcome
	if err := d.recordDecision(ctx); err != nil {
		return true, err
	}
	if err := d.markDecided(ctx); err != nil {
		return true, err
	}
	if err := d.emit(ctx, TypeHumanTaskDecided, map[string]any{
		"run_id":           run.ID,
		"node_run_id":      task.NodeRunID,
		"human_task_id":    task.ID,
		"kind":             task.Kind,
		"decider_actor_id": d.req.DeciderActorID,
		"outcome":          d.outcome,
	}); err != nil {
		return true, err
	}
	d.result.RunID = run.ID
	d.result.NodeRunID = task.NodeRunID
	d.result.Outcome = d.outcome
	return true, d.finish(ctx)
}
