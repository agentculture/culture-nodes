package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Actor is one actors table row (migrations/0001_namespaces_and_identity.sql).
// Actor identity is append-only -- a new capability or endpoint change is a
// new row (a new Revision), never an update to an existing one -- so this
// type, like WorkflowVersion, names exactly one immutable row rather than a
// mutable "current state of an actor key" projection. Capabilities and
// Metadata are rendered verbatim: neither column ever holds a credential --
// deploy/prod/register-actor.sh's own doc comment states metadata carries
// only the NAME of the environment variable a worker reads a token from
// (metadata.auth_token_env), never the token value itself -- so there is
// nothing to redact before exposing either column over the actors read
// surface (task t15).
type Actor struct {
	ID           string
	NamespaceID  string
	ActorKey     string
	Revision     int32
	Kind         string
	Protocol     string
	EndpointRef  string
	Capabilities json.RawMessage
	Metadata     json.RawMessage
	CreatedAt    time.Time
}

const actorColumns = `id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, capabilities, metadata, created_at`

func scanActor(row interface{ Scan(dest ...any) error }) (Actor, error) {
	var (
		a            Actor
		endpointRef  pgtype.Text
		capabilities []byte
		metadata     []byte
		createdAt    pgtype.Timestamptz
	)
	if err := row.Scan(
		&a.ID, &a.NamespaceID, &a.ActorKey, &a.Revision, &a.Kind, &a.Protocol,
		&endpointRef, &capabilities, &metadata, &createdAt,
	); err != nil {
		return Actor{}, err
	}
	a.EndpointRef = textOrEmpty(endpointRef)
	a.Capabilities = capabilities
	a.Metadata = metadata
	a.CreatedAt = tsValue(createdAt)
	return a, nil
}

