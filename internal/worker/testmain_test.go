package worker_test

import (
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is the shared, migrated PostgreSQL instance the end-to-end tests
// need. It is nil when neither NODES_TEST_DATABASE_URL nor a usable Docker
// install is available, in which case those tests skip themselves and the
// in-package unit tests (bindings, decisions) still run.
var testStore *postgres.Store

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *postgres.Store) { testStore = s }))
}
