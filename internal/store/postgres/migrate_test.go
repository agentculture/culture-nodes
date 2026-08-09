package postgres_test

import (
	"context"
	"testing"
)

// TestMigrateIsIdempotent proves nodes migrate (and Store.Migrate, which
// backs it) is safe to run repeatedly -- required for it to work as a k8s
// pre-install/pre-upgrade Job that fires on every deploy, not just the
// first one (docs/adr/0002-migration-policy.md).
func TestMigrateIsIdempotent(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	// TestMain already applied every migration once to set up the schema
	// this whole package's tests run against. Running Migrate again here
	// must find nothing pending and must not error.
	for attempt := 1; attempt <= 2; attempt++ {
		applied, err := s.Migrate(ctx)
		if err != nil {
			t.Fatalf("Migrate() run #%d: %v", attempt, err)
		}
		if len(applied) != 0 {
			t.Fatalf("Migrate() run #%d applied %v, want none (schema_migrations should already record every version)", attempt, applied)
		}
	}
}
