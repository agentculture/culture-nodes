package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/store"
)

// Durable plan/wave/task/deviation import state (migration 0024, task t22
// of the economy-discord-graphs plan; issue #45, spec claims c10/c15,
// honesty h7/h11). See migrations/0024_plan_imports.sql's header for why
// this is a purpose-built schema rather than ledger_records.

// PlanImportTaskInput is one task of an ImportPlanInput. Field names mirror
// internal/devague.PlanTask; this package does not import internal/devague
// (a store must not depend on a specific source adapter) so the caller
// (internal/api's plan-import handler) is the seam that translates
// devague.PlanTask into this shape.
type PlanImportTaskInput struct {
	TaskRef            string
	Summary            string
	Instruction        string
	OriginKind         string // "human" | "agent"
	SourceStatus       string
	DependsOn          []string
	Wave               *int
	AcceptanceCriteria []string
	Covers             []string
}

// PlanImportDeviationInput is one deviation of an ImportPlanInput. Field
// names mirror internal/devague.Deviation, for the same reason
// PlanImportTaskInput's do.
type PlanImportDeviationInput struct {
	DeviationRef   string
	What           string
	TaskRef        string
	Reason         string
	Affects        []string
	OriginKind     string // "human" | "agent"
	SourceStatus   string
	Classification string // "" means "devague recorded none"
}

// ImportPlanInput is the input to Store.ImportPlan / EngineStore.ImportPlan.
type ImportPlanInput struct {
	NamespaceID  string
	Slug         string
	Title        string
	SourceSlug   string
	SourceStatus string
	SourceDigest string
	Tasks        []PlanImportTaskInput
	Deviations   []PlanImportDeviationInput
}

// PlanImportTask is one plan_import_tasks row.
type PlanImportTask struct {
	ID string
	PlanImportTaskInput
}

// PlanImportDeviation is one plan_import_deviations row.
type PlanImportDeviation struct {
	ID string
	PlanImportDeviationInput
}

// PlanImport is one plan_imports row plus its tasks and deviations -- the
// full snapshot ImportPlan writes and GetPlanImport reads back.
type PlanImport struct {
	ID           string
	NamespaceID  string
	Slug         string
	Title        string
	SourceSlug   string
	SourceStatus string
	SourceDigest string
	ImportedAt   time.Time
	Tasks        []PlanImportTask
	Deviations   []PlanImportDeviation
}

// ImportPlan persists one plan snapshot (Store-level entry point, explicit
// namespace -- the internal/store/postgres test-suite convention; see
// EngineStore.ImportPlan for the namespace-bound API-layer entry point).
func (s *Store) ImportPlan(ctx context.Context, in ImportPlanInput) (PlanImport, error) {
	return importPlan(ctx, s.pool, in)
}

// GetPlanImport returns one plan import snapshot (Store-level entry point).
func (s *Store) GetPlanImport(ctx context.Context, namespaceID, id string) (PlanImport, error) {
	return getPlanImport(ctx, s.pool, namespaceID, id)
}

// ListPlanImports lists every import snapshot with the given slug
// (Store-level entry point).
func (s *Store) ListPlanImports(ctx context.Context, namespaceID, slug string) ([]PlanImport, error) {
	return listPlanImports(ctx, s.pool, namespaceID, slug)
}

// The namespace-bound mirror for the API surface (internal/api's
// plan-import handler holds an *EngineStore, not a bare *Store) --
// actoravailability.go's engineQueries convention, applied to EngineStore
// directly rather than to engineQueries: ImportPlan needs its OWN
// transaction (several statements that must commit together), and only
// EngineStore (not the narrower engineQueries, which may already be running
// inside someone else's transaction) safely owns a *pgxpool.Pool to open
// one from.

// ImportPlan persists one plan snapshot, scoped to es's namespace.
func (es *EngineStore) ImportPlan(ctx context.Context, in ImportPlanInput) (PlanImport, error) {
	in.NamespaceID = es.namespaceID
	return importPlan(ctx, es.pool, in)
}

