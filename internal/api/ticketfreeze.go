package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/events"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TicketFrozenReason is the run-level reason (migrations/0052) every run a
// ticket freeze ends carries, whether it was cancelled or parked. One
// spelling for both outcomes on purpose: the QUESTION a reader asks is "why
// did this run stop", and the answer is the same fact in both cases — the
// ticket it belongs to was frozen. Which of the two happened is already
// visible in the run's own state (`cancelled` vs `waiting`), and encoding it
// twice would let the two disagree.
const TicketFrozenReason = "ticket_frozen"

// doneTicketStatus is the board status that makes a freeze terminal.
// Compared case-insensitively against whatever the caller said the ticket's
// status was, because "Done" is a Jira display name typed by humans and by
// two different scripts, not an identifier this control plane mints.
const doneTicketStatus = "done"

// ticketFreezeEffect is what a freeze did to a ticket's runs: the ids it
// cancelled and the ids it parked, in the order it walked them.
type ticketFreezeEffect struct {
	Cancelled []string
	Parked    []string
}

// freezeEndsRunsAsCancelled reports whether a ticket in this board status
// ends its runs terminally (cancel) or reversibly (park).
//
// Only "Done" cancels. Everything else parks — including, deliberately, an
// UNKNOWN status: the Jira bridge has post_comment / transition_issue /
// create_issue and no read verb (spec s13/s18), so a caller that does not
// say what the board status is leaves this control plane genuinely unable
// to find out, and the honest response to "I do not know whether this
// ticket is finished" is the reversible one. A park keeps every durable row
// a resume would need; a cancel consumes the tokens and cannot be undone.
func freezeEndsRunsAsCancelled(ticketStatus string) bool {
	return strings.EqualFold(strings.TrimSpace(ticketStatus), doneTicketStatus)
}

// subjectRunsSQL finds the ticket's still-live runs, by both addresses a run
// can carry its ticket at.
//
// `subject` (migrations/0038, indexed by 0047's runs_subject_idx) is the
// modern one: a run minted from a Jira fact records the issue key its
// triggering event named. Runs created before that column existed carry it
// only inside their own input — the jira work-item contract
// examples/jira-intake/workflow.yaml consumes is `{source: "jira", id,
// project, ...}`, so `input->>'id'` is the ticket key for exactly those
// runs. The SCRUM-5 spec-chain run this task exists for
// (01M16GMQMWYCA0EW0V7MHHQFWN) is one of them.
//
// The input fallback applies only when `subject IS NULL`: a run that
// declares a subject is authoritative about which ticket it belongs to, and
// letting a stale input field contradict it would let one ticket's freeze
// reach into another ticket's run.
//
// Terminal runs are excluded — a completed, failed or cancelled run is not
// something a freeze can still end, and re-stamping a reason on one would
// rewrite history the freeze did not make.
const subjectRunsSQL = `
SELECT id, status
FROM runs
WHERE namespace_id = $1
  AND status <> ALL ($3::text[])
  AND reason IS DISTINCT FROM $4
  AND (subject = $2 OR (subject IS NULL AND input->>'id' = $2))
ORDER BY created_at, id
`

