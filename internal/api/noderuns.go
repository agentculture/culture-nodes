package api

import "net/http"

// handleListNodeRuns is GET /v1alpha1/node-runs (task t11): the cross-run
// "jobs timeline" listing — every node run in this namespace, not scoped to
// one run, newest-first by updated_at. See (*Server).listNodeRunsAcrossRuns
// in queries.go for the query shape, the index it uses, and why pagination
// here is a keyset cursor rather than OFFSET.
func (s *Server) handleListNodeRuns(w http.ResponseWriter, r *http.Request) error {
	updatedSince, err := parseRFC3339(r, "updated_since")
	if err != nil {
		return err
	}
	updatedUntil, err := parseRFC3339(r, "updated_until")
	if err != nil {
		return err
	}

	var cursor *nodeRunCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		decoded, err := decodeNodeRunCursor(raw)
		if err != nil {
			return badRequest(
				"omit cursor for the first page, or pass back a previous response's next_cursor unchanged",
				"parse cursor=%q: %v", raw, err)
		}
		cursor = &decoded
	}

	items, nextCursor, err := s.listNodeRunsAcrossRuns(r.Context(), updatedSince, updatedUntil, cursor, parseLimit(r, 50, 500))
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, NodeRunListOut{Items: items, NextCursor: nextCursor})
	return nil
}
