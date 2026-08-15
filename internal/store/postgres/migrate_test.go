package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
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

// TestMigrateSerializesConcurrentCallers gives several independent pools the
// same empty schema and starts Migrate simultaneously. Without the advisory
// lock, callers race while creating the same PostgreSQL catalog objects.
func TestMigrateSerializesConcurrentCallers(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	// Lower-cased deliberately. The schema is CREATEd quoted, which preserves
	// case, but `search_path` is passed unquoted in the connection string and
	// PostgreSQL folds unquoted identifiers to lower case — so an upper-case
	// ULID yields a search_path naming a schema that does not exist, and every
	// caller fails with "no schema has been selected to create in" before the
	// advisory lock is ever exercised.
	schema := strings.ToLower("migrate_concurrent_" + store.NewULID())
	if _, err := s.Pool().Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
	})

	parsed, err := url.Parse(s.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	const callers = 4
	stores := make([]*storepg.Store, callers)
	for i := range stores {
		stores[i], err = storepg.Connect(ctx, parsed.String())
		if err != nil {
			t.Fatalf("connect caller %d: %v", i, err)
		}
		defer stores[i].Close()
	}

	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i, migrationStore := range stores {
		wg.Add(1)
		go func(i int, migrationStore *storepg.Store) {
			defer wg.Done()
			<-start
			_, migrateErr := migrationStore.Migrate(ctx)
			if migrateErr != nil {
				errs <- fmt.Errorf("caller %d: %w", i, migrateErr)
			}
		}(i, migrationStore)
	}
	close(start)
	wg.Wait()
	close(errs)
	for migrateErr := range errs {
		t.Error(migrateErr)
	}
}
