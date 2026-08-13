package api

import (
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The dispatch pacing control's read surface (task t10, migration 0022;
// issue #48 item 2, spec claim c5).
//
// t9's breaker made a decision the control plane takes on an operator's
// behalf visible and reversible, on the grounds that budget.go's objection to
// a silent skip -- "it stays ready forever and nothing anywhere records why"
// -- applies to any automatic refusal. A declared rate is the same kind of
// decision and gets the same treatment: the rate being enforced, how much of
// the current window it has consumed, and when the next dispatch may go are
// all readable, so "why is this run sitting still" is answerable from the API
// alone. (Per deferral, the run's own event stream answers it too --
// dev.culture.nodes.dispatch.paced, internal/worker/pacing.go.)
//
// It is a READ surface only, and deliberately so this wave. The breaker's
// clear endpoint exists because a pause is an inference the control plane
// made from a provider error and can be wrong; a rate is not an inference, it
// is what the operator themselves declared, and the way to change a
// declaration is to change the configuration (see cmd/nodes/worker.go's
// NODES_DISPATCH_RATE_* variables) rather than to have an API that quietly
// disagrees with the workers' own environment.

// DispatchRateOut is components.schemas.DispatchRate: one rate scope's
// declared rate and its standing in the CURRENT session window.
//
// Two of these fields are computed against now rather than read from the row,
// and it matters which: WindowStartedAt/WindowEndsAt describe the window this
// scope is in AT THE MOMENT OF THE READ, and Dispatched/Remaining are
// measured against that window. A row whose stored counter belongs to a
// window that has already ended reports zero consumption, because that is
// the truth an operator needs -- the alternative would show last window's
// spending as though it still constrained anything. UpdatedAt and
// LastDispatchAt stay raw row provenance, so the history is not lost either.
type DispatchRateOut struct {
	// Scope is "global" (the whole installation's session rate) or "actor"
	// (one actor key's own).
	Scope string `json:"scope"`
	// ScopeKey is the actor key for scope "actor", absent for "global".
	ScopeKey string `json:"scope_key,omitempty"`

	// The declared rate, as the last worker to consume a slot enforced it.
	LimitPerWindow int       `json:"limit_per_window"`
	WindowSeconds  int       `json:"window_seconds"`
	WindowAnchor   time.Time `json:"window_anchor"`

	// The current window and this scope's standing in it.
	WindowStartedAt time.Time `json:"window_started_at"`
	WindowEndsAt    time.Time `json:"window_ends_at"`
	Dispatched      int       `json:"dispatched"`
	// Remaining is how many more dispatches this scope may admit before the
	// window ends: the remaining-window capacity capped by the remaining
	// budget. It is the number that answers "can this wave still run".
	Remaining int `json:"remaining"`

	// NextDispatchAt is the pace: the earliest instant the next dispatch in
	// this scope may go. Absent when nothing has dispatched yet.
	NextDispatchAt *time.Time `json:"next_dispatch_at,omitempty"`
	LastDispatchAt *time.Time `json:"last_dispatch_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// DispatchRateListOut is components.schemas.DispatchRateList.
type DispatchRateListOut struct {
	Items []DispatchRateOut `json:"items"`
}

func dispatchRateOut(state postgres.DispatchRateState, now time.Time) DispatchRateOut {
	window := state.Config().WindowAt(now)
	return DispatchRateOut{
		Scope:           state.Scope,
		ScopeKey:        state.ScopeKey,
		LimitPerWindow:  state.Limit,
		WindowSeconds:   int(state.Window / time.Second),
		WindowAnchor:    state.Anchor,
		WindowStartedAt: window.Start,
		WindowEndsAt:    window.End,
		Dispatched:      state.ConsumedAt(now),
		Remaining:       state.Remaining(now),
		NextDispatchAt:  state.NextDispatchAt,
		LastDispatchAt:  state.LastDispatchAt,
		UpdatedAt:       state.UpdatedAt,
	}
}

// handleListDispatchRates is GET /v1alpha1/dispatch-rates: every rate scope
// with recorded state in this namespace, global first-class alongside the
// per-actor ones.
//
// A scope that has never admitted a dispatch has no row and is absent, which
// is honest rather than tidy: this surface reports what the workers have
// actually enforced, not what some environment somewhere claims to have
// configured. An empty list on an installation that believes it configured a
// rate is therefore informative -- nothing has been dispatched under it.
func (s *Server) handleListDispatchRates(w http.ResponseWriter, r *http.Request) error {
	states, err := s.engineStore.DispatchRates(r.Context())
	if err != nil {
		return internalError(err)
	}
	now := time.Now().UTC()
	out := make([]DispatchRateOut, len(states))
	for i, state := range states {
		out[i] = dispatchRateOut(state, now)
	}
	writeJSON(w, http.StatusOK, DispatchRateListOut{Items: out})
	return nil
}
