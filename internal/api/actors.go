package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
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
	// Availability is the capacity circuit breaker's state for this actor
	// KEY (task t9, migration 0020) — absent when this actor has never been
	// paused. It is deliberately keyed by actor_key rather than by this
	// row's id, so every revision of one identity reports the same
	// availability: provider capacity belongs to the identity, not to one
	// append-only registration revision.
	Availability *ActorAvailabilityOut `json:"availability,omitempty"`
	// DispatchRate is the pacing control's state for this actor KEY (task
	// t10, migration 0022) — absent when no declared rate has ever admitted a
	// dispatch to it. Keyed by actor_key for the same reason Availability is:
	// a rate belongs to the identity, not to one registration revision. The
	// global rate is not rendered here (it is not this actor's); it is on
	// GET /v1alpha1/dispatch-rates alongside every per-actor scope.
	DispatchRate *DispatchRateOut `json:"dispatch_rate,omitempty"`
}

// ActorAvailabilityOut is one actor_availability row rendered for the
// actors read surface (task t9, honesty condition h38: "visible with reason
// and until-when, and clearable without touching the database").
//
// Paused is the answer to the only question a caller usually has, computed
// server-side against the database's clock rather than left to a client to
// derive from PausedUntil and its own — the same reason
// postgres.Store.ActivePause compares in SQL. The row is still rendered when
// Paused is false: an actor whose pause lapsed or was cleared should say so,
// which is a different fact from an actor that was never paused (that one
// carries no availability block at all).
type ActorAvailabilityOut struct {
	Paused      bool      `json:"paused"`
	PausedUntil time.Time `json:"paused_until"`
	// Reason is the §13.5 error class that tripped the breaker
	// ("capacity_exhausted").
	Reason string `json:"reason"`
	// RetryAfterSeconds is the provider's own Retry-After hint, omitted when
	// it named none — never rendered as 0, which would read as "retry
	// immediately".
	RetryAfterSeconds *int32    `json:"retry_after_seconds,omitempty"`
	Detail            string    `json:"detail,omitempty"`
	TrippedAt         time.Time `json:"tripped_at"`
	// The dispatch that discovered the exhaustion, so "why is this paused"
	// is answerable without correlating timestamps against the event log.
	TrippedByRunID     string `json:"tripped_by_run_id,omitempty"`
	TrippedByNodeRunID string `json:"tripped_by_node_run_id,omitempty"`
	TrippedByAttemptID string `json:"tripped_by_attempt_id,omitempty"`
	// ClearedAt/ClearedBy are present only when an operator ended the pause
	// early, which is what keeps "it expired" and "a human let it back in"
	// distinguishable after the fact.
	ClearedAt *time.Time `json:"cleared_at,omitempty"`
	ClearedBy string     `json:"cleared_by,omitempty"`
}