// ListActors returns every registered actor row in this namespace, ordered
// by actor_key then revision. Every revision of an actor_key is a distinct,
// independently addressable row (see Actor's doc comment), so this lists
// every row rather than collapsing to "the latest revision per key" -- a
// projection this read surface does not need yet and would otherwise have
// to invent unasked. GET /v1alpha1/actors/{id} then fetches exactly one of
// these rows by its own id.
func (eq engineQueries) ListActors(ctx context.Context) ([]Actor, error) {
	rows, err := eq.q.Query(ctx,
		`SELECT `+actorColumns+` FROM actors WHERE namespace_id = $1 ORDER BY actor_key, revision`,
		eq.namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ListActors: %w", err)
	}
	defer rows.Close()

	out := make([]Actor, 0)
	for rows.Next() {
		a, err := scanActor(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: engine: ListActors: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetActor returns one actor row by id, or ErrNotFound.
func (eq engineQueries) GetActor(ctx context.Context, id string) (Actor, error) {
	row := eq.q.QueryRow(ctx,
		`SELECT `+actorColumns+` FROM actors WHERE namespace_id = $1 AND id = $2`,
		eq.namespaceID, id)
	a, err := scanActor(row)
	if err != nil {
		if isNoRows(err) {
			return Actor{}, ErrNotFound
		}
		return Actor{}, fmt.Errorf("postgres: engine: GetActor %s: %w", id, err)
	}
	return a, nil
}

// ActorNodeRunOutcome is one (status, outcome) bucket of node-run
// participation counts for an actor. Status is node_runs.status (the
// engine's technical lifecycle state -- completed/failed/cancelled/...);
// Outcome is node_runs.outcome (the PRD's domain outcome, e.g.
// "changes_required", set only on a succeeded transition -- see
// internal/engine/complete.go). The two are reported side by side, never
// collapsed into one field, so this stays honest to this repo's own ground
// rule that a domain outcome is not a technical status. Outcome is "" when
// the node run has none recorded yet (in progress, failed, cancelled, or
// contract-rejected) -- a real, structural value, not an omission.
type ActorNodeRunOutcome struct {
	Status  string
	Outcome string
	Count   int
}

// ActorAuthorityCount is one ledger authority bucket (proposed/confirmed/
// observed/derived/rejected) of records this actor originated.
type ActorAuthorityCount struct {
	Authority string
	Count     int
}

// ActorRetryBurn is task t15's "attempts per completion" measure: how many
// dispatch attempts this actor made in scope, against how many of the node
// runs it attempted actually reached 'completed'. The ratio itself is left
// to the caller (internal/api) to compute as Attempts/CompletedNodeRuns,
// null when CompletedNodeRuns is 0 -- a division this type deliberately
// does not perform, so a zero denominator can never silently become a
// fabricated zero or an inf/NaN leaking into JSON.
type ActorRetryBurn struct {
	Attempts          int
	CompletedNodeRuns int
}

// ActorDurationPercentiles is computed only over attempts that actually
// completed (completed_at IS NOT NULL) -- an in-flight attempt has no
// duration to report. A nil *ActorDurationPercentiles (see
// ActorCategoryStats.DurationPercentiles) means no attempt in that scope
// has completed yet, distinct from Count == 0 inside a present-but-empty
// struct, which never happens: this type is only ever constructed from a
// query row that already required COUNT(*) > 0 to exist at all.
type ActorDurationPercentiles struct {
	P50Seconds float64
	P90Seconds float64
	P99Seconds float64
	Count      int
}

// ActorGradeAgg is one grade.schema.json authority bucket (proposed or
// confirmed -- see ActorGrades) evaluating an actor: how many grade records
// and their mean rating. MeanRating is nil when Count == 0 -- an average of
// zero opinions is not zero, it is undefined, and this type says so
// structurally rather than rendering a misleading 0.
type ActorGradeAgg struct {
	Count      int
	MeanRating *float64
}

// ActorGrades separates proposed grades (agent-origin opinions, §10.4) from
// confirmed grades (a human's own direct opinion -- see
// internal/ledger/authority.go's checkHumanAuthority) -- task t15's
// acceptance criterion that the two are "reported as separate numbers,
// never blended". A grade record's Authority is never observed, derived, or
// (on the record itself) rejected -- see internal/ledger/authority.go's
// RuleGradeNeverObservedOrDerived and the review-transaction discussion in
// checkHumanAuthority -- so these two buckets are exhaustive for
// record_type=grade.
type ActorGrades struct {
	Proposed  ActorGradeAgg
	Confirmed ActorGradeAgg
}

// ActorCategoryStats is one per-actor stats slice: either one run category
// (see ActorStats.Categories) or the all-categories Total. Every field is
// independently "present but empty" or "absent" per its own doc comment --
// nothing here is ever a fabricated zero standing in for missing data (task
// t15 acceptance criterion 3).
type ActorCategoryStats struct {
	RunsByOutcome       []ActorNodeRunOutcome
	ClaimsByAuthority   []ActorAuthorityCount
	RetryBurn           ActorRetryBurn
	DurationPercentiles *ActorDurationPercentiles
	Usage               UsageRollup
	Grades              ActorGrades
}

// sorted returns s with every slice field in a deterministic order --
// GROUPING SETS makes no row-order guarantee, and a caller (tests, a stable
// API response) should not have to depend on incidental physical order.
func (s ActorCategoryStats) sorted() ActorCategoryStats {
	sort.Slice(s.RunsByOutcome, func(i, j int) bool {
		if s.RunsByOutcome[i].Status != s.RunsByOutcome[j].Status {
			return s.RunsByOutcome[i].Status < s.RunsByOutcome[j].Status
		}
		return s.RunsByOutcome[i].Outcome < s.RunsByOutcome[j].Outcome
	})
	sort.Slice(s.ClaimsByAuthority, func(i, j int) bool {
		return s.ClaimsByAuthority[i].Authority < s.ClaimsByAuthority[j].Authority
	})
	return s
}

// ActorStats is task t15's per-actor aggregate: Total across every run
// category this actor has touched, plus a Categories breakdown keyed by
// runs.category ("" is the uncategorized bucket -- runs with no category
// tag, or a ledger/grade record with no run at all -- and is a real,
// present key in this map, never an omitted one). Total is never the sum
// of Categories' numbers recomputed client-side: it is its own query scope
// (the GROUPING SETS "()" total row), which is the only way
// DurationPercentiles' percentiles stay mathematically correct -- a
// percentile is not additive across subgroups.
type ActorStats struct {
	ActorID    string
	Total      ActorCategoryStats
	Categories map[string]ActorCategoryStats
}

// bucket returns the ActorCategoryStats a grouped row belongs in: the Total
// bucket when category is SQL NULL (the GROUPING SETS "()" row identified
// by `GROUPING(...) = 1`), or the named (possibly "") category bucket
// otherwise, creating it on first touch.
func (s *ActorStats) bucket(category pgtype.Text) *actorCategoryStatsBuilder {
	if !category.Valid {
		return &actorCategoryStatsBuilder{stats: &s.Total}
	}
	key := category.String
	cur := s.Categories[key]
	return &actorCategoryStatsBuilder{stats: &cur, commit: func() { s.Categories[key] = cur }}
}

// actorCategoryStatsBuilder lets each aggregate-loading helper below mutate
// one ActorCategoryStats bucket uniformly whether that bucket is the Total
// (a field of *ActorStats, mutable in place) or a map entry (which Go does
// not hand out as an addressable pointer) -- commit, when set, writes the
// mutated copy back into the map after each row.
type actorCategoryStatsBuilder struct {
	stats  *ActorCategoryStats
	commit func()
}

func (b *actorCategoryStatsBuilder) apply(fn func(*ActorCategoryStats)) {
	fn(b.stats)
	if b.commit != nil {
		b.commit()
	}
}

// actorStatsCategoryTotalGroupingSQL is the GROUPING SETS clause shared by
// every single-dimension (attempts count, completed-node-run count, usage
// totals) actor-stats query below: one row per distinct run category this
// actor has data in, plus one grand-total row (the "()" set) computed over
// the actor's entire scope regardless of category -- not a client-side sum
// of the per-category rows, which would be correct for a simple count/sum
// but wrong the moment a duration percentile needs the same shape.
const actorStatsCategoryGroupingSQL = `GROUP BY GROUPING SETS ((COALESCE(r.category, '')), ())`

// ActorStats computes task t15's per-actor aggregate for actorID, scoped to
// this store's namespace: node-run participation by (status, outcome),
// ledger records this actor originated by authority, attempts-per-
// completion ("retry burn"), attempt duration percentiles, §13.2 usage/cost
// (task t2's rollup, reused verbatim -- see UsageRollup), and grade.schema.json
// aggregates evaluating this actor (task t14) -- every one of it sliced by
// runs.category (task t3), with "" as the uncategorized bucket and Total as
// the all-categories row. It does not verify actorID actually names a
// registered actor; GetActor above is the existence check, and an unknown
// (or genuinely idle) actor id simply yields an ActorStats whose Total and
// Categories are all present-but-empty -- consistent with how a freshly
// created run's Usage renders (see RunOut's doc comment in internal/api).
func (eq engineQueries) ActorStats(ctx context.Context, actorID string) (ActorStats, error) {
	stats := ActorStats{ActorID: actorID, Categories: make(map[string]ActorCategoryStats)}

	loaders := []func(context.Context, string, *ActorStats) error{
		eq.loadActorRunsByOutcome,
		eq.loadActorClaimsByAuthority,
		eq.loadActorRetryBurnAttempts,
		eq.loadActorRetryBurnCompletions,
		eq.loadActorDurationPercentiles,
		eq.loadActorUsageTotals,
		eq.loadActorUsageCostByCurrency,
		eq.loadActorGrades,
	}
	for _, load := range loaders {
		if err := load(ctx, actorID, &stats); err != nil {
			return ActorStats{}, err
		}
	}

	stats.Total = stats.Total.sorted()
	for k, v := range stats.Categories {
		stats.Categories[k] = v.sorted()
	}
	return stats, nil
}

// loadActorRunsByOutcome joins attempts (this actor) -> node_runs (status,
// outcome) -> runs (category) -- the exact join path task t15's brief
// names -- counting each participated node run once (COUNT(DISTINCT nr.id))
// even though it may have carried more than one of this actor's attempts.
func (eq engineQueries) loadActorRunsByOutcome(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			nr.status,
			COALESCE(nr.outcome, ''),
			COUNT(DISTINCT nr.id)
		FROM node_runs nr
		JOIN runs r ON r.id = nr.run_id
		WHERE nr.namespace_id = $1
		  AND EXISTS (SELECT 1 FROM attempts a WHERE a.node_run_id = nr.id AND a.actor_id = $2)
		GROUP BY GROUPING SETS (
			(COALESCE(r.category, ''), nr.status, COALESCE(nr.outcome, '')),
			(nr.status, COALESCE(nr.outcome, ''))
		)`,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: runs by outcome: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category pgtype.Text
		var oc ActorNodeRunOutcome
		if err := rows.Scan(&category, &oc.Status, &oc.Outcome, &oc.Count); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: runs by outcome: scan: %w", err)
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.RunsByOutcome = append(s.RunsByOutcome, oc)
		})
	}
	return rows.Err()
}

// loadActorClaimsByAuthority counts every ledger record this actor
// originated (origin_actor_id = actorID -- every record_type, not only
// record_type='claim': PRD §10.4 uses "claim" generically for anything an
// agent asserts, e.g. "an agent saying it is done creates a completion
// claim, not verified evidence"), grouped by authority and sliced by the
// category of the run it belongs to. A record with no run_id at all (the
// envelope's run_id is nullable) or whose run has no category both land in
// the "" uncategorized bucket via the LEFT JOIN + COALESCE.
func (eq engineQueries) loadActorClaimsByAuthority(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			lr.authority,
			COUNT(*)
		FROM ledger_records lr
		LEFT JOIN runs r ON r.id = lr.run_id
		WHERE lr.namespace_id = $1 AND lr.origin_actor_id = $2
		GROUP BY GROUPING SETS (
			(COALESCE(r.category, ''), lr.authority),
			(lr.authority)
		)`,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: claims by authority: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category pgtype.Text
		var ac ActorAuthorityCount
		if err := rows.Scan(&category, &ac.Authority, &ac.Count); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: claims by authority: scan: %w", err)
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.ClaimsByAuthority = append(s.ClaimsByAuthority, ac)
		})
	}
	return rows.Err()
}

