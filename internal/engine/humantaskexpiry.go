package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Expiry: resolving a human task the world already answered (task t11,
// spec c6).
//
// # Why this is not a decision
//
// DecideHumanTask is a human exercising authority: it appends a `proposed`
// decision record and confirms it through a review transaction, because PRD
// §10.4 says confirmed authority belongs to people. An expiry has no person
// in it. Nobody chose `expired`; the engine READ a fact — the sweep's
// pr.merged — and computed that the question this task asks can no longer
// have a useful answer, because the pull request it is asking whether to
// merge is already merged.
//
// That is exactly PRD §10.4's `derived`: "deterministic validators create
// derived". So the expiry appends ONE derived decision record naming the
// reason and the fact behind it, and no review. It cannot promote itself,
// and it never claims a human agreed.
//
// # Why it routes the graph anyway
//
// `expired` is one of the three outcomes the compiler implies for every
// approval node (internal/compiler/vocabulary.go), so it is a declared domain
// outcome with a declared edge — not an engine failure. An expiring task
// therefore takes the same transition/advance path a decision does, through
// the same planTransition and dispatchNode calls (see humandecision.go's
// do()): the run continues down whatever edge the author drew from
// `<node>.expired`. A workflow that draws none gets the ordinary "no edge
// matched" run failure, which is the honest answer — it means the author
// never said what expiry should do.
//
// # Why the status is its own
//
// human_tasks.status becomes `expired`, not `decided`. Counting the two
// together would make "how many decisions did people actually make"
// unanswerable, and the 26 stale prod approvals this task exists to clear
// are precisely the population that must stay distinguishable from answered
// ones afterwards.

// OutcomeExpired is the domain outcome an expiry routes under — the same
// name the compiler implies for every approval node.
const OutcomeExpired = "expired"

// HumanTaskExpiryReasonPRMerged is the one reason this engine expires a task
// on its own today: the pull request the task is an approval for has already
// been merged, which the control plane learns from the sweep's `pr.merged`
// fact and from nowhere else.
const HumanTaskExpiryReasonPRMerged = "pr_merged"

// HumanTaskExpiryActorID is the producer identity an expiry's derived record
// is written under when a caller names none.
//
// It names the RULE, not the process that happened to apply it — the same
// argument api.DefaultRepairRouterActorID and postgres.RemintSchedulerActorID
// make: a derived record's producer is the deterministic computation that
// produced it (PRD §10.4), and two scheduler processes expiring the same task
// from the same facts are the same producer in every sense a reader cares
// about.
//
// Like those two it is a REGISTRATION OBLIGATION, not a label: the ledger
// envelope requires a non-empty origin.actor_id and ledger_records
// .origin_actor_id is a real foreign key to actors(id), so a deployment that
// has not registered this identity expires nothing — loudly, on the first
// attempt, rather than silently recording no provenance. Register it the way
// engine_remint_scheduler was registered:
//
//	deploy/prod/register-actor.sh --engine human_task_expiry
const HumanTaskExpiryActorID = "human_task_expiry"

// humanTaskExpiryReason is the expiry half of a humanTaskDecision (see that
// struct's `expiry` field): what to call this resolution and what evidence
// stands behind it.
type humanTaskExpiryReason struct {
	// Reason is the machine-readable cause, written to
	// human_tasks.expiry_reason so a count is a query rather than a JSON
	// unpack.
	Reason string
	// Detail is the human-readable statement of the same fact — e.g.
	// "agentculture/culture-nodes#236 is merged". It is prose for a reader,
	// never something a consumer should parse.
	Detail string
	// ProducerActorID is the registered identity the derived record names.
	ProducerActorID string
}

// producerActorID applies the default in exactly one place, so the CLI, the
// scheduler lane and a direct caller cannot disagree about it.
func producerActorID(configured string) string {
	if configured == "" {
		return HumanTaskExpiryActorID
	}
	return configured
}

// ExpireHumanTaskRequest is one bounded expiry.
type ExpireHumanTaskRequest struct {
	// HumanTaskID names the pending task to expire.
	HumanTaskID string
	// Reason is required: an expiry with no stated cause is indistinguishable
	// from a task that was quietly dropped, which is the failure mode this
	// whole task exists to end.
	Reason string
	// Detail is the optional prose statement of the fact behind Reason.
	Detail string
	// ProducerActorID is the registered identity the derived record is
	// written under. Empty selects HumanTaskExpiryActorID.
	ProducerActorID string
}

func (r ExpireHumanTaskRequest) validate() error {
	switch {
	case r.HumanTaskID == "":
		return errors.New("engine: ExpireHumanTask requires a human task id")
	case r.Reason == "":
		return errors.New("engine: ExpireHumanTask requires a reason")
	}
	return nil
}

// ExpireHumanTask resolves one pending human task as `expired`, recording why,
// and routes the run down the node's `expired` edge — the whole thing in one
// transaction, exactly like DecideHumanTask, so a refused expiry (an
// already-decided task, a terminal run, a node whose contract does not allow
// `expired`) leaves the task exactly as it was.
func (e *Engine) ExpireHumanTask(ctx context.Context, req ExpireHumanTaskRequest) (CompletionResult, error) {
	if err := req.validate(); err != nil {
		return CompletionResult{}, err
	}

	var result CompletionResult
	err := e.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		d := &humanTaskDecision{
			engine: e,
			tx:     tx,
			now:    e.now().UTC(),
			req:    HumanTaskDecisionRequest{HumanTaskID: req.HumanTaskID, Outcome: OutcomeExpired},
			expiry: &humanTaskExpiryReason{
				Reason: req.Reason, Detail: req.Detail, ProducerActorID: producerActorID(req.ProducerActorID),
			},
		}
		if err := d.do(ctx); err != nil {
			return err
		}
		result = d.result
		return nil
	})
	if err != nil {
		return CompletionResult{}, err
	}
	return result, nil
}

