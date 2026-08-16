package api

import (
	"net/http"
	"time"
)

type NamespaceOut struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// handleListNamespaces lists the installation's namespace rows. Unlike most
// operations this is intentionally installation-wide: its purpose is to let
// workers discover the namespace id before they can bind themselves to one.
func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.Store.Pool().Query(r.Context(), `
		SELECT id, slug, display_name, created_at
		FROM namespaces
		ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := make([]NamespaceOut, 0)
	for rows.Next() {
		var ns NamespaceOut
		if err := rows.Scan(&ns.ID, &ns.Slug, &ns.DisplayName, &ns.CreatedAt); err != nil {
			return err
		}
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