// loadActorRetryBurnAttempts fills ActorRetryBurn.Attempts: every attempt
// this actor made, in scope, regardless of its technical outcome -- the
// same "retry burn" counting rule task t2's UsageRollup documents (a
// retried, failed attempt still counts).
func (eq engineQueries) loadActorRetryBurnAttempts(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			COUNT(*)
		FROM attempts a
		JOIN node_runs nr ON nr.id = a.node_run_id
		JOIN runs r ON r.id = nr.run_id
		WHERE a.namespace_id = $1 AND a.actor_id = $2
		`+actorStatsCategoryGroupingSQL,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: retry burn attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category pgtype.Text
		var count int
		if err := rows.Scan(&category, &count); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: retry burn attempts: scan: %w", err)
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.RetryBurn.Attempts = count
		})
	}
	return rows.Err()
}

// loadActorRetryBurnCompletions fills ActorRetryBurn.CompletedNodeRuns:
// node runs that reached status='completed' AND carry at least one attempt
// from this actor -- the denominator internal/api divides Attempts by to
// report the retry-burn ratio (never divided here; see ActorRetryBurn's
// doc comment).
func (eq engineQueries) loadActorRetryBurnCompletions(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			COUNT(DISTINCT nr.id)
		FROM node_runs nr
		JOIN runs r ON r.id = nr.run_id
		WHERE nr.namespace_id = $1 AND nr.status = 'completed'
		  AND EXISTS (SELECT 1 FROM attempts a WHERE a.node_run_id = nr.id AND a.actor_id = $2)
		`+actorStatsCategoryGroupingSQL,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: retry burn completions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category pgtype.Text
		var count int
		if err := rows.Scan(&category, &count); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: retry burn completions: scan: %w", err)
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.RetryBurn.CompletedNodeRuns = count
		})
	}
	return rows.Err()
}

