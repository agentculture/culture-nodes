package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// createRunRequest is components.schemas.CreateRunRequest.
type createRunRequest struct {
	WorkflowDigest string          `json:"workflow_digest"`
	Input          json.RawMessage `json:"input"`
}

// handleCreateRun is POST /v1alpha1/runs. It resolves the pinned,
// already-published workflow version by digest and hands its normalized
// IR straight to Engine.CreateRun — never recompiling from source, so a
// run always pins exactly the immutable bytes that digest addresses
// (PRD §20.1).
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) error {
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching CreateRunRequest: {workflow_digest, input}", "decode request body: %v", err)
	}
	if req.WorkflowDigest == "" {
		return badRequest("workflow_digest is required", "workflow_digest must not be empty")
	}

	ctx := r.Context()
	version, err := s.workflowVersionByDigest(ctx, req.WorkflowDigest)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("publish the workflow first via POST /v1alpha1/workflows", "no workflow version with digest %s", req.WorkflowDigest)
		}
		return internalError(err)
	}

	cw := &compiler.CompiledWorkflow{
		Format:     compiler.Format(version.SourceFormat),
		Source:     []byte(version.Source),
		Normalized: version.NormalizedIR,
		Digest:     version.ContentDigest,
	}

	run, err := s.Engine.CreateRun(ctx, cw, req.Input)
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, runOut(run))
	return nil
}

// handleListRuns is GET /v1alpha1/runs. updated_since/updated_until/sort
// (task t11) filter and order by updated_at, using
// runs_namespace_updated_at_idx (migrations/0010) — see listRuns in
// queries.go for the two query shapes and parseRunSort below for how the
// default sort column is chosen.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) error {
	updatedSince, err := parseRFC3339(r, "updated_since")
	if err != nil {
		return err
	}
	updatedUntil, err := parseRFC3339(r, "updated_until")
	if err != nil {
		return err
	}
	sort, err := parseRunSort(r, updatedSince != nil || updatedUntil != nil)
	if err != nil {
		return err
	}

	runs, err := s.listRuns(r.Context(), listRunsParams{
		State:        r.URL.Query().Get("state"),
		Limit:        parseLimit(r, 50, 500),
		UpdatedSince: updatedSince,
		UpdatedUntil: updatedUntil,
		Sort:         sort,
	})
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, RunListOut{Items: runs})
	return nil
}

// parseRunSort reads GET /v1alpha1/runs' "sort" query parameter: sortCreatedAt
// or sortUpdatedAt, always descending — every list in this API is
// newest-first, and this task does not add an ascending option anywhere.
// An explicit value that is neither is refused with 400 — silently falling
// back the way parseLimit does for an out-of-range page size would mean a
// caller who mistyped sort gets a page silently ordered differently from
// what they asked for, with no signal anything was wrong.
//
// Omitted (the common case), the default depends on timeWindowed
// (whether updated_since or updated_until was set): sortUpdatedAt when it
// is — a time-windowed query over updated_at is naturally read in that same
// order, and migrations/0010's (namespace_id, updated_at) index makes it the
// efficient one — and sortCreatedAt otherwise, preserving every pre-t11
// caller's behavior unchanged.
func parseRunSort(r *http.Request, timeWindowed bool) (string, error) {
	raw := r.URL.Query().Get("sort")
	switch raw {
	case "":
		if timeWindowed {
			return sortUpdatedAt, nil
		}
		return sortCreatedAt, nil
	case sortCreatedAt, sortUpdatedAt:
		return raw, nil
	default:
		return "", badRequest("sort must be created_at or updated_at", "unrecognized sort=%q", raw)
	}
}

// handleGetRun is GET /v1alpha1/runs/{id}: the Run-view payload.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	ctx := r.Context()

	run, err := s.engineStore.Run(ctx, id)
	if err != nil {
		return classify(err)
	}
	tokens, err := s.runTokens(ctx, id)
	if err != nil {
		return internalError(err)
	}
	nodeRuns, err := s.runNodeRuns(ctx, id)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, RunViewOut{Run: runOut(run), Tokens: tokens, NodeRuns: nodeRuns})
	return nil
}

// handleCancelRun is POST /v1alpha1/runs/{id}/cancel.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.cancelRun(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, runOut(run))
	return nil
}

