package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// ActorOut is one actors table row, as documented in
// components.schemas.Actor (task t15). It renders every column the row
// actually holds, including Capabilities and Metadata verbatim: neither
// ever carries a credential (postgres.Actor's doc comment) -- only the NAME
// of the environment variable a worker reads a token from -- so there is
// nothing to redact here.
type ActorOut struct {
	ID           string          `json:"id"`
	ActorKey     string          `json:"actor_key"`
	Revision     int32           `json:"revision"`
	Kind         string          `json:"kind"`
	Protocol     string          `json:"protocol"`
	EndpointRef  string          `json:"endpoint_ref,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

func actorOut(a postgres.Actor) ActorOut {
	return ActorOut{
		ID:           a.ID,
		ActorKey:     a.ActorKey,
		Revision:     a.Revision,
		Kind:         a.Kind,
		Protocol:     a.Protocol,
		EndpointRef:  a.EndpointRef,
		Capabilities: nonNullJSON(a.Capabilities),
		Metadata:     nonNullJSON(a.Metadata),
		CreatedAt:    a.CreatedAt,
	}
}

// ActorListOut is components.schemas.ActorList.
type ActorListOut struct {
	Items []ActorOut `json:"items"`
}

// handleListActors is GET /v1alpha1/actors: every registered actor row in
// this namespace (every revision -- see postgres.Actor's doc comment for
// why a revision change is a new row, not an update). Read-only; there is
// no registration endpoint here (that stays deploy/prod/register-actor.sh,
// issue #8).
func (s *Server) handleListActors(w http.ResponseWriter, r *http.Request) error {
	actors, err := s.engineStore.ListActors(r.Context())
	if err != nil {
		return internalError(err)
	}
	out := make([]ActorOut, len(actors))
	for i, a := range actors {
		out[i] = actorOut(a)
	}
	writeJSON(w, http.StatusOK, ActorListOut{Items: out})
	return nil
}

// handleGetActor is GET /v1alpha1/actors/{id}.
func (s *Server) handleGetActor(w http.ResponseWriter, r *http.Request) error {
	a, err := s.engineStore.GetActor(r.Context(), r.PathValue("id"))
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, actorOut(a))
	return nil
}

// ActorNodeRunOutcomeOut is one components.schemas.ActorCategoryStats.runs_by_outcome
// entry. Status is node_runs.status (the engine's technical lifecycle
// state); Outcome is node_runs.outcome (the PRD's domain outcome), reported
// side by side rather than conflated -- see
// postgres.ActorNodeRunOutcome's doc comment.
type ActorNodeRunOutcomeOut struct {
	Status  string `json:"status"`
	Outcome string `json:"outcome,omitempty"`
	Count   int    `json:"count"`
}

// ActorLedgerAuthorityOut is one claims_by_authority entry: how many
// ledger records this actor originated at a given authority.
type ActorLedgerAuthorityOut struct {
	Authority string `json:"authority"`
	Count     int    `json:"count"`
}

// ActorRetryBurnOut is the "attempts per completion" measure.
// AttemptsPerCompletion is nil (never a fabricated division) when
// CompletedNodeRuns is 0.
type ActorRetryBurnOut struct {
	Attempts              int      `json:"attempts"`
	CompletedNodeRuns     int      `json:"completed_node_runs"`
	AttemptsPerCompletion *float64 `json:"attempts_per_completion,omitempty"`
}

func actorRetryBurnOut(rb postgres.ActorRetryBurn) ActorRetryBurnOut {
	out := ActorRetryBurnOut{Attempts: rb.Attempts, CompletedNodeRuns: rb.CompletedNodeRuns}
	if rb.CompletedNodeRuns > 0 {
		ratio := float64(rb.Attempts) / float64(rb.CompletedNodeRuns)
		out.AttemptsPerCompletion = &ratio
	}
	return out
}

// ActorDurationPercentilesOut is present only when at least one of this
// actor's attempts, in scope, has completed_at set — see
// postgres.ActorDurationPercentiles' doc comment for why "no data" is
// structural absence (the wrapping *ActorDurationPercentilesOut is nil),
// never a fabricated 0-second percentile.
type ActorDurationPercentilesOut struct {
	P50Seconds float64 `json:"p50_seconds"`
	P90Seconds float64 `json:"p90_seconds"`
	P99Seconds float64 `json:"p99_seconds"`
	Count      int     `json:"count"`
}

func actorDurationPercentilesOut(dp *postgres.ActorDurationPercentiles) *ActorDurationPercentilesOut {
	if dp == nil {
		return nil
	}
	return &ActorDurationPercentilesOut{
		P50Seconds: dp.P50Seconds,
		P90Seconds: dp.P90Seconds,
		P99Seconds: dp.P99Seconds,
		Count:      dp.Count,
	}
}

// ActorGradeAggOut is one grades.proposed/grades.confirmed bucket.
// MeanRating is omitted (not a fabricated 0) when Count is 0 — see
// postgres.ActorGradeAgg's doc comment.
type ActorGradeAggOut struct {
	Count      int      `json:"count"`
	MeanRating *float64 `json:"mean_rating,omitempty"`
}

// ActorGradesOut separates proposed grades from confirmed grades — never
// blended into one number (task t15 acceptance criterion 2).
type ActorGradesOut struct {
	Proposed  ActorGradeAggOut `json:"proposed"`
	Confirmed ActorGradeAggOut `json:"confirmed"`
}

func actorGradesOut(g postgres.ActorGrades) ActorGradesOut {
	return ActorGradesOut{
		Proposed:  ActorGradeAggOut{Count: g.Proposed.Count, MeanRating: g.Proposed.MeanRating},
		Confirmed: ActorGradeAggOut{Count: g.Confirmed.Count, MeanRating: g.Confirmed.MeanRating},
	}
}

// ActorStatsBucketOut is one stats slice's numbers — either the
// all-categories Total or one named category (see ActorCategoryBucketOut)
// — components.schemas.ActorStatsBucket.
type ActorStatsBucketOut struct {
	RunsByOutcome       []ActorNodeRunOutcomeOut     `json:"runs_by_outcome"`
	ClaimsByAuthority   []ActorLedgerAuthorityOut    `json:"claims_by_authority"`
	RetryBurn           ActorRetryBurnOut            `json:"retry_burn"`
	DurationPercentiles *ActorDurationPercentilesOut `json:"duration_percentiles,omitempty"`
	Usage               *UsageOut                    `json:"usage,omitempty"`
	Grades              ActorGradesOut               `json:"grades"`
}

func actorStatsBucketOut(cs postgres.ActorCategoryStats) ActorStatsBucketOut {
	outcomes := make([]ActorNodeRunOutcomeOut, len(cs.RunsByOutcome))
	for i, oc := range cs.RunsByOutcome {
		outcomes[i] = ActorNodeRunOutcomeOut{Status: oc.Status, Outcome: oc.Outcome, Count: oc.Count}
	}
	if outcomes == nil {
		outcomes = []ActorNodeRunOutcomeOut{}
	}
	claims := make([]ActorLedgerAuthorityOut, len(cs.ClaimsByAuthority))
	for i, ac := range cs.ClaimsByAuthority {
		claims[i] = ActorLedgerAuthorityOut{Authority: ac.Authority, Count: ac.Count}
	}
	if claims == nil {
		claims = []ActorLedgerAuthorityOut{}
	}
	return ActorStatsBucketOut{
		RunsByOutcome:       outcomes,
		ClaimsByAuthority:   claims,
		RetryBurn:           actorRetryBurnOut(cs.RetryBurn),
		DurationPercentiles: actorDurationPercentilesOut(cs.DurationPercentiles),
		Usage:               usageOut(cs.Usage),
		Grades:              actorGradesOut(cs.Grades),
	}
}

// ActorCategoryBucketOut is one components.schemas.ActorStats.categories
// entry: ActorStatsBucketOut plus the category tag it summarizes. Category
// is always emitted, never omitted — "" is the real, explicit uncategorized
// bucket (runs.category IS NULL, or a ledger/grade record with no run at
// all), structurally distinct from the Total bucket, which carries no
// category field at all (see ActorStatsOut).
type ActorCategoryBucketOut struct {
	Category string `json:"category"`
	ActorStatsBucketOut
}

// ActorStatsOut is components.schemas.ActorStats — GET
// /v1alpha1/actors/{id}/stats' payload. Total is the all-categories
// aggregate, computed as its own query scope rather than summed
// client-side from Categories (percentiles are not additive across
// subgroups — see postgres.ActorStats' doc comment). Categories is sorted
// by category name ("" — uncategorized — sorts first) for a stable
// response.
type ActorStatsOut struct {
	ActorID    string                   `json:"actor_id"`
	Total      ActorStatsBucketOut      `json:"total"`
	Categories []ActorCategoryBucketOut `json:"categories"`
}

func actorStatsOut(stats postgres.ActorStats) ActorStatsOut {
	categories := make([]ActorCategoryBucketOut, 0, len(stats.Categories))
	for category, cs := range stats.Categories {
		categories = append(categories, ActorCategoryBucketOut{
			Category:            category,
			ActorStatsBucketOut: actorStatsBucketOut(cs),
		})
	}
	sort.Slice(categories, func(i, j int) bool { return categories[i].Category < categories[j].Category })
	return ActorStatsOut{
		ActorID:    stats.ActorID,
		Total:      actorStatsBucketOut(stats.Total),
		Categories: categories,
	}
}

// handleGetActorStats is GET /v1alpha1/actors/{id}/stats (task t15): the
// per-actor aggregate — see postgres.ActorStats' doc comment for exactly
// what is computed and how "no data" renders structurally rather than as a
// fabricated zero. 404s the same way handleGetActor does when the actor id
// does not exist in this namespace; an actor that exists but has never
// participated in anything renders a present-but-entirely-empty
// ActorStatsOut, the same "computed, and it says nothing happened yet"
// convention RunOut.Usage already uses.
func (s *Server) handleGetActorStats(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.GetActor(ctx, id); err != nil {
		return classify(err)
	}

	stats, err := s.engineStore.ActorStats(ctx, id)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, actorStatsOut(stats))
	return nil
}