// loadActorDurationPercentiles computes p50/p90/p99 (PERCENTILE_CONT, linear
// interpolation) over this actor's attempt durations
// (completed_at - started_at, in seconds), counting only attempts that have
// actually completed -- an in-flight attempt contributes no duration. A
// category with zero completed attempts in scope simply produces no row,
// leaving ActorCategoryStats.DurationPercentiles nil for it (see that
// field's doc comment).
func (eq engineQueries) loadActorDurationPercentiles(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (a.completed_at - a.started_at))),
			PERCENTILE_CONT(0.9) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (a.completed_at - a.started_at))),
			PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (a.completed_at - a.started_at))),
			COUNT(*)
		FROM attempts a
		JOIN node_runs nr ON nr.id = a.node_run_id
		JOIN runs r ON r.id = nr.run_id
		WHERE a.namespace_id = $1 AND a.actor_id = $2 AND a.completed_at IS NOT NULL
		`+actorStatsCategoryGroupingSQL,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: duration percentiles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			category      pgtype.Text
			p50, p90, p99 pgtype.Float8
			count         int
		)
		if err := rows.Scan(&category, &p50, &p90, &p99, &count); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: duration percentiles: scan: %w", err)
		}
		// The Total ("()") grouping set always returns one row even when
		// this actor has zero completed attempts anywhere -- COUNT(*)
		// still evaluates to 0 over an empty input the way any bare
		// aggregate does, but PERCENTILE_CONT has nothing to interpolate
		// and reports SQL NULL. count == 0 is exactly
		// ActorDurationPercentiles' documented "no attempt in scope has
		// completed yet" case, so this row is skipped entirely rather
		// than stored as a zero-second percentile — leaving
		// DurationPercentiles nil, per that field's doc comment, instead
		// of a fabricated 0.
		if count == 0 {
			continue
		}
		dpRow := ActorDurationPercentiles{P50Seconds: p50.Float64, P90Seconds: p90.Float64, P99Seconds: p99.Float64, Count: count}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.DurationPercentiles = &dpRow
		})
	}
	return rows.Err()
}

// loadActorUsageTotals fills the token/attempt-count half of Usage,
// reusing task t2's exact aggregation rules (retry burn included,
// unreported attempts excluded from sums -- see UsageRollup's doc comment)
// scoped to this actor's own attempts and grouped by run category.
func (eq engineQueries) loadActorUsageTotals(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			COALESCE(SUM(a.usage_input_tokens), 0),
			COALESCE(SUM(a.usage_output_tokens), 0),
			COALESCE(SUM(a.usage_cached_input_tokens), 0),
			COALESCE(SUM(a.usage_reasoning_tokens), 0),
			COUNT(*) FILTER (WHERE a.usage_input_tokens IS NOT NULL)::int,
			COUNT(*) FILTER (WHERE a.usage_input_tokens IS NULL)::int
		FROM attempts a
		JOIN node_runs nr ON nr.id = a.node_run_id
		JOIN runs r ON r.id = nr.run_id
		WHERE a.namespace_id = $1 AND a.actor_id = $2
		`+actorStatsCategoryGroupingSQL,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: usage totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category pgtype.Text
		var u UsageRollup
		if err := rows.Scan(
			&category, &u.InputTokens, &u.OutputTokens,
			&u.CachedInputTokens, &u.ReasoningTokens,
			&u.AttemptsReported, &u.AttemptsNotReported,
		); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: usage totals: scan: %w", err)
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.Usage.InputTokens = u.InputTokens
			s.Usage.OutputTokens = u.OutputTokens
			s.Usage.CachedInputTokens = u.CachedInputTokens
			s.Usage.ReasoningTokens = u.ReasoningTokens
			s.Usage.AttemptsReported = u.AttemptsReported
			s.Usage.AttemptsNotReported = u.AttemptsNotReported
		})
	}
	return rows.Err()
}

