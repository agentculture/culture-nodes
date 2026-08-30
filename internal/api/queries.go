package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// This file holds the read paths internal/store/postgres has no typed
// method for: listing workflow versions, resolving one by digest, listing
// runs, and reading a run's tokens/node runs/attempts for the Run-view
// payload. Every query here reads through (*postgres.Store).Pool(), which
// that package's own doc comment names as the sanctioned escape hatch "for
// callers ... that need raw SQL access beyond this package's typed
// methods" — internal/api is exactly such a caller, and adding these as
// one-off typed methods to internal/store/postgres itself would grow that
// package's surface for a need only this package has.

const workflowVersionColumns = `id, namespace_id, workflow_key, version, draft_id, owner_id,
	source_format, source, normalized_ir, content_digest, published_by_actor_id, created_at`

func scanWorkflowVersion(row pgxRow) (postgres.WorkflowVersion, error) {
	var (
		v                                    postgres.WorkflowVersion
		draftID, ownerID, publishedByActorID pgtype.Text
		createdAt                            pgtype.Timestamptz
	)
	if err := row.Scan(
		&v.ID, &v.NamespaceID, &v.WorkflowKey, &v.Version, &draftID, &ownerID,
		&v.SourceFormat, &v.Source, &v.NormalizedIR, &v.ContentDigest, &publishedByActorID, &createdAt,
	); err != nil {
		return postgres.WorkflowVersion{}, err
	}
	v.DraftID = textOrEmpty(draftID)
	v.OwnerID = textOrEmpty(ownerID)
	v.PublishedByActorID = textOrEmpty(publishedByActorID)
	v.CreatedAt = tsOrZero(createdAt)
	return v, nil
}

// pgxRow is the subset of pgx.Row/pgx.Rows this file scans with.
type pgxRow interface {
	Scan(dest ...any) error
}