// GetPlanImport returns one plan import snapshot, scoped to es's namespace.
func (es *EngineStore) GetPlanImport(ctx context.Context, id string) (PlanImport, error) {
	return getPlanImport(ctx, es.pool, es.namespaceID, id)
}

// ListPlanImports lists every import snapshot with the given slug, scoped
// to es's namespace.
func (es *EngineStore) ListPlanImports(ctx context.Context, slug string) ([]PlanImport, error) {
	return listPlanImports(ctx, es.pool, es.namespaceID, slug)
}

// importPlan is the shared implementation Store.ImportPlan and
// EngineStore.ImportPlan both call: insert the plan_imports row plus every
// task and deviation row inside one transaction, so a caller never sees a
// plan_imports row with a partial task/deviation set, and a failure at any
// point leaves nothing behind (migrations/0024_plan_imports.sql's "every
// import is its own row, inserted once" convention starts here).
//
// It is NOT idempotent on (namespace, slug): re-importing the same slug
// inserts a new plan_imports row (see the migration file's header for why
// this table has no supersedes chain). A caller that wants "did this exact
// content already get imported" compares SourceDigest against
// ListPlanImports' results itself; importPlan does not guess at that.
func importPlan(ctx context.Context, pool *pgxpool.Pool, in ImportPlanInput) (PlanImport, error) {
	switch {
	case in.NamespaceID == "":
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: namespaceID is required")
	case in.Slug == "":
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: slug is required")
	case in.SourceDigest == "":
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: sourceDigest is required")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	planImportID := store.NewULID()
	_, err = tx.Exec(ctx, `
		INSERT INTO plan_imports (id, namespace_id, slug, title, source_slug, source_status, source_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		planImportID, in.NamespaceID, in.Slug, in.Title, in.SourceSlug, in.SourceStatus, in.SourceDigest)
	if err != nil {
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: insert plan_imports: %w", err)
	}

	tasks := make([]PlanImportTask, 0, len(in.Tasks))
	for _, taskIn := range in.Tasks {
		id := store.NewULID()
		dependsOn, err := json.Marshal(nonNilStrings(taskIn.DependsOn))
		if err != nil {
			return PlanImport{}, fmt.Errorf("postgres: ImportPlan: encode depends_on of %s: %w", taskIn.TaskRef, err)
		}
		acceptance, err := json.Marshal(nonNilStrings(taskIn.AcceptanceCriteria))
		if err != nil {
			return PlanImport{}, fmt.Errorf("postgres: ImportPlan: encode acceptance_criteria of %s: %w", taskIn.TaskRef, err)
		}
		covers, err := json.Marshal(nonNilStrings(taskIn.Covers))
		if err != nil {
			return PlanImport{}, fmt.Errorf("postgres: ImportPlan: encode covers of %s: %w", taskIn.TaskRef, err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO plan_import_tasks (
				id, plan_import_id, namespace_id, task_ref, summary, instruction,
				origin_kind, source_status, depends_on, wave_index, acceptance_criteria, covers
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			id, planImportID, in.NamespaceID, taskIn.TaskRef, taskIn.Summary, taskIn.Instruction,
			taskIn.OriginKind, taskIn.SourceStatus, dependsOn, intOrNull(taskIn.Wave), acceptance, covers)
		if err != nil {
			if isUniqueViolation(err) {
				return PlanImport{}, fmt.Errorf("postgres: ImportPlan: task_ref %s appears more than once in this plan: %w", taskIn.TaskRef, err)
			}
			return PlanImport{}, fmt.Errorf("postgres: ImportPlan: insert task %s: %w", taskIn.TaskRef, err)
		}
		tasks = append(tasks, PlanImportTask{ID: id, PlanImportTaskInput: taskIn})
	}

	deviations := make([]PlanImportDeviation, 0, len(in.Deviations))
	for _, devIn := range in.Deviations {
		id := store.NewULID()
		affects, err := json.Marshal(nonNilStrings(devIn.Affects))
		if err != nil {
			return PlanImport{}, fmt.Errorf("postgres: ImportPlan: encode affects of %s: %w", devIn.DeviationRef, err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO plan_import_deviations (
				id, plan_import_id, namespace_id, deviation_ref, what, task_ref,
				reason, affects, origin_kind, source_status, classification
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			id, planImportID, in.NamespaceID, devIn.DeviationRef, devIn.What, devIn.TaskRef,
			devIn.Reason, affects, devIn.OriginKind, devIn.SourceStatus, textOrNull(devIn.Classification))
		if err != nil {
			if isUniqueViolation(err) {
				return PlanImport{}, fmt.Errorf("postgres: ImportPlan: deviation_ref %s appears more than once in this plan: %w", devIn.DeviationRef, err)
			}
			return PlanImport{}, fmt.Errorf("postgres: ImportPlan: insert deviation %s: %w", devIn.DeviationRef, err)
		}
		deviations = append(deviations, PlanImportDeviation{ID: id, PlanImportDeviationInput: devIn})
	}

	var importedAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `SELECT imported_at FROM plan_imports WHERE id = $1`, planImportID).Scan(&importedAt); err != nil {
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: read back imported_at: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return PlanImport{}, fmt.Errorf("postgres: ImportPlan: commit transaction: %w", err)
	}

	return PlanImport{
		ID:           planImportID,
		NamespaceID:  in.NamespaceID,
		Slug:         in.Slug,
		Title:        in.Title,
		SourceSlug:   in.SourceSlug,
		SourceStatus: in.SourceStatus,
		SourceDigest: in.SourceDigest,
		ImportedAt:   tsValue(importedAt),
		Tasks:        tasks,
		Deviations:   deviations,
	}, nil
}

// getPlanImport is the shared implementation Store.GetPlanImport and
// EngineStore.GetPlanImport both call.
func getPlanImport(ctx context.Context, pool *pgxpool.Pool, namespaceID, id string) (PlanImport, error) {
	if namespaceID == "" {
		return PlanImport{}, fmt.Errorf("postgres: GetPlanImport: namespaceID is required")
	}
	if id == "" {
		return PlanImport{}, fmt.Errorf("postgres: GetPlanImport: id is required")
	}

	var (
		pi         PlanImport
		importedAt pgtype.Timestamptz
	)
	err := pool.QueryRow(ctx, `
		SELECT id, namespace_id, slug, title, source_slug, source_status, source_digest, imported_at
		FROM plan_imports WHERE namespace_id = $1 AND id = $2`,
		namespaceID, id,
	).Scan(&pi.ID, &pi.NamespaceID, &pi.Slug, &pi.Title, &pi.SourceSlug, &pi.SourceStatus, &pi.SourceDigest, &importedAt)
	if err != nil {
		if isNoRows(err) {
			return PlanImport{}, fmt.Errorf("postgres: plan import %s: %w", id, ErrNotFound)
		}
		return PlanImport{}, fmt.Errorf("postgres: GetPlanImport %s: %w", id, err)
	}
	pi.ImportedAt = tsValue(importedAt)

	tasks, err := planImportTasks(ctx, pool, namespaceID, id)
	if err != nil {
		return PlanImport{}, fmt.Errorf("postgres: GetPlanImport %s: %w", id, err)
	}
	pi.Tasks = tasks

	deviations, err := planImportDeviations(ctx, pool, namespaceID, id)
	if err != nil {
		return PlanImport{}, fmt.Errorf("postgres: GetPlanImport %s: %w", id, err)
	}
	pi.Deviations = deviations

	return pi, nil
}

func planImportTasks(ctx context.Context, pool *pgxpool.Pool, namespaceID, planImportID string) ([]PlanImportTask, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, task_ref, summary, instruction, origin_kind, source_status,
			depends_on, wave_index, acceptance_criteria, covers
		FROM plan_import_tasks
		WHERE namespace_id = $1 AND plan_import_id = $2
		ORDER BY task_ref`, namespaceID, planImportID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	out := make([]PlanImportTask, 0)
	for rows.Next() {
		var (
			t          PlanImportTask
			dependsOn  []byte
			wave       pgtype.Int4
			acceptance []byte
			covers     []byte
		)
		if err := rows.Scan(&t.ID, &t.TaskRef, &t.Summary, &t.Instruction, &t.OriginKind, &t.SourceStatus,
			&dependsOn, &wave, &acceptance, &covers); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		if err := json.Unmarshal(dependsOn, &t.DependsOn); err != nil {
			return nil, fmt.Errorf("decode depends_on of %s: %w", t.TaskRef, err)
		}
		if err := json.Unmarshal(acceptance, &t.AcceptanceCriteria); err != nil {
			return nil, fmt.Errorf("decode acceptance_criteria of %s: %w", t.TaskRef, err)
		}
		if err := json.Unmarshal(covers, &t.Covers); err != nil {
			return nil, fmt.Errorf("decode covers of %s: %w", t.TaskRef, err)
		}
		if wave.Valid {
			w := int(wave.Int32)
			t.Wave = &w
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	return out, nil
}

func planImportDeviations(ctx context.Context, pool *pgxpool.Pool, namespaceID, planImportID string) ([]PlanImportDeviation, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, deviation_ref, what, task_ref, reason, affects, origin_kind, source_status, classification
		FROM plan_import_deviations
		WHERE namespace_id = $1 AND plan_import_id = $2
		ORDER BY deviation_ref`, namespaceID, planImportID)
	if err != nil {
		return nil, fmt.Errorf("list deviations: %w", err)
	}
	defer rows.Close()

	out := make([]PlanImportDeviation, 0)
	for rows.Next() {
		var (
			d              PlanImportDeviation
			affects        []byte
			classification pgtype.Text
		)
		if err := rows.Scan(&d.ID, &d.DeviationRef, &d.What, &d.TaskRef, &d.Reason, &affects,
			&d.OriginKind, &d.SourceStatus, &classification); err != nil {
			return nil, fmt.Errorf("scan deviation: %w", err)
		}
		if err := json.Unmarshal(affects, &d.Affects); err != nil {
			return nil, fmt.Errorf("decode affects of %s: %w", d.DeviationRef, err)
		}
		d.Classification = textOrEmpty(classification)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read deviations: %w", err)
	}
	return out, nil
}

// listPlanImports is the shared implementation Store.ListPlanImports and
// EngineStore.ListPlanImports both call: every plan import snapshot with
// the given slug, most recent first -- how a caller finds "the current
// one" without this package guessing at a supersedes relationship the
// schema deliberately does not model (migrations/0024_plan_imports.sql).
// Tasks/deviations are not populated (use GetPlanImport for those); this is
// the lightweight listing surface.
func listPlanImports(ctx context.Context, pool *pgxpool.Pool, namespaceID, slug string) ([]PlanImport, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("postgres: ListPlanImports: namespaceID is required")
	}
	if slug == "" {
		return nil, fmt.Errorf("postgres: ListPlanImports: slug is required")
	}
	rows, err := pool.Query(ctx, `
		SELECT id, namespace_id, slug, title, source_slug, source_status, source_digest, imported_at
		FROM plan_imports WHERE namespace_id = $1 AND slug = $2 ORDER BY imported_at DESC`,
		namespaceID, slug)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListPlanImports: %w", err)
	}
	defer rows.Close()

	out := make([]PlanImport, 0)
	for rows.Next() {
		var pi PlanImport
		var importedAt pgtype.Timestamptz
		if err := rows.Scan(&pi.ID, &pi.NamespaceID, &pi.Slug, &pi.Title, &pi.SourceSlug,
			&pi.SourceStatus, &pi.SourceDigest, &importedAt); err != nil {
			return nil, fmt.Errorf("postgres: ListPlanImports: scan: %w", err)
		}
		pi.ImportedAt = tsValue(importedAt)
		out = append(out, pi)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListPlanImports: %w", err)
	}
	return out, nil
}

// nonNilStrings keeps a nil slice from encoding as JSON null into a column
// that means "no entries", which is a list, not an absence (ledger_store.go's
// nonNilRefs draws the identical distinction for provenance_refs).
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// intOrNull renders a *int as the nullable column value: nil stays NULL.
func intOrNull(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}
