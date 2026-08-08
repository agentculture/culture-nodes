package events_test

import (
	"os"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is shared across every test in this package. It is nil only
// when neither NODES_TEST_DATABASE_URL nor a usable Docker install is
// available; individual tests call pgtest.RequireStore(t, testStore), which
// t.Skip()s in that case.
var testStore *storepg.Store

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *storepg.Store) { testStore = s }))
}
