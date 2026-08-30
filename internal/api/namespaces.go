package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type NamespaceOut struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

type createNamespaceRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func namespaceOut(ns postgres.Namespace) NamespaceOut {
	return NamespaceOut{ID: ns.ID, Slug: ns.Slug, DisplayName: ns.DisplayName, CreatedAt: ns.CreatedAt}
}

func (s *Server) handleCreateNamespace(w http.ResponseWriter, r *http.Request) error {
	var req createNamespaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON object with non-empty name and slug fields", "decode namespace: %v", err)
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	if req.Name == "" || req.Slug == "" {
		return badRequest("send non-empty name and slug fields", "namespace name and slug are required")
	}
	ns, err := s.Store.CreateNamespace(r.Context(), req.Slug, req.Name)
	if errors.Is(err, postgres.ErrDuplicateNamespace) {
		return conflict("choose a different slug or list namespaces to use the existing row", "namespace slug %q already exists", req.Slug)
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, namespaceOut(ns))
	return nil
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