func actorAvailabilityOut(pause postgres.ActorPause, now time.Time) *ActorAvailabilityOut {
	return &ActorAvailabilityOut{
		Paused:             pause.Paused(now),
		PausedUntil:        pause.PausedUntil,
		Reason:             pause.Reason,
		RetryAfterSeconds:  pause.RetryAfterSeconds,
		Detail:             pause.Detail,
		TrippedAt:          pause.TrippedAt,
		TrippedByRunID:     pause.TrippedByRunID,
		TrippedByNodeRunID: pause.TrippedByNodeRunID,
		TrippedByAttemptID: pause.TrippedByAttemptID,
		ClearedAt:          pause.ClearedAt,
		ClearedBy:          pause.ClearedBy,
	}
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

// actorOutWithAvailability is actorOut plus the breaker state for this
// actor's key, when there is one.
func actorOutWithAvailability(a postgres.Actor, pause postgres.ActorPause, ok bool, now time.Time) ActorOut {
	out := actorOut(a)
	if ok {
		out.Availability = actorAvailabilityOut(pause, now)
	}
	return out
}

// withDispatchRate adds the pacing state recorded for this actor's key, when
// there is any. Both per-key blocks -- the breaker's and the rate's -- are
// attached the same way and resolved the same way, by actor_key rather than
// by row id.
func withDispatchRate(out ActorOut, rate postgres.DispatchRateState, ok bool, now time.Time) ActorOut {
	if ok {
		rendered := dispatchRateOut(rate, now)
		out.DispatchRate = &rendered
	}
	return out
}

// actorRatesByKey indexes the namespace's actor-scoped pacing rows by actor
// key. The global scope is deliberately dropped here: it is not any one
// actor's rate, and rendering it on every actor would read as though each of
// them had its own copy of it.
func actorRatesByKey(states []postgres.DispatchRateState) map[string]postgres.DispatchRateState {
	byKey := make(map[string]postgres.DispatchRateState, len(states))
	for _, state := range states {
		if state.Scope == postgres.RateScopeActor {
			byKey[state.ScopeKey] = state
		}
	}
	return byKey
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
	ctx := r.Context()
	actors, err := s.engineStore.ListActors(ctx)
	if err != nil {
		return internalError(err)
	}
	// One query for every availability row rather than one per actor: the
	// list renders every revision of every key, and an actor_key's pause is
	// the same row for all of them (task t9).
	pauses, err := s.engineStore.ActorPauses(ctx)
	if err != nil {
		return internalError(err)
	}
	// The same one-query-for-all argument, for the pacing rows (task t10).
	rates, err := s.engineStore.DispatchRates(ctx)
	if err != nil {
		return internalError(err)
	}
	ratesByKey := actorRatesByKey(rates)
	now := time.Now().UTC()
	out := make([]ActorOut, len(actors))
	for i, a := range actors {
		pause, ok := pauses[a.ActorKey]
		rate, rated := ratesByKey[a.ActorKey]
		out[i] = withDispatchRate(actorOutWithAvailability(a, pause, ok, now), rate, rated, now)
	}
	writeJSON(w, http.StatusOK, ActorListOut{Items: out})
	return nil
}

// handleGetActor is GET /v1alpha1/actors/{id}.
func (s *Server) handleGetActor(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	a, err := s.engineStore.GetActor(ctx, r.PathValue("id"))
	if err != nil {
		return classify(err)
	}
	pause, ok, err := s.engineStore.ActorPauseFor(ctx, a.ActorKey)
	if err != nil {
		return internalError(err)
	}
	rates, err := s.engineStore.DispatchRates(ctx)
	if err != nil {
		return internalError(err)
	}
	rate, rated := actorRatesByKey(rates)[a.ActorKey]
	now := time.Now().UTC()
	writeJSON(w, http.StatusOK, withDispatchRate(actorOutWithAvailability(a, pause, ok, now), rate, rated, now))
	return nil
}

// resumeActorRequest is components.schemas.ResumeActorRequest (task t9):
// who is clearing the pause. It is optional — an operator at a shell should
// not be blocked from unblocking their fleet over a provenance field — and
// defaults to a value that says exactly that rather than naming nobody.
type resumeActorRequest struct {
	ClearedBy string `json:"cleared_by"`
}

// defaultClearedBy is what an unattributed clear records. It is a real
// statement ("an operator through this API"), not an empty string, so the
// cleared_by column never has to be read as "unknown, possibly automatic".
const defaultClearedBy = "operator"

// handleResumeActor is POST /v1alpha1/actors/{id}/resume (task t9, honesty
// condition h38): end a capacity pause early, without touching the database.
//
// It is the reversibility half of the circuit breaker. The breaker's whole
// job is to refuse dispatch to an actor an automated classification believes
// is exhausted; an operator who knows better — the quota reset early, the
// bridge was misreporting, the limit was per-project and this run is not on
// it — must be able to overrule it through the same surface they read it
// from. Clearing an actor that is not paused is 200 with the current state,
// not an error: the operator's intent ("this actor should be dispatchable")
// is already satisfied, and two operators racing to clear one pause should
// both succeed.
//
// Like actor registration on this same noun — and unlike the rest of this
// authless-by-phase-1-design API (spec decision c45) — it requires the
// actor-registration bearer token, and deliberately the SAME secret rather
// than a new one: registration is what grants an endpoint the standing to be
// dispatched real work, and clearing a pause is restoring exactly that
// standing.
func (s *Server) handleResumeActor(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireActorRegistrationAuth(r); err != nil {
		return err
	}

	ctx := r.Context()
	a, err := s.engineStore.GetActor(ctx, r.PathValue("id"))
	if err != nil {
		return classify(err)
	}

	// The body is optional in full: no body, an empty body, and `{}` all
	// mean "an operator did this and did not say who".
	req := resumeActorRequest{}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			return badRequest(
				"send no body, or a JSON body matching ResumeActorRequest: {cleared_by?}",
				"decode request body: %v", err)
		}
	}
	clearedBy := strings.TrimSpace(req.ClearedBy)
	if clearedBy == "" {
		clearedBy = defaultClearedBy
	}

	if _, _, err := s.engineStore.ClearActorPause(ctx, a.ActorKey, clearedBy); err != nil {
		return internalError(err)
	}

	// Re-read rather than render the clear's own return value: an actor with
	// no pause at all has nothing to return from the clear, and this way one
	// response shape covers both cases.
	pause, ok, err := s.engineStore.ActorPauseFor(ctx, a.ActorKey)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, actorOutWithAvailability(a, pause, ok, time.Now().UTC()))
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