// cancelRun consumes every active token, marks every non-terminal node run
// and every leasable work item cancelled, and moves the run to cancelled,
// all in one transaction under the run's advisory lock — the same
// ledger.RunLockKey(runID) the engine's own §12.5 completion transaction
// takes before it touches a run, so a cancel cannot interleave with a
// concurrent attempt completion of the same run. After that transaction
// commits, it best-effort propagates the cancellation to any actor an
// asynchronous node run was still waiting on (see cancelpropagate.go,
// issue #19).
//
// internal/engine has no CancelRun method (only a worker-reported
// TechStatus of "cancelled" flowing through CompleteAttempt for the one
// node run currently dispatched); a general "cancel this run from outside"
// operation is real product surface the OpenAPI spec requires but that
// package does not implement yet. Rather than inventing an engine feature
// this task does not own, this method reads and writes the same
// authoritative tables the engine's own transaction does, through
// (*postgres.Store).Pool() (the documented escape hatch), reproducing the
// audit-event-plus-outbox-row invariant PRD §12.5 steps 7 and 10 require —
// so a cancelled run still leaves exactly the same kind of durable trail a
// worker-driven completion would.
//
// Every leasable work_items row is cancelled here — 'ready', 'waiting' (an
// asynchronous actor invocation parked mid-flight, §12.6), and 'leased' (a
// worker actively holding it) alike. Earlier, only 'ready' rows were
// cancelled, on the theory that a 'leased' or 'waiting' row was a no-op to
// touch since nothing reclaims it anyway; that held for a completion arriving
// after cancellation (the engine's own fenced guard already refuses it) but
// not for RE-DISPATCH: a 'waiting' row left alone is exactly the row a fired
// deadline timer returns to 'ready' and a live worker then claims and
// dispatches all over again for a run that is supposed to be dead (issue
// #19). Cancelling it here removes it from every state ClaimWork,
// ReclaimExpired, or the deadline-timer effect would otherwise act on. This
// is race-safe against a worker completing the SAME row concurrently: this
// UPDATE runs inside the run's advisory-lock transaction, and
// Store.CompleteWork's/Store.ResumeWaitingWork's own fenced UPDATEs require
// `state = 'leased'`/`state = 'waiting'` respectively — once this commits
// with the row at 'cancelled', either the worker's completion already landed
// first (this UPDATE simply finds nothing to touch, since a completed row
// is no longer 'leased' either) or it lands after and matches zero rows,
// which the worker's engine.ErrStaleClaim / engine.TerminalNodeRunError
// handling already treats as a documented, tested no-op rather than an
// error it needs new handling for.
func (s *Server) cancelRun(ctx context.Context, runID string) (engine.Run, error) {
	tx, err := s.Store.Pool().Begin(ctx)
	if err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: begin: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, ledger.RunLockKey(runID)); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: lock: %w", err))
	}

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 AND namespace_id = $2`, runID, s.NamespaceID).Scan(&status)
	if err != nil {
		if isNoRowsErr(err) {
			return engine.Run{}, notFound("check the run id", "no run with id %s", runID)
		}
		return engine.Run{}, internalError(fmt.Errorf("cancel run: %w", err))
	}
	if engine.RunState(status).Terminal() {
		return engine.Run{}, conflict("the run has already reached a terminal state", "run %s is already %s", runID, status)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE runs SET status = 'cancelled', updated_at = now(), completed_at = now() WHERE id = $1`, runID,
	); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: update run: %w", err))
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tokens SET state = 'consumed', consumed_at = now() WHERE run_id = $1 AND state = 'active'`, runID,
	); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: consume tokens: %w", err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE node_runs SET status = 'cancelled', updated_at = now(), completed_at = now()
		WHERE run_id = $1 AND status NOT IN ('completed', 'failed', 'cancelled')`, runID,
	); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: cancel node runs: %w", err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE work_items SET state = 'cancelled', updated_at = now()
		WHERE state IN ('ready', 'waiting', 'leased') AND node_run_id IN (SELECT id FROM node_runs WHERE run_id = $1)`, runID,
	); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: cancel work items: %w", err))
	}

	var sequence int64
	if err := tx.QueryRow(ctx,
		`SELECT (COALESCE(MAX(sequence), 0) + 1)::bigint FROM events WHERE aggregate_id = $1`, runID,
	).Scan(&sequence); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: next event sequence: %w", err))
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id": runID,
		"state":  string(engine.RunCancelled),
		"detail": "cancelled via POST /v1alpha1/runs/{id}/cancel",
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO events (id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at)
		VALUES ($1, $2, 'run', $3, $4, $5, 'nodes', $6, now())`,
		store.NewULID(), s.NamespaceID, runID, sequence, engine.TypeRunCancelled, payload,
	); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: append event: %w", err))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
		VALUES ($1, $2, $3, $4, 'pending', now())`,
		store.NewULID(), s.NamespaceID, engine.TypeRunCancelled, payload,
	); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: append outbox: %w", err))
	}

	if err := tx.Commit(ctx); err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: commit: %w", err))
	}

	// PROPAGATE (issue #19): the run is now durably cancelled regardless of
	// what happens below — propagateCancelToActors is entirely best-effort
	// and never returns an error for cancelRun to surface.
	s.propagateCancelToActors(ctx, runID)

	updated, err := s.engineStore.Run(ctx, runID)
	if err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: re-read: %w", err))
	}
	return updated, nil
}
