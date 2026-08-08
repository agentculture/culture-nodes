package scheduler_test

import (
	"os"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is shared across every test in this package (the same pattern
// internal/events and internal/queue/postgres use -- see pgtest's package
// doc for why this cannot just import internal/store/postgres's own
// testmain_test.go). It is nil only when neither NODES_TEST_DATABASE_URL
// nor a usable Docker install is available; individual tests call
// requireStore(t), which t.Skip()s in that case.
var testStore *storepg.Store

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *storepg.Store) { testStore = s }))
}

func requireStore(t *testing.T) *storepg.Store {
	t.Helper()
	return pgtest.RequireStore(t, testStore)
}
