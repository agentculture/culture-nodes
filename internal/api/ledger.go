package api

import (
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// handleListLedgerRecords is GET /v1alpha1/runs/{id}/ledger: the PRD §8.6
// Ledger view's raw feed, every record (live and superseded) in append
// order, plus the run's current ledger version.
func (s *Server) handleListLedgerRecords(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	records, err := s.Ledger.Records(ctx, id)
	if err != nil {
		return internalError(err)
	}
	if records == nil {
		records = []ledger.Record{}
	}
	version, err := s.Ledger.LedgerVersion(ctx, id)
	if err != nil {
		return internalError(err)
	}

	writeJSON(w, http.StatusOK, LedgerRecordsOut{Items: records, LedgerVersion: version})
	return nil
}

// handleGetLedgerProjection is GET /v1alpha1/runs/{id}/ledger/projections/{name}.
func (s *Server) handleGetLedgerProjection(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	name := r.PathValue("name")
	subject := r.URL.Query().Get("subject")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	kind := ledger.ProjectionKind(name)
	if !isKnownProjection(kind) {
		return badRequest("use one of: "+strings.Join(projectionNames(), ", "), "unknown projection %q", name)
	}

	projection, err := s.Ledger.ProjectRun(ctx, id, kind, subject)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, projection)
	return nil
}

func isKnownProjection(kind ledger.ProjectionKind) bool {
	for _, k := range ledger.ProjectionKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func projectionNames() []string {
	kinds := ledger.ProjectionKinds()
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	return names
}
