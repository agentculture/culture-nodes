package api

import "net/http"

// GET /v1alpha1/declaration-engine/open-runs (task t17, #328, spec
// c94/h63): "the graph engine retires only when its open-run count is
// zero, and that count is exposed via the API". This route is that
// exposure -- read-only, namespace-scoped like every other route this
// single-namespace server serves, and unauthenticated like the rest of
// this phase-1 API (PRD spec decision c45).
//
// It reports a fact, not a decision: this task does not make the graph
// engine retire itself at zero, it only makes the count real enough that
// something else (an operator, or a later task) can act on it.

// OpenGraphRunCountOut is this route's payload.
type OpenGraphRunCountOut struct {
	// NamespaceID is the namespace the count was read for -- this server
	// only ever serves one, but the field is named so a client reading
	// this payload alone never has to assume which.
	NamespaceID string `json:"namespace_id"`
	// OpenRuns is the number of graph runs whose state is not yet
	// terminal (not completed, failed, or cancelled) -- created, running,
	// and waiting all count.
	OpenRuns int `json:"open_runs"`
}

func (s *Server) handleGetOpenGraphRunCount(w http.ResponseWriter, r *http.Request) error {
	if s.openRunCounter == nil {
		return unavailable("this deployment has no open-run counter configured", "open-run counter unavailable")
	}
	n, err := s.openRunCounter.OpenGraphRunCount(r.Context(), s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, OpenGraphRunCountOut{NamespaceID: s.NamespaceID, OpenRuns: n})
	return nil
}