// humanTaskExpiryData is the derived ledger record's payload: what expired,
// under what outcome, and why.
type humanTaskExpiryData struct {
	HumanTaskID string `json:"human_task_id"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail,omitempty"`
}

// recordExpiry appends the one derived record an expiry writes.
func (d *humanTaskDecision) recordExpiry(ctx context.Context) error {
	data, err := json.Marshal(humanTaskExpiryData{
		HumanTaskID: d.task.ID,
		Outcome:     d.outcome,
		Reason:      d.expiry.Reason,
		Detail:      d.expiry.Detail,
	})
	if err != nil {
		return fmt.Errorf("engine: encode human task %s expiry: %w", d.task.ID, err)
	}
	record, err := d.tx.Ledger().Append(ctx, ledger.Record{
		RecordType: ledger.RecordDecision,
		RunID:      d.run.ID,
		NodeRunID:  ledger.NullableID(d.nodeRun.ID),
		Origin:     ledger.Origin{Kind: ledger.OriginEngine, ActorID: d.expiry.ProducerActorID},
		Authority:  ledger.AuthorityDerived,
		Data:       data,
	})
	if err != nil {
		return err
	}
	d.result.LedgerRecords = append(d.result.LedgerRecords, record)
	return nil
}

// humanTaskExpiryResponse is the JSON shape written to human_tasks.response.
// It mirrors humanTaskResponse's field order and omits decider_actor_id
// entirely rather than filling it with a placeholder: there was no decider.
type humanTaskExpiryResponse struct {
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason"`
	Detail    string    `json:"detail,omitempty"`
	ExpiredAt time.Time `json:"expired_at"`
}

// markExpired flips the task pending -> expired. A false return means a
// racing decision already won, and the expiry is abandoned with nothing
// written — a person's answer beats the engine's inference, always.
func (d *humanTaskDecision) markExpired(ctx context.Context) error {
	response, err := json.Marshal(humanTaskExpiryResponse{
		Outcome:   d.outcome,
		Reason:    d.expiry.Reason,
		Detail:    d.expiry.Detail,
		ExpiredAt: d.now,
	})
	if err != nil {
		return fmt.Errorf("engine: encode human task %s expiry response: %w", d.task.ID, err)
	}
	expired, err := d.tx.MarkHumanTaskExpired(ctx, d.task.ID, d.expiry.Reason, response, d.now)
	if err != nil {
		return err
	}
	if !expired {
		return &HumanTaskAlreadyDecidedError{HumanTaskID: d.task.ID, Status: HumanTaskStatusDecided}
	}
	return nil
}

// PendingHumanTasksWithMergedPR lists the pending tasks whose subject pull
// request the control plane has already been told is merged. It is exported
// so the backfill command can show an operator what it WOULD expire before it
// expires anything.
func (e *Engine) PendingHumanTasksWithMergedPR(ctx context.Context, limit int) ([]string, error) {
	var ids []string
	err := e.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		found, err := tx.PendingHumanTasksWithMergedPR(ctx, limit)
		if err != nil {
			return err
		}
		ids = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ExpiredForMergedPR is one task the consumer resolved, or tried to.
type ExpiredForMergedPR struct {
	HumanTaskID string
	RunID       string
	Outcome     string
	// Err is why this one task was left alone. A task can legitimately
	// refuse: its node may not declare an `expired` outcome, its run may
	// already be terminal, or a human may have decided it between the scan
	// and the write. None of those is a reason to abandon the rest.
	Err error
}

// ExpirePendingTasksForMergedPR is the pr.merged consumer: find every pending
// task whose subject PR has merged, and expire each one in its own
// transaction.
//
// One transaction per task, not one for the batch, because each expiry takes
// its own run's advisory lock and routes its own graph. A batch transaction
// would hold every affected run's lock at once and make one refusing task
// (an already-decided one, say) roll back the others' work.
//
// The scan is re-run from scratch on every call rather than tracked against a
// cursor: expiring is idempotent because a task is pending exactly once, so a
// missed tick costs a delay and never a double expiry.
func (e *Engine) ExpirePendingTasksForMergedPR(ctx context.Context, limit int, producer string) ([]ExpiredForMergedPR, error) {
	ids, err := e.PendingHumanTasksWithMergedPR(ctx, limit)
	if err != nil {
		return nil, err
	}
	results := make([]ExpiredForMergedPR, 0, len(ids))
	for _, id := range ids {
		result, err := e.ExpireHumanTask(ctx, ExpireHumanTaskRequest{
			HumanTaskID:     id,
			Reason:          HumanTaskExpiryReasonPRMerged,
			Detail:          "the pull request this approval is for is already merged (pr.merged fact)",
			ProducerActorID: producer,
		})
		results = append(results, ExpiredForMergedPR{
			HumanTaskID: id, RunID: result.RunID, Outcome: result.Outcome, Err: err,
		})
	}
	return results, nil
}
