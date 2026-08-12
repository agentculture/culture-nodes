package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// createRunRequest is components.schemas.CreateRunRequest. Name,
// Description, and Category (task t3, migrations/0013) are optional and
// additive: a body carrying only {workflow_digest, input} — every
// pre-t3 client — decodes identically to before, with all three as their
// zero value, and handleCreateRun below skips writing them entirely in
// that case (see setRunMetadata in queries.go).
type createRunRequest struct {
	WorkflowDigest string          `json:"workflow_digest"`
	Input          json.RawMessage `json:"input"`
	Name           string          `json:"name,omitempty"`
	Description    string          `json:"description,omitempty"`
	Category       string          `json:"category,omitempty"`
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
	// Task t3: name/description/category cannot ride inside
	// Engine.CreateRun's own transaction — engine.Run/InsertRun carry no
	// metadata columns (see runMetadata's doc comment in queries.go for
	// why that boundary stays put) — so this is a second statement right
	// after the run row exists. setRunMetadata no-ops when the request
	// carried none of the three, so an old {workflow_digest, input}-only
	// body never pays for it.
	if err := s.setRunMetadata(ctx, run.ID, req.Name, req.Description, req.Category); err != nil {
		return internalError(err)
	}
	usage, err := s.engineStore.RunUsage(ctx, run.ID)
	if err != nil {
		return internalError(err)
	}
	// The response reflects exactly what was just written above, with no
	// extra read: createRunRequest's fields ARE the metadata now
	// persisted.
	meta := runMetadata{Name: req.Name, Description: req.Description, Category: req.Category}
	writeJSON(w, http.StatusCreated, runOut(run, usage, meta))
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
	usage, err := s.engineStore.RunUsage(ctx, id)
	if err != nil {
		return internalError(err)
	}
	meta, err := s.runMetadataByID(ctx, id)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, RunViewOut{Run: runOut(run, usage, meta), Tokens: tokens, NodeRuns: nodeRuns})
	return nil
}

// handleCancelRun is POST /v1alpha1/runs/{id}/cancel.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.cancelRun(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	usage, err := s.engineStore.RunUsage(r.Context(), run.ID)
	if err != nil {
		return internalError(err)
	}
	meta, err := s.runMetadataByID(r.Context(), run.ID)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, runOut(run, usage, meta))
	return nil
}

// handlePatchRun is PATCH /v1alpha1/runs/{id}: retag a run's category —
// the only field this endpoint accepts. Frame decision q4 (docs/specs/
// 2026-08-12-operate-through-the-ui.md) makes name and description
// immutable once a run is created (POST /v1alpha1/runs is their only
// writer); this handler enforces that by inspecting the raw request body
// for either key BEFORE decoding a typed struct, since a typed decode with
// only a Category field would otherwise silently ignore a caller's attempt
// to also send name/description rather than refusing it with a structured
// error, the honesty condition this task's acceptance criteria ask for.
func (s *Server) handlePatchRun(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return badRequest("send a JSON body matching PatchRunRequest: {category}", "decode request body: %v", err)
	}
	if _, ok := raw["name"]; ok {
		return badRequest(
			"name is set at run creation only and cannot be changed afterward (frame decision q4) — remove it from the request body",
			"PATCH /v1alpha1/runs/%s: name is immutable", id)
	}
	if _, ok := raw["description"]; ok {
		return badRequest(
			"description is set at run creation only and cannot be changed afterward (frame decision q4) — remove it from the request body",
			"PATCH /v1alpha1/runs/%s: description is immutable", id)
	}
	categoryRaw, ok := raw["category"]
	if !ok {
		return badRequest("send a JSON body matching PatchRunRequest: {category}", "PATCH /v1alpha1/runs/%s requires category", id)
	}
	var category string
	if err := json.Unmarshal(categoryRaw, &category); err != nil {
		return badRequest("category must be a JSON string", "decode category: %v", err)
	}

	ctx := r.Context()
	if err := s.setRunCategory(ctx, id, category); err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the run id", "no run with id %s", id)
		}
		return internalError(err)
	}

	run, err := s.engineStore.Run(ctx, id)
	if err != nil {
		return classify(err)
	}
	usage, err := s.engineStore.RunUsage(ctx, id)
	if err != nil {
		return internalError(err)
	}
	meta, err := s.runMetadataByID(ctx, id)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, runOut(run, usage, meta))
	return nil
}