func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func tsOrZero(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// workflowVersionByDigest resolves one workflow version by content digest,
// returning postgres.ErrNotFound when none matches.
func (s *Server) workflowVersionByDigest(ctx context.Context, digest string) (postgres.WorkflowVersion, error) {
	row := s.Store.Pool().QueryRow(ctx,
		`SELECT `+workflowVersionColumns+` FROM workflow_versions WHERE namespace_id = $1 AND content_digest = $2`,
		s.NamespaceID, digest)
	v, err := scanWorkflowVersion(row)
	if err != nil {
		if isNoRowsErr(err) {
			return postgres.WorkflowVersion{}, postgres.ErrNotFound
		}
		return postgres.WorkflowVersion{}, fmt.Errorf("api: workflow version %s: %w", digest, err)
	}
	return v, nil
}

// listWorkflowVersions returns published workflow versions, newest first,
// optionally filtered to one workflow key.
func (s *Server) listWorkflowVersions(ctx context.Context, workflowKey string, limit int) ([]postgres.WorkflowVersion, error) {
	rows, err := s.Store.Pool().Query(ctx,
		`SELECT `+workflowVersionColumns+` FROM workflow_versions
		 WHERE namespace_id = $1 AND ($2 = '' OR workflow_key = $2)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3`,
		s.NamespaceID, workflowKey, limit)
	if err != nil {
		return nil, fmt.Errorf("api: list workflow versions: %w", err)
	}
	defer rows.Close()

	out := make([]postgres.WorkflowVersion, 0)
	for rows.Next() {
		v, err := scanWorkflowVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("api: list workflow versions: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// runRow is one runs row joined to its pinned workflow digest, including
// task t3's name/description/category (migrations/0013) — listRuns' own
// SELECT reads them straight off the same row, unlike runOut's other call
// sites (createRun/getRun/cancelRun), which fetch them separately via
// runMetadataByID because they build a RunOut from an engine.Run rather
// than from this file's own runRow.
type runRow struct {
	ID             string
	WorkflowDigest string
	WorkflowKey    string
	Status         string
	Input          json.RawMessage
	Output         json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
	Name           string
	Description    string
	Category       string
	Subject        string
}

// out renders r as a RunOut. Usage stays unset here (see RunOut's doc
// comment on Usage — listRuns never computes a per-row rollup), but
// name/description/category and the input-derived DisplayHint render the
// same way runOut's other call sites do, since this listing already reads
// them off the same row at no extra query cost.
func (r runRow) out() RunOut {
	out := RunOut{
		ID:             r.ID,
		WorkflowDigest: r.WorkflowDigest,
		WorkflowKey:    r.WorkflowKey,
		State:          r.Status,
		Input:          nonNullJSON(r.Input),
		Output:         nonNullJSON(r.Output),
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		Name:           r.Name,
		Description:    r.Description,
		Category:       r.Category,
		Subject:        r.Subject,
	}
	if r.Name == "" {
		out.DisplayHint = deriveDisplayHint(r.Input)
	}
	if !r.CompletedAt.IsZero() {
		completedAt := r.CompletedAt
		out.CompletedAt = &completedAt
	}
	return out
}

// sortCreatedAt and sortUpdatedAt are the two values GET /v1alpha1/runs'
// "sort" query parameter accepts (see parseRunSort in runs.go). Always
// descending — every list in this API is newest-first; there is no
// ascending option anywhere in this surface, and this task does not add one.
const (
	sortCreatedAt = "created_at"
	sortUpdatedAt = "updated_at"
)

// listRunsParams bundles GET /v1alpha1/runs' query parameters (task t11
// adds UpdatedSince, UpdatedUntil, and Sort to the pre-existing State and
// Limit). Bundled rather than positional: two bare *time.Time parameters
// next to each other invites a since/until swap at the call site that the
// compiler cannot catch, and a struct's field names make each argument
// self-documenting at handleListRuns' single call site.
type listRunsParams struct {
	State        string
	Subject      string
	WorkflowKey  string
	Cursor       *nodeRunCursor
	Limit        int
	UpdatedSince *time.Time
	UpdatedUntil *time.Time
	// Sort is sortCreatedAt or sortUpdatedAt, always descending; see
	// parseRunSort in runs.go for how the default is chosen and validated
	// before this is called.
	Sort string
}

// listRuns returns runs newest first by p.Sort, optionally filtered to one
// state and/or an updated_at window. The two branches below are separate,
// fully literal query strings — not one query built with a dynamically
// interpolated ORDER BY — so each is exactly the query
// internal/store/postgres/updated_at_index_test.go's
// TestRunsUpdatedAtSortedListingQueryUsesIndexScan EXPLAINs: proof that
// query plan uses runs_namespace_updated_at_idx (migrations/0010) stays
// proof about the literal query this function actually runs, not a
// simplified stand-in.
func (s *Server) listRuns(ctx context.Context, p listRunsParams) ([]RunOut, string, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if p.Sort == sortUpdatedAt {
		rows, err = s.Store.Pool().Query(ctx, `
			SELECT r.id, wv.content_digest, wv.workflow_key, r.status, r.input, r.output, r.created_at, r.updated_at, r.completed_at,
			       r.name, r.description, r.category, COALESCE(r.subject,'')
			FROM runs r JOIN workflow_versions wv ON wv.id = r.workflow_version_id
			WHERE r.namespace_id = $1
			  AND ($2 = '' OR r.status = $2)
			  AND ($3::timestamptz IS NULL OR r.updated_at >= $3)
			  AND ($4::timestamptz IS NULL OR r.updated_at <= $4)
			  AND ($5 = '' OR r.subject = $5)
			  AND ($6 = '' OR wv.workflow_key = $6)
			  AND ($7::timestamptz IS NULL OR (r.updated_at, r.id) < ($7, $8))
			ORDER BY r.updated_at DESC, r.id DESC
			LIMIT $9`,
			s.NamespaceID, p.State, p.UpdatedSince, p.UpdatedUntil, p.Subject, p.WorkflowKey, cursorTime(p.Cursor), cursorID(p.Cursor), p.Limit+1)
	} else {
		rows, err = s.Store.Pool().Query(ctx, `
			SELECT r.id, wv.content_digest, wv.workflow_key, r.status, r.input, r.output, r.created_at, r.updated_at, r.completed_at,
			       r.name, r.description, r.category, COALESCE(r.subject,'')
			FROM runs r JOIN workflow_versions wv ON wv.id = r.workflow_version_id
			WHERE r.namespace_id = $1
			  AND ($2 = '' OR r.status = $2)
			  AND ($3::timestamptz IS NULL OR r.updated_at >= $3)
			  AND ($4::timestamptz IS NULL OR r.updated_at <= $4)
			  AND ($5 = '' OR r.subject = $5)
			  AND ($6 = '' OR wv.workflow_key = $6)
			  AND ($7::timestamptz IS NULL OR (r.created_at, r.id) < ($7, $8))
			ORDER BY r.created_at DESC, r.id DESC
			LIMIT $9`,
			s.NamespaceID, p.State, p.UpdatedSince, p.UpdatedUntil, p.Subject, p.WorkflowKey, cursorTime(p.Cursor), cursorID(p.Cursor), p.Limit+1)
	}
	if err != nil {
		return nil, "", fmt.Errorf("api: list runs: %w", err)
	}
	defer rows.Close()

	out := make([]RunOut, 0)
	for rows.Next() {
		var (
			r                           runRow
			input                       []byte
			output                      []byte
			createdAt                   pgtype.Timestamptz
			updatedAt                   pgtype.Timestamptz
			completedAt                 pgtype.Timestamptz
			name, description, category pgtype.Text
		)
		if err := rows.Scan(
			&r.ID, &r.WorkflowDigest, &r.WorkflowKey, &r.Status, &input, &output, &createdAt, &updatedAt, &completedAt,
			&name, &description, &category, &r.Subject,
		); err != nil {
			return nil, "", fmt.Errorf("api: list runs: scan: %w", err)
		}
		r.Input = json.RawMessage(input)
		r.Output = json.RawMessage(output)
		r.CreatedAt = tsOrZero(createdAt)
		r.UpdatedAt = tsOrZero(updatedAt)
		r.CompletedAt = tsOrZero(completedAt)
		r.Name = textOrEmpty(name)
		r.Description = textOrEmpty(description)
		r.Category = textOrEmpty(category)
		out = append(out, r.out())
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > p.Limit {
		out = out[:p.Limit]
		last := out[len(out)-1]
		at := last.CreatedAt
		if p.Sort == sortUpdatedAt {
			at = last.UpdatedAt
		}
		next = encodeNodeRunCursor(nodeRunCursor{UpdatedAt: at, ID: last.ID})
	}
	return out, next, nil
}

func cursorTime(c *nodeRunCursor) *time.Time {
	if c == nil {
		return nil
	}
	return &c.UpdatedAt
}
func cursorID(c *nodeRunCursor) string {
	if c == nil {
		return ""
	}
	return c.ID
}

// runTokens returns every token of a run, oldest first.
func (s *Server) runTokens(ctx context.Context, runID string) ([]TokenOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, node_key, state, parent_token_id, origin_event_id, created_at, consumed_at
		FROM tokens WHERE run_id = $1 ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("api: run %s: list tokens: %w", runID, err)
	}
	defer rows.Close()

	out := make([]TokenOut, 0)
	for rows.Next() {
		var (
			t             TokenOut
			parentTokenID pgtype.Text
			originEventID pgtype.Text
			createdAt     pgtype.Timestamptz
			consumedAt    pgtype.Timestamptz
		)
		if err := rows.Scan(&t.ID, &t.NodeID, &t.State, &parentTokenID, &originEventID, &createdAt, &consumedAt); err != nil {
			return nil, fmt.Errorf("api: run %s: list tokens: scan: %w", runID, err)
		}
		t.ParentTokenID = textOrEmpty(parentTokenID)
		t.OriginEventID = textOrEmpty(originEventID)
		t.CreatedAt = tsOrZero(createdAt)
		if consumedAt.Valid {
			consumed := consumedAt.Time
			t.ConsumedAt = &consumed
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// runNodeRuns returns every node run of a run, oldest first, each with its
// attempts nested (oldest first).
func (s *Server) runNodeRuns(ctx context.Context, runID string) ([]NodeRunOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, token_id, node_key, status, outcome, visit_count, created_at, updated_at, completed_at
		FROM node_runs WHERE run_id = $1 ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("api: run %s: list node runs: %w", runID, err)
	}

	out := make([]NodeRunOut, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var (
			nr          NodeRunOut
			tokenID     pgtype.Text
			outcome     pgtype.Text
			createdAt   pgtype.Timestamptz
			updatedAt   pgtype.Timestamptz
			completedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&nr.ID, &tokenID, &nr.NodeID, &nr.State, &outcome, &nr.VisitCount, &createdAt, &updatedAt, &completedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("api: run %s: list node runs: scan: %w", runID, err)
		}
		nr.TokenID = textOrEmpty(tokenID)
		nr.Outcome = textOrEmpty(outcome)
		nr.CreatedAt = tsOrZero(createdAt)
		nr.UpdatedAt = tsOrZero(updatedAt)
		if completedAt.Valid {
			completed := completedAt.Time
			nr.CompletedAt = &completed
		}
		nr.Attempts = []AttemptOut{}
		byID[nr.ID] = len(out)
		out = append(out, nr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: run %s: list node runs: %w", runID, err)
	}

	attemptRows, err := s.Store.Pool().Query(ctx, `
		SELECT a.id, a.node_run_id, a.attempt_number, a.actor_id, a.status, a.fencing_token, a.result, a.started_at, a.completed_at,
		       a.preserve_branch, a.preserve_pushed, a.preserve_remote,
		       a.usage_input_tokens, a.usage_output_tokens, a.usage_cost, a.usage_currency,
		       a.usage_cached_input_tokens, a.usage_reasoning_tokens, a.usage_model, a.usage_thread_id,
		       a.termination_reason, a.continuation_ref, a.supersedes
		FROM attempts a JOIN node_runs nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1
		ORDER BY a.node_run_id, a.attempt_number`, runID)
	if err != nil {
		return nil, fmt.Errorf("api: run %s: list attempts: %w", runID, err)
	}
	defer attemptRows.Close()

	for attemptRows.Next() {
		var (
			a                                                                          AttemptOut
			actorID                                                                    pgtype.Text
			fencingToken                                                               pgtype.Int8
			result                                                                     []byte
			startedAt                                                                  pgtype.Timestamptz
			completedAt                                                                pgtype.Timestamptz
			preserveBranch                                                             pgtype.Text
			preservePushed                                                             pgtype.Bool
			preserveRemote                                                             pgtype.Text
			usageInput, usageOutput, usageCached, usageReasoning                       pgtype.Int8
			usageCost                                                                  pgtype.Float8
			usageCurrency, usageModel, usageThread, terminationReason, continuationRef pgtype.Text
			supersedes                                                                 pgtype.Text
		)
		if err := attemptRows.Scan(&a.ID, &a.NodeRunID, &a.AttemptNumber, &actorID, &a.Status, &fencingToken, &result, &startedAt, &completedAt,
			&preserveBranch, &preservePushed, &preserveRemote,
			&usageInput, &usageOutput, &usageCost, &usageCurrency, &usageCached, &usageReasoning, &usageModel, &usageThread,
			&terminationReason, &continuationRef, &supersedes); err != nil {
			return nil, fmt.Errorf("api: run %s: list attempts: scan: %w", runID, err)
		}
		a.ActorID = textOrEmpty(actorID)
		if fencingToken.Valid {
			a.FencingToken = fencingToken.Int64
		}
		a.Result = nonNullJSON(result)
		// The attempt this record corrects (task t11, ADR 0012). Both rows
		// are listed -- the aggregates are where superseded history drops
		// out, not the run's own account of what happened.
		a.Supersedes = textOrEmpty(supersedes)
		// NULL since migration 0049: an attempt with no invocation row has an
		// unknown start, which is omitted rather than rendered as year 1.
		if startedAt.Valid {
			started := startedAt.Time
			a.StartedAt = &started
		}
		if completedAt.Valid {
			completed := completedAt.Time
			a.CompletedAt = &completed
		}
		// preserve_branch is the presence check (migrations/0025's own
		// header): pushed/remote are only ever written alongside it.
		if preserveBranch.Valid {
			a.PreserveBranch = preserveBranch.String
			pushed := preservePushed.Bool
			a.PreservePushed = &pushed
			a.PreserveRemote = textOrEmpty(preserveRemote)
		}
		if usageInput.Valid {
			a.Usage = &AttemptUsageOut{InputTokens: usageInput.Int64, OutputTokens: usageOutput.Int64}
			if usageCost.Valid {
				cost := usageCost.Float64
				a.Usage.Cost = &cost
			}
			a.Usage.Currency = textOrEmpty(usageCurrency)
			if usageCached.Valid {
				value := usageCached.Int64
				a.Usage.CachedInputTokens = &value
			}
			if usageReasoning.Valid {
				value := usageReasoning.Int64
				a.Usage.ReasoningTokens = &value
			}
			if usageModel.Valid {
				value := usageModel.String
				a.Usage.UsageModel = &value
			}
			if usageThread.Valid {
				value := usageThread.String
				a.Usage.ThreadID = &value
			}
		}
		a.TerminationReason = textOrEmpty(terminationReason)
		a.ContinuationRef = textOrEmpty(continuationRef)

		idx, ok := byID[a.NodeRunID]
		if !ok {
			continue // an attempt for a node run outside this page cannot happen (both queries share runID), but stay defensive.
		}
		out[idx].Attempts = append(out[idx].Attempts, a)
	}
	return out, attemptRows.Err()
}

// runMetadata is task t3's optional run name/description/category triple
// (migrations/0013). It is read and written directly against the `runs`
// table through (*postgres.Store).Pool() rather than threaded through
// engine.Run/postgres.EngineStore: the engine's own state machine never
// reads or branches on these fields, so growing its Store interface for a
// need only this package's HTTP surface has would widen that boundary for
// nothing — the same "escape hatch" reasoning this file's header comment
// already gives for the run/token/node-run/attempt reads above.
type runMetadata struct {
	Name        string
	Description string
	Category    string
}

// runMetadataByID reads one run's name/description/category, returning
// postgres.ErrNotFound when no run with this id exists in this server's
// namespace — the same sentinel workflowVersionByDigest returns above, so
// callers can classify() it the same way.
func (s *Server) runMetadataByID(ctx context.Context, runID string) (runMetadata, error) {
	var name, description, category pgtype.Text
	err := s.Store.Pool().QueryRow(ctx,
		`SELECT name, description, category FROM runs WHERE id = $1 AND namespace_id = $2`,
		runID, s.NamespaceID,
	).Scan(&name, &description, &category)
	if err != nil {
		if isNoRowsErr(err) {
			return runMetadata{}, postgres.ErrNotFound
		}
		return runMetadata{}, fmt.Errorf("api: run %s: metadata: %w", runID, err)
	}
	return runMetadata{Name: textOrEmpty(name), Description: textOrEmpty(description), Category: textOrEmpty(category)}, nil
}

// setRunCategory retags an existing run's category alone — POST-creation,
// through PATCH /v1alpha1/runs/{id} (handlePatchRun) — per frame decision
// q4: category is the one field of runMetadata retaggable after creation;
// name and description are immutable once set (enforced by handlePatchRun
// refusing either key in the request body before this is ever called). An
// empty category clears the tag (NULLIF), the same "absent, not empty
// string" contract setRunMetadata above uses. Returns postgres.ErrNotFound
// when no run with this id exists in this server's namespace, so
// handlePatchRun can classify() it into the documented 404 the same way
// every other run lookup in this package does.
func (s *Server) setRunCategory(ctx context.Context, runID, category string) error {
	tag, err := s.Store.Pool().Exec(ctx,
		`UPDATE runs SET category = NULLIF($2, ''), updated_at = now() WHERE id = $1 AND namespace_id = $3`,
		runID, category, s.NamespaceID,
	)
	if err != nil {
		return fmt.Errorf("api: run %s: set category: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return postgres.ErrNotFound
	}
	return nil
}

// nodeRunCursor is the decoded form of GET /v1alpha1/node-runs' opaque
// "cursor" query parameter: the (updated_at, id) of the last row the caller
// already has, in this listing's own sort order (updated_at DESC, id DESC).
// See listNodeRunsAcrossRuns' doc comment for why this endpoint paginates by
// keyset cursor rather than OFFSET.
type nodeRunCursor struct {
	UpdatedAt time.Time
	ID        string
}

// nodeRunCursorSeparator joins encodeNodeRunCursor's two fields before
// base64-encoding. A pipe never appears in a ULID (store.NewULID's
// alphabet) or in time.RFC3339Nano's output, so a single SplitN(..., 2) in
// decodeNodeRunCursor is an unambiguous inverse.
const nodeRunCursorSeparator = "|"

// encodeNodeRunCursor renders c as the opaque token GET /v1alpha1/node-runs'
// response carries in next_cursor for a caller to round-trip back as
// "cursor". Nanosecond-precision RFC3339 preserves exactly the instant
// Postgres returned — the cursor is compared against updated_at with a
// strict inequality/equality pair in listNodeRunsAcrossRuns' query, not a
// tolerant range, so truncating precision here could skip or repeat the
// boundary row.
func encodeNodeRunCursor(c nodeRunCursor) string {
	raw := c.UpdatedAt.UTC().Format(time.RFC3339Nano) + nodeRunCursorSeparator + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeNodeRunCursor is encodeNodeRunCursor's inverse. handleListNodeRuns
// renders any returned error as 400: a cursor a client did not just receive
// from this endpoint's own next_cursor is refused rather than silently
// treated as "no cursor" (which would silently restart pagination from the
// first page instead of reporting the caller's mistake).
func decodeNodeRunCursor(s string) (nodeRunCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nodeRunCursor{}, fmt.Errorf("not valid base64: %w", err)
	}
	parts := strings.SplitN(string(raw), nodeRunCursorSeparator, 2)
	if len(parts) != 2 || parts[1] == "" {
		return nodeRunCursor{}, fmt.Errorf("does not decode to <updated_at>%s<id>", nodeRunCursorSeparator)
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nodeRunCursor{}, fmt.Errorf("updated_at component: %w", err)
	}
	return nodeRunCursor{UpdatedAt: t, ID: parts[1]}, nil
}

// nodeRunListItemRow is one row of the cross-run node_runs listing before
// its actor_id is attached (see latestAttemptActorIDs below) and it is
// rendered as NodeRunListItemOut.
type nodeRunListItemRow struct {
	ID          string
	RunID       string
	NodeKey     string
	State       string
	Outcome     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
}

func (r nodeRunListItemRow) out(actorID string, usage postgres.UsageRollup) NodeRunListItemOut {
	out := NodeRunListItemOut{
		ID:        r.ID,
		RunID:     r.RunID,
		NodeID:    r.NodeKey,
		ActorID:   actorID,
		State:     r.State,
		Outcome:   r.Outcome,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		Usage:     usageOut(usage),
	}
	if !r.CompletedAt.IsZero() {
		completedAt := r.CompletedAt
		out.CompletedAt = &completedAt
	}
	return out
}

// listNodeRunsAcrossRuns is GET /v1alpha1/node-runs' query (task t11): every
// node run in this namespace — not scoped to one run, unlike runNodeRuns
// above — newest-first by updated_at, filtered to an optional
// [updatedSince, updatedUntil] window and paged by an optional keyset
// cursor. It uses node_runs_namespace_updated_at_idx (migrations/0010) for
// the namespace scope, the updated_at range, and the ORDER BY in one index
// walk — see internal/store/postgres/updated_at_index_test.go's
// TestNodeRunsCrossRunListingQueryUsesIndexScan, which EXPLAINs this exact
// query text for both a first page and a cursor-engaged later page.
//
// Pagination is a keyset cursor, not OFFSET: node_runs.updated_at changes on
// every state transition of any node run in the namespace, including ones
// outside the caller's current page, so an OFFSET N silently skips or
// repeats rows as items elsewhere in the table move past the offset
// boundary between two page requests — exactly the client-visible
// correctness failure a "jobs timeline" that is read while runs are still
// progressing would hit constantly. A keyset cursor anchored to
// (updated_at, id) of the last row already served has no such failure mode:
// "give me what comes after this exact row, in this exact order" stays
// correct no matter what else in the table changes concurrently, and it
// reuses the identical index walk a first page already does rather than a
// second, more expensive plan (no COUNT, no OFFSET-scan-and-discard).
//
// It fetches one row beyond limit to detect whether a further page exists
// (returning a non-empty nextCursor exactly when it does) without a second
// round trip — the same page-boundary technique keyset pagination
// implementations conventionally use.
func (s *Server) listNodeRunsAcrossRuns(ctx context.Context, updatedSince, updatedUntil *time.Time, cursor *nodeRunCursor, limit int) ([]NodeRunListItemOut, string, error) {
	var cursorUpdatedAt *time.Time
	var cursorID string
	if cursor != nil {
		cursorUpdatedAt = &cursor.UpdatedAt
		cursorID = cursor.ID
	}

	rows, err := s.Store.Pool().Query(ctx, `
		SELECT nr.id, nr.run_id, nr.node_key, nr.status, nr.outcome, nr.created_at, nr.updated_at, nr.completed_at
		FROM node_runs nr
		WHERE nr.namespace_id = $1
		  AND ($2::timestamptz IS NULL OR nr.updated_at >= $2)
		  AND ($3::timestamptz IS NULL OR nr.updated_at <= $3)
		  AND ($4::timestamptz IS NULL OR nr.updated_at < $4 OR (nr.updated_at = $4 AND nr.id < $5))
		ORDER BY nr.updated_at DESC, nr.id DESC
		LIMIT $6`,
		s.NamespaceID, updatedSince, updatedUntil, cursorUpdatedAt, cursorID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("api: list node runs: %w", err)
	}

	var scanned []nodeRunListItemRow
	for rows.Next() {
		var (
			r           nodeRunListItemRow
			outcome     pgtype.Text
			createdAt   pgtype.Timestamptz
			updatedAt   pgtype.Timestamptz
			completedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&r.ID, &r.RunID, &r.NodeKey, &r.State, &outcome, &createdAt, &updatedAt, &completedAt); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("api: list node runs: scan: %w", err)
		}
		r.Outcome = textOrEmpty(outcome)
		r.CreatedAt = tsOrZero(createdAt)
		r.UpdatedAt = tsOrZero(updatedAt)
		r.CompletedAt = tsOrZero(completedAt)
		scanned = append(scanned, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("api: list node runs: %w", err)
	}

	var nextCursor string
	if len(scanned) > limit {
		last := scanned[limit-1]
		nextCursor = encodeNodeRunCursor(nodeRunCursor{UpdatedAt: last.UpdatedAt, ID: last.ID})
		scanned = scanned[:limit]
	}

	ids := make([]string, len(scanned))
	for i, r := range scanned {
		ids[i] = r.ID
	}
	actorByNodeRun, err := s.latestAttemptActorIDs(ctx, ids)
	if err != nil {
		return nil, "", err
	}
	// Task t2: batched the same way actorByNodeRun above is — one extra
	// round trip for the whole page rather than one per row — see
	// postgres.EngineStore.NodeRunUsages' doc comment.
	usageByNodeRun, err := s.engineStore.NodeRunUsages(ctx, ids)
	if err != nil {
		return nil, "", fmt.Errorf("api: list node runs: usage: %w", err)
	}

	out := make([]NodeRunListItemOut, len(scanned))
	for i, r := range scanned {
		out[i] = r.out(actorByNodeRun[r.ID], usageByNodeRun[r.ID])
	}
	return out, nextCursor, nil
}

// latestAttemptActorIDs returns, for each of nodeRunIDs that has at least
// one attempt, the actor_id of its highest-numbered (most recent) attempt —
// the "actor/runner reference" listNodeRunsAcrossRuns reports per row (this
// is the identical attempts.actor_id column AttemptOut.ActorID already
// exposes, including for code nodes: internal/worker's runner dispatch
// paths (code.go, runnerasync.go) record their own runner actor there via
// codeRunnerActorID(), so this one column already answers both "which actor"
// and "which runner" without a second, dispatch-kind-specific lookup). A
// node run with no attempts yet (still 'ready', never dispatched) is simply
// absent from the returned map; callers treat that as an empty actor_id, the
// same optional reference AttemptOut.ActorID already is elsewhere in this
// package.
//
// This is a second, small query — bounded by the page's own limit, so at
// most a few hundred ids per call — rather than a LATERAL join folded into
// listNodeRunsAcrossRuns' primary query: keeping that query's shape
// identical to the one TestNodeRunsCrossRunListingQueryUsesIndexScan
// EXPLAINs means that proof stays proof about the query this method actually
// runs. It is the same two-query split runNodeRuns above already uses for
// node runs plus their attempts, one level up.
func (s *Server) latestAttemptActorIDs(ctx context.Context, nodeRunIDs []string) (map[string]string, error) {
	if len(nodeRunIDs) == 0 {
		return map[string]string{}, nil
	}

	rows, err := s.Store.Pool().Query(ctx, `
		SELECT a.node_run_id, a.actor_id
		FROM attempts a
		WHERE a.node_run_id = ANY($1)
		ORDER BY a.node_run_id, a.attempt_number`, nodeRunIDs)
	if err != nil {
		return nil, fmt.Errorf("api: list node runs: latest attempt actor ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var nodeRunID string
		var actorID pgtype.Text
		if err := rows.Scan(&nodeRunID, &actorID); err != nil {
			return nil, fmt.Errorf("api: list node runs: latest attempt actor ids: scan: %w", err)
		}
		// Ascending attempt_number: each row for the same node_run_id
		// overwrites the last, so the final value per key is the
		// highest-numbered (most recent) attempt's actor.
		out[nodeRunID] = textOrEmpty(actorID)
	}
	return out, rows.Err()
}

// listHumanTasks returns human tasks newest first, optionally filtered to
// one status ("pending" or "decided"), scoped to this server's namespace —
// the same shape listRuns and listWorkflowVersions use above.
func (s *Server) listHumanTasks(ctx context.Context, status string, limit int) ([]HumanTaskOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, run_id, node_run_id, kind, assigned_owner_id, status, request, response, created_at, resolved_at
		FROM human_tasks
		WHERE namespace_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3`,
		s.NamespaceID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("api: list human tasks: %w", err)
	}
	defer rows.Close()

	out := make([]HumanTaskOut, 0)
	for rows.Next() {
		var (
			t               HumanTaskOut
			nodeRunID       pgtype.Text
			assignedOwnerID pgtype.Text
			request         []byte
			response        []byte
			createdAt       pgtype.Timestamptz
			resolvedAt      pgtype.Timestamptz
		)
		if err := rows.Scan(
			&t.ID, &t.RunID, &nodeRunID, &t.Kind, &assignedOwnerID, &t.Status,
			&request, &response, &createdAt, &resolvedAt,
		); err != nil {
			return nil, fmt.Errorf("api: list human tasks: scan: %w", err)
		}
		t.NodeRunID = textOrEmpty(nodeRunID)
		t.AssignedOwnerID = textOrEmpty(assignedOwnerID)
		t.Request = json.RawMessage(request)
		t.Response = nonNullJSON(json.RawMessage(response))
		t.CreatedAt = tsOrZero(createdAt)
		if resolvedAt.Valid {
			resolved := resolvedAt.Time
			t.ResolvedAt = &resolved
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// isNoRowsErr reports whether err is pgx's "no rows in result set"
// sentinel, mirroring internal/store/postgres's own isNoRows helper (that
// one is unexported, so this package needs its own copy).
func isNoRowsErr(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
