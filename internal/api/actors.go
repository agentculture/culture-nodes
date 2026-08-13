package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
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
// why a revision change is a new row, not an update). Registration is
// handleRegisterActor below (task t13), which replaced the raw-SQL-only
// deploy/prod/register-actor.sh lane (issue #8).
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

// registerActorRequest is components.schemas.RegisterActorRequest (task
// t13). Namespace is optional: this server is bound to one namespace at
// construction, so when present it may only name that namespace — anything
// else is refused rather than silently rerouted. Capabilities and Metadata
// follow the actors table's credential rule (postgres.Actor's doc comment):
// metadata carries the NAME of the environment variable a worker reads a
// token from (metadata.auth_token_env), never a token value.
type registerActorRequest struct {
	Namespace    string          `json:"namespace"`
	ActorKey     string          `json:"actor_key"`
	Kind         string          `json:"kind"`
	Protocol     string          `json:"protocol"`
	EndpointRef  string          `json:"endpoint_ref"`
	Capabilities json.RawMessage `json:"capabilities"`
	Metadata     json.RawMessage `json:"metadata"`
}

// handleRegisterActor is POST /v1alpha1/actors (task t13): the authenticated
// registration lane that replaces the raw-SQL-only
// deploy/prod/register-actor.sh path. Registration is append-only —
// re-registering an existing actor_key INSERTs the next revision row, never
// an update to an existing one (postgres.RegisterActor carries the
// semantics; see postgres.Actor's doc comment for why actor identity, like
// ledger records, only ever appends).
//
// Like the human-task decision endpoint — and unlike the rest of this
// authless-by-phase-1-design API (PRD spec decision c45) — this one requires
// a bearer token, verified by requireActorRegistrationAuth against its own
// secret (NODES_ACTOR_REGISTRATION_TOKEN_SECRET): a registration row is what
// grants an endpoint the standing to be dispatched real work.
func (s *Server) handleRegisterActor(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireActorRegistrationAuth(r); err != nil {
		return err
	}

	var req registerActorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching RegisterActorRequest: {actor_key, kind, protocol, endpoint_ref?, capabilities?, metadata?}",
			"decode request body: %v", err)
	}
	if req.ActorKey == "" {
		return badRequest("actor_key is required", "actor_key must not be empty")
	}
	if req.Kind == "" {
		return badRequest("kind is required (e.g. agent, human, runner)", "kind must not be empty")
	}
	if req.Protocol == "" {
		return badRequest("protocol is required (e.g. http)", "protocol must not be empty")
	}
	if req.Namespace != "" && req.Namespace != s.NamespaceID {
		return badRequest(
			"omit namespace or set it to this server's own namespace id",
			"this server registers actors in namespace %q, not %q", s.NamespaceID, req.Namespace)
	}

	a, err := s.engineStore.RegisterActor(r.Context(), postgres.RegisterActorParams{
		ActorKey:     req.ActorKey,
		Kind:         req.Kind,
		Protocol:     req.Protocol,
		EndpointRef:  req.EndpointRef,
		Capabilities: req.Capabilities,
		Metadata:     req.Metadata,
	})
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, actorOut(a))
	return nil
}

// requireActorRegistrationAuth is requireDecisionAuth's pattern
// (humantasks.go) applied to actor registration, against its OWN secret
// (Server.actorRegistrationSecret, NODES_ACTOR_REGISTRATION_TOKEN_SECRET) —
// deliberately not the human-decision secret, so an operator can hand out
// registration standing without also handing out the power to decide human
// tasks. A missing secret refuses every registration (closed by default),
// and a present-but-wrong bearer token is refused 401 after a fixed-cost
// digest comparison.
func (s *Server) requireActorRegistrationAuth(r *http.Request) error {
	if len(s.actorRegistrationSecret) == 0 {
		return unauthorized(
			"configure the server with an actor registration secret (NODES_ACTOR_REGISTRATION_TOKEN_SECRET) to enable actor registration",
			"actor registration requires a configured bearer secret and none is configured")
	}

	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return unauthorized("send Authorization: Bearer <token>", "missing or malformed Authorization header")
	}

	// Compare digests, not the raw values — the same fixed-cost shape
	// requireDecisionAuth uses: ConstantTimeCompare returns early on length
	// mismatch, which would leak the secret's length; hashing both sides
	// first makes the comparison genuinely constant-time.
	presented := sha256.Sum256([]byte(header[len(prefix):]))
	expected := sha256.Sum256(s.actorRegistrationSecret)
	if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return unauthorized("the bearer token is not valid for this deployment", "authorization failed")
	}
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