// hintCandidateKeys is deriveDisplayHint's priority-ordered list of exact
// top-level input keys checked first, before it falls back to substring
// matching — see that function's doc comment. Ordered by how common each
// shape is across this repo's own examples/ and the nodes-operator skill's
// assign workflow template, which binds "instruction"; examples/
// delivery-loop/input.json uses "request".
var hintCandidateKeys = []string{"instruction", "request", "task", "prompt", "summary"}

// hintCandidateSubstrings is the fallback deriveDisplayHint scans for when
// none of hintCandidateKeys is present verbatim — covers workflows whose
// input uses a compound key like "build_instruction" or
// "review_instruction" (examples/independent-review/input.json), which
// would otherwise derive no hint at all despite plainly carrying one.
var hintCandidateSubstrings = []string{"instruction", "request", "task"}

// displayHintMaxLen bounds RunOut.DisplayHint — a "sane length" for a list
// row or card, not a full task description. Truncation is rune-safe.
const displayHintMaxLen = 140

// deriveDisplayHint returns a truncated, best-effort hint at what a run is
// about, read from a request/instruction/task-ish top-level string field
// of input — RunOut's DisplayHint, computed here at read time (runOut,
// runRow.out()) and never persisted (see RunOut's doc comment in types.go
// for why a UI must be able to tell this apart from an operator-given
// Name). Returns "" when input is not a JSON object, or contains none of
// the candidate fields, or every candidate field it does contain is blank
// — deriving nothing is always preferred to guessing wrong.
func deriveDisplayHint(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		return "" // not a JSON object (an array, a scalar, ...) — nothing to derive from.
	}

	for _, key := range hintCandidateKeys {
		if hint := hintFromField(obj, key); hint != "" {
			return truncateHint(hint)
		}
	}

	// Fallback: scan keys in a deterministic (sorted) order for one whose
	// name contains one of the candidate substrings, so a compound key
	// like "build_instruction" still yields a hint. Sorted rather than map
	// iteration order, which Go deliberately randomizes — an unstable hint
	// across identical requests would be its own small honesty problem.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, sub := range hintCandidateSubstrings {
			if strings.Contains(k, sub) {
				if hint := hintFromField(obj, k); hint != "" {
					return truncateHint(hint)
				}
			}
		}
	}
	return ""
}

// hintFromField reads obj[key] as a JSON string, returning "" if the key
// is absent, is not a string, or is blank once trimmed.
func hintFromField(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// truncateHint bounds s to displayHintMaxLen runes, appending an ellipsis
// when it does. Rune-based, not byte-based: input is operator-authored
// free text and may contain multi-byte characters that a byte-slice
// truncation would split mid-codepoint.
func truncateHint(s string) string {
	r := []rune(s)
	if len(r) <= displayHintMaxLen {
		return s
	}
	return strings.TrimSpace(string(r[:displayHintMaxLen])) + "…"
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
	// Detached from the request context deliberately: the run is already
	// durably cancelled, so a client that disconnects the instant it gets
	// its response must not abort the propagation or its evidence events
	// mid-flight (PR #22 review). Still synchronous — moving this behind
	// the response entirely is the recorded outbox follow-up.
	s.propagateCancelToActors(context.WithoutCancel(ctx), runID)

	updated, err := s.engineStore.Run(ctx, runID)
	if err != nil {
		return engine.Run{}, internalError(fmt.Errorf("cancel run: re-read: %w", err))
	}
	return updated, nil
}
