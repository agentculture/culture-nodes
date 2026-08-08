package api

import (
	"context"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// EnsureNamespace resolves slug to a namespace id, creating it with
// displayName if it does not already exist. It is idempotent: a second call
// with the same slug returns the same id.
//
// internal/store/postgres.Store has no typed "get or create" for namespaces
// — CreateNamespace is create-only, matching how every other task has
// needed it so far — so this lives here rather than in that package,
// reading through Store.Pool() (the escape hatch that package's own doc
// comment names for callers with no typed method to reach for). `nodes
// serve`/`nodes all` call this once at startup to resolve the single
// namespace they run against (see cmd/nodes/serve.go).
func EnsureNamespace(ctx context.Context, store *postgres.Store, slug, displayName string) (string, error) {
	ns, createErr := store.CreateNamespace(ctx, slug, displayName)
	if createErr == nil {
		return ns.ID, nil
	}

	var id string
	if err := store.Pool().QueryRow(ctx, `SELECT id FROM namespaces WHERE slug = $1`, slug).Scan(&id); err != nil {
		return "", fmt.Errorf("api: ensure namespace %q: create failed (%v) and lookup failed: %w", slug, createErr, err)
	}
	return id, nil
}