// loadActorUsageCostByCurrency fills the cost-by-currency half of Usage --
// see UsageRollup.Cost's doc comment for why this is never a single summed
// number across differing currencies.
func (eq engineQueries) loadActorUsageCostByCurrency(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			COALESCE(a.usage_currency, ''),
			SUM(a.usage_cost)
		FROM attempts a
		JOIN node_runs nr ON nr.id = a.node_run_id
		JOIN runs r ON r.id = nr.run_id
		WHERE a.namespace_id = $1 AND a.actor_id = $2 AND a.usage_cost IS NOT NULL
		GROUP BY GROUPING SETS (
			(COALESCE(r.category, ''), COALESCE(a.usage_currency, '')),
			(COALESCE(a.usage_currency, ''))
		)
		ORDER BY 2`,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: usage cost by currency: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category pgtype.Text
		var cc CurrencyCost
		if err := rows.Scan(&category, &cc.Currency, &cc.Cost); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: usage cost by currency: scan: %w", err)
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			s.Usage.Cost = append(s.Usage.Cost, cc)
		})
	}
	return rows.Err()
}

// loadActorGrades computes count + mean rating of grade.schema.json records
// evaluating this actor (data->>'evaluated_actor_id' = actorID), split into
// the proposed and confirmed buckets (see ActorGrades' doc comment for why
// those two authorities are exhaustive for record_type=grade) and sliced by
// the category of the run the grade was recorded against.
func (eq engineQueries) loadActorGrades(ctx context.Context, actorID string, stats *ActorStats) error {
	rows, err := eq.q.Query(ctx, `
		SELECT
			CASE WHEN GROUPING(COALESCE(r.category, '')) = 1 THEN NULL ELSE COALESCE(r.category, '') END,
			lr.authority,
			COUNT(*),
			AVG((lr.data->>'rating')::numeric)
		FROM ledger_records lr
		LEFT JOIN runs r ON r.id = lr.run_id
		WHERE lr.namespace_id = $1
		  AND lr.record_type = 'grade'
		  AND lr.data->>'evaluated_actor_id' = $2
		  AND lr.authority IN ('proposed', 'confirmed')
		GROUP BY GROUPING SETS (
			(COALESCE(r.category, ''), lr.authority),
			(lr.authority)
		)`,
		eq.namespaceID, actorID)
	if err != nil {
		return fmt.Errorf("postgres: engine: ActorStats: grades: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			category   pgtype.Text
			authority  string
			count      int
			meanRating pgtype.Float8
		)
		if err := rows.Scan(&category, &authority, &count, &meanRating); err != nil {
			return fmt.Errorf("postgres: engine: ActorStats: grades: scan: %w", err)
		}
		agg := ActorGradeAgg{Count: count}
		if meanRating.Valid {
			mean := meanRating.Float64
			agg.MeanRating = &mean
		}
		stats.bucket(category).apply(func(s *ActorCategoryStats) {
			switch authority {
			case "proposed":
				s.Grades.Proposed = agg
			case "confirmed":
				s.Grades.Confirmed = agg
			}
		})
	}
	return rows.Err()
}
