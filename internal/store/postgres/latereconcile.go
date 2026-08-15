package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Late-callback reconciliation (task t11, ADR 0012,
// migrations/0028_attempt_supersedes.sql).
//
// §13.4 refuses a terminal callback that arrives after its attempt was
// replaced or cancelled the right to commit workflow state, and that refusal
// is not what this file changes. What it changes is the silence that used to
// go with it: a deadline expires, the scheduler records a `timed_out`
// attempt, and the session it bounded keeps running until the cancel reaches
// its bridge — which then measures its workspace, preserves what it left
// behind, and reports the tokens it burned, the model that burned them, and
// why the turn ended. All of that describes work that really happened, and
// before this it landed nowhere a reader of `attempts` could see.
//
// It lives in its own file rather than in engine_store.go for the reason the
// spec measured up front (scope entry s46): engine_store.go sits ~80 lines
// under this repo's 1000-line file gate, and this task is one of three
// aimed straight at it.

// attemptCurrentSQL and attemptCurrentUnaliasedSQL are ADR 0012 §3's reader
// rule as a WHERE fragment: a row that another row supersedes is superseded
// history, and the row that supersedes it is current.
//
// Every aggregate over `attempts` applies it, and that single rule is what
// keeps a late-callback reconciliation from lying in either direction. Per-
// actor retry burn counts every attempt regardless of technical outcome, so
// without the rule one deadline reconciliation would make ONE dispatch look
// like two tries and inflate the actor's attempts-per-completion — the exact
// distortion issue #82 exists to remove. With it, the timed-out record drops
// out at the moment its correction lands. The same rule makes a usage rollup
// read the tokens the session actually reported rather than counting the
// superseded record's silence as an unreported attempt beside its own
// correction.
//
// It is deliberately NOT applied to the per-node-run attempts LISTING
// (engine_store.go's Attempts): a reader reconstructing what happened needs
// to see that the deadline fired and that the session reported afterwards.
// Aggregates count; listings recount.
//
// Two spellings because the queries that need it differ in whether they
// alias the table: `a` for the actorstats joins, the bare table name for the
// usage rollups.
const (
	attemptCurrentSQL          = ` AND NOT EXISTS (SELECT 1 FROM attempts sup WHERE sup.supersedes = a.id)`
	attemptCurrentUnaliasedSQL = ` AND NOT EXISTS (SELECT 1 FROM attempts sup WHERE sup.supersedes = attempts.id)`
)

// supersededCandidateSQL picks the attempt record a late report corrects:
// the newest row already recorded for this node run under the SAME fencing
// tuple the dispatch held.
//
// The fencing token is what makes the match precise rather than a guess.
// Every claim mints a new token, so a token identifies one dispatch — and on
// the deadline path the scheduler completes the timed-out attempt with
// exactly the invocation's own token (internal/scheduler's
// failWaitingExternal), so the row this finds is the record of THIS session's
// expiry and nothing else.
//
// It can legitimately find nothing. §13.4 lateness has a second flavour: a
// deadline returned the work item to `ready` and a different worker claimed
// it, bumping the fencing token, with no row ever written under the original.
// The late report is still recorded — the session ran — it simply corrects
// no earlier record, and ADR 0012 spells out what that costs.
const supersededCandidateSQL = `
SELECT id, started_at
FROM attempts
WHERE node_run_id = $1 AND namespace_id = $2 AND fencing_token = $3
ORDER BY attempt_number DESC
LIMIT 1
`

// recordSupersedingAttemptSQL appends the correcting record.
//
// The attempt number is minted inside the INSERT rather than read first and
// passed in, so the window between "read MAX+1" and "write it" is one
// statement wide instead of one round trip wide. It cannot be eliminated —
// a concurrent dispatch completing against the same node run can still take
// the number in between — so RecordSupersedingAttempt retries on exactly
// that constraint.
//
// ON CONFLICT targets migrations/0029's partial unique index over the
// report's DELIVERY identity — the protocol attempt id and the callback
// event id — which is what makes the append idempotent under §13.4's
// at-least-once delivery: callback ingest releases its event-id claim
// whenever processing fails part-way, so the same late report can honestly
// be processed twice, and the second pass must find its record already there
// rather than write a twin. DO NOTHING returns no row; the caller reads the
// existing record back instead.
//
// It deliberately does NOT target 0028's `supersedes` index, which was this
// statement's arbiter until ADR 0012 §5: that index is partial on
// `supersedes IS NOT NULL`, so it left the reconciliation that corrects
// nothing — §13.4's reclaimed-item flavour of lateness — with no guard at
// all. `supersedes` keeps its unique index for the semantic invariant (a
// record is corrected at most once), and a collision there is now a
// violation the caller resolves rather than a silent DO NOTHING.
const recordSupersedingAttemptSQL = `
INSERT INTO attempts (
	id, namespace_id, node_run_id, attempt_number, actor_id, status, fencing_token,
	result, started_at, completed_at,
	usage_input_tokens, usage_output_tokens, usage_cost, usage_currency,
	usage_cached_input_tokens, usage_reasoning_tokens, usage_model, usage_thread_id,
	termination_reason, continuation_ref,
	preserve_branch, preserve_pushed, preserve_remote,
	supersedes, late_callback_attempt_id, late_callback_event_id
)
SELECT $1, $2, $3,
       (SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM attempts WHERE node_run_id = $3),
       $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23,
       $24, $25
ON CONFLICT (late_callback_attempt_id, late_callback_event_id)
    WHERE late_callback_event_id IS NOT NULL DO NOTHING
RETURNING id, attempt_number
`

// selectSupersedingAttemptByDeliverySQL reads back the record an earlier
// delivery of THIS report already appended — the row the ON CONFLICT above
// declined to duplicate. It is keyed on the delivery rather than on
// `supersedes` because the report that corrects nothing has no other key.
const selectSupersedingAttemptByDeliverySQL = `
SELECT id, attempt_number, supersedes
FROM attempts
WHERE late_callback_attempt_id = $1 AND late_callback_event_id = $2
`

// selectSupersedingAttemptSQL reads back the correction a DIFFERENT delivery
// already appended for the same superseded record: 0028's `supersedes`
// uniqueness refusing a second correction of one record (ADR 0012 §2's
// chain-not-fan-out invariant), as distinct from a redelivery of the same
// report.
const selectSupersedingAttemptSQL = `
SELECT id, attempt_number
FROM attempts
WHERE supersedes = $1
`

// recordSupersedingAttemptRetries bounds the retry on a raced attempt
// number. Each retry re-reads MAX+1, and the only thing it races is another
// completion against the same node run — a node run has one dispatch in
// flight at a time, so more than one contender is already pathological.
const recordSupersedingAttemptRetries = 3

// RecordSupersedingAttempt appends the attempt record a late terminal
// callback leaves, correcting the record of the attempt it belongs to
// without rewriting it (PRD §10.4: records are immutable; corrections append
// with `supersedes`).
//
// It is deliberately NOT engine.CompleteAttempt. Nothing about the run
// moves: no work item is claimed or released, no node run changes status, no
// edge is followed, no ledger record is appended. §13.4's refusal stands
// exactly as it did. The only thing that happens is that the facts the
// session reported stop being invisible.
//
// The correction carries the report's own facts — status, output, usage
// (including the model), termination reason, continuation ref and the
// preserve block — plus two taken from the record it supersedes rather than
// from the report: the actor and the fencing tuple come from the durable
// invocation, because they are what the DISPATCH was, not what the reporter
// claims; and started_at is copied from the superseded row so the
// correction's duration measures the session, not the moment the report
// arrived. A correction with no superseded row to copy from starts at
// completed_at, which is the honest "this record cannot say how long it
// took" rather than an invented span back to nothing.
//
// callbackEventID is the §13.4 event id of the delivery being recorded, and
// with inv.AttemptID it is this record's idempotency key (migration 0029,
// ADR 0012 §5): a redelivery of the same report finds its own row and
// returns it instead of appending a twin. It is required — a report whose
// delivery has no identity cannot be recorded idempotently, and writing it
// unkeyed would silently re-open exactly the gap §5 closes. Callback ingest
// refuses an empty event id before any of this runs (HandleCallback's
// request-shape validation), so this refuses nothing a delivery could
// legitimately carry.
func (cs *CallbackStore) RecordSupersedingAttempt(
	ctx context.Context, inv actors.PendingInvocation, callbackEventID string, req engine.CompletionRequest,
) (actors.SupersedingAttempt, error) {
	if callbackEventID == "" {
		return actors.SupersedingAttempt{}, fmt.Errorf(
			"postgres: RecordSupersedingAttempt: a callback event id is required to record a late report idempotently")
	}
	var (
		supersededID pgtype.Text
		startedAt    pgtype.Timestamptz
	)
	err := cs.store.pool.QueryRow(ctx, supersededCandidateSQL,
		inv.NodeRunID, cs.namespaceID, inv.FencingToken).Scan(&supersededID, &startedAt)
	switch {
	case err != nil && isNoRows(err):
		// No earlier record under this dispatch's fencing tuple. The report
		// is still recorded; it corrects nothing.
		supersededID = pgtype.Text{}
		startedAt = pgtype.Timestamptz{}
	case err != nil:
		return actors.SupersedingAttempt{}, fmt.Errorf("postgres: RecordSupersedingAttempt: find superseded attempt: %w", err)
	}

	completedAt := tsOrNow(time.Now().UTC())
	if !startedAt.Valid {
		startedAt = completedAt
	}

	var (
		inputTokens, outputTokens          pgtype.Int8
		cost                               pgtype.Float8
		currency                           pgtype.Text
		cachedInputTokens, reasoningTokens pgtype.Int8
		usageModel, usageThreadID          pgtype.Text
	)
	if req.Usage != nil {
		inputTokens = int8FromPtr(&req.Usage.InputTokens)
		outputTokens = int8FromPtr(&req.Usage.OutputTokens)
		cost = float8FromPtr(req.Usage.Cost)
		currency = textPtrFromNullable(req.Usage.Currency)
		cachedInputTokens = int8FromPtr(req.Usage.CachedInputTokens)
		reasoningTokens = int8FromPtr(req.Usage.ReasoningTokens)
		usageModel = textPtrFromNullable(req.Usage.Model)
		usageThreadID = textPtrFromNullable(req.Usage.ThreadID)
	}
	var (
		preserveBranch pgtype.Text
		preservePushed pgtype.Bool
		preserveRemote pgtype.Text
	)
	if req.Preserve != nil {
		preserveBranch = textOrNull(req.Preserve.Branch)
		preservePushed = pgtype.Bool{Bool: req.Preserve.Pushed, Valid: true}
		preserveRemote = textOrNull(req.Preserve.Remote)
	}
	var result any
	if len(req.Output) > 0 {
		result = []byte(req.Output)
	}
	status := req.TechStatus
	if status == "" {
		status = engine.StatusFailed
	}

	for retry := 0; ; retry++ {
		var (
			id     string
			number int32
		)
		err := cs.store.pool.QueryRow(ctx, recordSupersedingAttemptSQL,
			store.NewULID(), cs.namespaceID, inv.NodeRunID,
			textOrNull(inv.ActorID), string(status), inv.FencingToken,
			result, startedAt, completedAt,
			inputTokens, outputTokens, cost, currency,
			cachedInputTokens, reasoningTokens, usageModel, usageThreadID,
			textPtrFromNullable(req.TerminationReason),
			textPtrFromNullable(req.ContinuationRef),
			preserveBranch, preservePushed, preserveRemote,
			supersededID, inv.AttemptID, callbackEventID,
		).Scan(&id, &number)

		switch {
		case err == nil:
			return actors.SupersedingAttempt{
				AttemptID:  id,
				Number:     int(number),
				Supersedes: textOrEmpty(supersededID),
			}, nil

		case errors.Is(err, pgx.ErrNoRows):
			// ON CONFLICT DO NOTHING: an earlier delivery of this same report
			// already recorded it. Report that record — the ingest's caller
			// must see the same answer whichever delivery got there first.
			return cs.supersedingAttemptForDelivery(ctx, inv.AttemptID, callbackEventID)

		case uniqueViolationConstraint(err) == "attempts_supersedes_key":
			// A DIFFERENT late report already corrected this same record
			// (ADR 0012 §2: one record is corrected at most once, so
			// corrections chain rather than fan out). Not a redelivery of
			// this report — the delivery key would have absorbed that — so
			// the honest answer is the correction that is already there,
			// which is what this path returned before the arbiter moved.
			return cs.existingSupersedingAttempt(ctx, supersededID)

		case uniqueViolationConstraint(err) == "attempts_node_run_attempt_number_key" && retry < recordSupersedingAttemptRetries:
			// A completion against the same node run took the number between
			// this statement's MAX+1 and its INSERT. Re-mint and try again.
			continue

		default:
			return actors.SupersedingAttempt{}, fmt.Errorf("postgres: RecordSupersedingAttempt: %w", err)
		}
	}
}

// supersedingAttemptForDelivery reads back the record an earlier delivery of
// this same report appended (migration 0029's delivery key). It reports the
// `supersedes` link as PERSISTED rather than as recomputed by this pass: the
// answer a redelivery gives has to be the record that exists, not this
// caller's second look at a candidate that may since have changed.
func (cs *CallbackStore) supersedingAttemptForDelivery(
	ctx context.Context, invocationAttemptID, callbackEventID string,
) (actors.SupersedingAttempt, error) {
	var (
		id         string
		number     int32
		supersedes pgtype.Text
	)
	err := cs.store.pool.QueryRow(ctx, selectSupersedingAttemptByDeliverySQL,
		invocationAttemptID, callbackEventID).Scan(&id, &number, &supersedes)
	if err != nil {
		return actors.SupersedingAttempt{}, fmt.Errorf(
			"postgres: RecordSupersedingAttempt: read back the record of callback event %s: %w", callbackEventID, err)
	}
	return actors.SupersedingAttempt{
		AttemptID:  id,
		Number:     int(number),
		Supersedes: textOrEmpty(supersedes),
	}, nil
}

// existingSupersedingAttempt reads back the correction a previous delivery
// appended for the same superseded record.
func (cs *CallbackStore) existingSupersedingAttempt(
	ctx context.Context, supersededID pgtype.Text,
) (actors.SupersedingAttempt, error) {
	var (
		id     string
		number int32
	)
	if err := cs.store.pool.QueryRow(ctx, selectSupersedingAttemptSQL, supersededID).Scan(&id, &number); err != nil {
		return actors.SupersedingAttempt{}, fmt.Errorf(
			"postgres: RecordSupersedingAttempt: read back existing correction: %w", err)
	}
	return actors.SupersedingAttempt{
		AttemptID:  id,
		Number:     int(number),
		Supersedes: textOrEmpty(supersededID),
	}, nil
}
