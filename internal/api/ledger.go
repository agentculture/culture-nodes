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
//
// format=markdown renders the same, freshly computed projection through
// internal/ledger's Projection.Markdown instead of the default JSON body
// (PRD §10.9, docs/acceptance.md criterion 9). It is a second rendering of
// the identical read, never a cached or separately maintained copy — there
// is no path through this handler that produces Markdown without first
// computing the JSON projection it reflects.
func (s *Server) handleGetLedgerProjection(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	name := r.PathValue("name")
	subject := r.URL.Query().Get("subject")
	format := r.URL.Query().Get("format")
	ctx := r.Context()

	if format != "" && format != formatJSON && format != formatMarkdown {
		return badRequest("use format=json (the default) or format=markdown",
			"unknown format %q", format)
	}

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

	if format == formatMarkdown {
		md, err := projection.Markdown()
		if err != nil {
			return internalError(err)
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(md))
		return nil
	}

	writeJSON(w, http.StatusOK, projection)
	return nil
}

// The two format values handleGetLedgerProjection accepts. JSON is the
// default whether or not a caller states it, matching this endpoint's
// behavior before format existed at all.
const (
	formatJSON     = "json"
	formatMarkdown = "markdown"
)

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