// freezeTicketRuns ends every live run of a frozen ticket and records why on
// each one (spec c28/h19, task t17).
//
// Cancel or park is decided once, from the ticket's board status, and
// applied to every run — a ticket is finished or it is not, and half a
// ticket's runs cancelled with the other half parked would be a state no
// reader could explain.
//
// It is best-effort at the level of the whole ticket but not silent: an
// error from any single run aborts and is returned, so the caller can
// decide whether the freeze itself should fail. The freeze row is written
// first and separately, because the ticket being frozen is true regardless
// of how far this walk got — a ticket that took a merge and then failed to
// end one of its runs must still refuse new replies.
func (s *Server) freezeTicketRuns(ctx context.Context, ticketID, ticketStatus string) (ticketFreezeEffect, error) {
	var effect ticketFreezeEffect
	rows, err := s.Store.Pool().Query(ctx, subjectRunsSQL, s.NamespaceID, ticketID, postgres.TerminalRunStatuses(), TicketFrozenReason)
	if err != nil {
		return effect, fmt.Errorf("api: freeze ticket %s: list subject runs: %w", ticketID, err)
	}
	type liveRun struct{ id, status string }
	live := make([]liveRun, 0)
	for rows.Next() {
		var r liveRun
		if err := rows.Scan(&r.id, &r.status); err != nil {
			rows.Close()
			return effect, fmt.Errorf("api: freeze ticket %s: scan subject run: %w", ticketID, err)
		}
		live = append(live, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return effect, fmt.Errorf("api: freeze ticket %s: list subject runs: %w", ticketID, err)
	}

	cancel := freezeEndsRunsAsCancelled(ticketStatus)
	for _, run := range live {
		if cancel {
			detail := fmt.Sprintf("ticket %s frozen with status %q; run cancelled", ticketID, ticketStatus)
			if _, err := s.cancelRunWithReason(ctx, run.id, TicketFrozenReason, detail); err != nil {
				return effect, fmt.Errorf("api: freeze ticket %s: cancel run %s: %w", ticketID, run.id, err)
			}
			effect.Cancelled = append(effect.Cancelled, run.id)
			continue
		}
		parked, err := s.parkRunForFrozenTicket(ctx, run.id, ticketID, ticketStatus)
		if err != nil {
			return effect, err
		}
		if parked {
			effect.Parked = append(effect.Parked, run.id)
		}
	}
	return effect, nil
}

// parkRunForFrozenTicket suspends one run without ending it, and returns
// whether it actually parked anything (false means the run reached a
// terminal state between the listing above and this transaction).
//
// THERE IS NO ENGINE PARK PRIMITIVE FOR A WHOLE RUN, and this task does not
// invent one. internal/engine parks a WORK ITEM, never a run: the durable
// wait (Store.StartDurableWait), the async actor wait (StartAsyncWait) and
// the signal wait (StartDurableSignalWait) all move one leased item to
// work_items.state = 'waiting' — "no owner, no expiry, no reclaim",
// invisible to ClaimWork and to ReclaimExpired (internal/store/postgres/
// async.go's own summary). This reuses exactly that mechanism, at run
// scope: every claimable item of the run is moved to the same 'waiting'
// state the signal wait leaves its item in, and the run row is moved to
// engine.RunWaiting, which is already a non-terminal member of
// postgres.ActiveRunStatuses.
//
// The one thing it does NOT reuse is a waker. A signal park arms a
// subscription; a wait park arms a timer; this park arms neither, because
// the event that should resume it is a human reopening the ticket, and no
// such fact exists yet. So "resumable" here means precisely this and no
// more: every durable row a resume would need is left intact — active
// tokens stay active, node runs keep their status, pending timers and
// pending signal subscriptions stay pending, and no attempt is invalidated
// — where a cancel consumes and retires all four. Nothing in this repo can
// yet perform that resume; a reopen surface is not part of this task and is
// stated as absent rather than implied.
//
// One residual, stated rather than papered over: a run parked here that was
// ALREADY waiting on a signal keeps its pending subscription, so an external
// delivery of that signal can still fire it — returning its work item to
// 'ready' and making it claimable again while the run row still reads
// 'waiting'. That is not fixed by retiring the subscription, because the
// subscription is precisely what a resume would need; it is the cost of a
// park that has no reopen surface to pair with. It is out of reach from the
// ticket page itself (a frozen ticket refuses replies, so no page reply can
// mint the fact) and needs an out-of-band POST /v1alpha1/events to happen at
// all.
//
// 'leased' items are parked alongside 'ready' ones, the same way cancelRun
// cancels them: a worker holding one completes through Store.CompleteWork,
// whose fenced UPDATE requires `state = 'leased'` and therefore matches
// zero rows once this commits — the documented engine.ErrStaleClaim no-op,
// not a new failure mode. Items already 'waiting' are left exactly as they
// are; they are parked already, and they own the timer or subscription that
// is their waker.
func (s *Server) parkRunForFrozenTicket(ctx context.Context, runID, ticketID, ticketStatus string) (bool, error) {
	tx, err := s.Store.Pool().Begin(ctx)
	if err != nil {
		return false, internalError(fmt.Errorf("park run %s: begin: %w", runID, err))
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	// The same advisory lock the engine's §12.5 completion transaction and
	// cancelRun both take, so a park cannot interleave with a concurrent
	// attempt completion of the same run.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, ledger.RunLockKey(runID)); err != nil {
		return false, internalError(fmt.Errorf("park run %s: lock: %w", runID, err))
	}

	var status string
	var reason *string
	if err := tx.QueryRow(ctx, `SELECT status, reason FROM runs WHERE id = $1 AND namespace_id = $2`, runID, s.NamespaceID).Scan(&status, &reason); err != nil {
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, internalError(fmt.Errorf("park run %s: %w", runID, err))
	}
	if engine.RunState(status).Terminal() {
		return false, nil
	}
	if status == string(engine.RunWaiting) && reason != nil && *reason == TicketFrozenReason {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE runs SET status = $2, reason = $3, updated_at = now() WHERE id = $1`,
		runID, string(engine.RunWaiting), TicketFrozenReason,
	); err != nil {
		return false, internalError(fmt.Errorf("park run %s: update run: %w", runID, err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE work_items
		SET state = 'waiting', lease_owner = NULL, lease_expires_at = NULL,
		    state_version = state_version + 1, updated_at = now()
		WHERE state IN ('ready', 'leased') AND node_run_id IN (SELECT id FROM node_runs WHERE run_id = $1)`,
		runID,
	); err != nil {
		return false, internalError(fmt.Errorf("park run %s: park work items: %w", runID, err))
	}

	// The same audit-event-plus-outbox-row pair cancelRun writes (PRD §12.5
	// steps 7 and 10): a park decided outside the engine still leaves the
	// durable trail a worker-driven transition would.
	var sequence int64
	if err := tx.QueryRow(ctx,
		`SELECT (COALESCE(MAX(sequence), 0) + 1)::bigint FROM events WHERE aggregate_id = $1`, runID,
	).Scan(&sequence); err != nil {
		return false, internalError(fmt.Errorf("park run %s: next event sequence: %w", runID, err))
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id": runID,
		"state":  string(engine.RunWaiting),
		"detail": fmt.Sprintf("ticket %s frozen with status %q; run parked", ticketID, ticketStatus),
		"reason": TicketFrozenReason,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at)
		VALUES ($1, $2, 'run', $3, $4, $5, 'nodes', $6, now())`,
		store.NewULID(), s.NamespaceID, runID, sequence, events.TypeRunWaiting, payload,
	); err != nil {
		return false, internalError(fmt.Errorf("park run %s: append event: %w", runID, err))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
		VALUES ($1, $2, $3, $4, 'pending', now())`,
		store.NewULID(), s.NamespaceID, events.TypeRunWaiting, payload,
	); err != nil {
		return false, internalError(fmt.Errorf("park run %s: append outbox: %w", runID, err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, internalError(fmt.Errorf("park run %s: commit: %w", runID, err))
	}
	return true, nil
}

// ticketFreezeOut derives the frozen ticket's freeze summary from the runs
// the projection already returns, and composes the banner sentence the page
// renders.
//
// It counts by state rather than by "what the freeze did", because the run's
// state is the fact and the freeze's intent is not: a run cancelled by the
// freeze that a later operator action somehow revived would be counted where
// it actually is, and a run parked by the freeze that has since been
// cancelled reads as cancelled. Runs with no freeze reason are not counted at
// all — a ticket can carry runs that ended long before the merge.
func ticketFreezeOut(ticketStatus string, runs []RunOut) *TicketFreezeOut {
	out := &TicketFreezeOut{Reason: TicketFrozenReason, TicketStatus: ticketStatus}
	for _, run := range runs {
		if run.Reason != TicketFrozenReason {
			continue
		}
		switch engine.RunState(run.State) {
		case engine.RunCancelled:
			out.Cancelled++
		case engine.RunWaiting:
			out.Parked++
		}
	}
	out.Banner = freezeBannerText(out)
	return out
}

// freezeBannerText is the one sentence a human reads on the frozen ticket
// page. It always names both counts, including zero: "0 runs cancelled" is
// the answer to "did the freeze leave anything running", and hiding it
// behind an absent clause would make the silent-no-op case this task fixes
// look exactly like the working case.
func freezeBannerText(f *TicketFreezeOut) string {
	status := f.TicketStatus
	if status == "" {
		status = "unknown"
	}
	return fmt.Sprintf(
		"Ticket status %s: %d %s cancelled and %d parked with reason %s.",
		status, f.Cancelled, runNoun(f.Cancelled), f.Parked, f.Reason,
	)
}

func runNoun(n int) string {
	if n == 1 {
		return "run"
	}
	return "runs"
}

// freezeRunsForMergedPRFact applies the freeze's run half to the ticket a
// pr.merged fact named, and does nothing at all for any other fact.
//
// The ticket_freezes row itself is already written by the delivery that
// produced this fact; this only ends the ticket's runs. A payload with no
// issue_key cannot reach here — Store.DeliverSignalEvent refuses a pr.merged
// fact without one before the fact is appended — but it is treated as "no
// ticket to freeze" rather than an error, so this function stays a no-op for
// anything it does not recognise instead of failing a delivery it does not
// own.
func (s *Server) freezeRunsForMergedPRFact(ctx context.Context, name string, payload json.RawMessage) error {
	if name != mergedPRFactName {
		return nil
	}
	var merged struct {
		IssueKey string `json:"issue_key"`
	}
	if err := json.Unmarshal(payload, &merged); err != nil || merged.IssueKey == "" {
		return nil
	}
	// No status: the merged-PR fact carries none, and an unknown status parks.
	if _, err := s.freezeTicketRuns(ctx, merged.IssueKey, ""); err != nil {
		return err
	}
	return nil
}

// mergedPRFactName is the signal name Store.DeliverSignalEvent treats as a
// ticket freeze. Duplicated as a constant here rather than exported from the
// store because the two are the same wire contract read by two layers, and
// signalfreeze_test.go pins them equal.
const mergedPRFactName = "pr.merged"
