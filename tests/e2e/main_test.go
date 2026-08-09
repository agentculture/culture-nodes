// Package e2etest is the Phase-1 vertical slice: the reference workflow in
// examples/delivery-loop driven end to end through the real API, the real
// engine, a real worker, a real scheduler, and a real runner boundary,
// against a real PostgreSQL.
//
// Nothing in this package reaches inside a component to make something
// happen. The only stand-ins are the things that genuinely live in somebody
// else's process — four HTTP actors and (in the default run) the code
// runner — and each of them speaks the same wire contract a deployed one
// would. The `e2elive` variant in live_test.go replaces the runner with the
// real headspace-cli Docker bridge.
package e2etest

import (
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is the shared, migrated PostgreSQL instance these tests need. It
// is nil when neither NODES_TEST_DATABASE_URL nor a usable Docker install is
// available, in which case every test skips itself.
var testStore *postgres.Store

// testDatabaseURL is the connection string testStore was built from. The
// restart-survival test needs it: proving a run survives a process restart
// means closing the pool entirely and opening a brand new one, which a
// borrowed *postgres.Store cannot do without breaking every other test in
// the package.
var testDatabaseURL string

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *postgres.Store) {
		testStore = s
		testDatabaseURL = s.Pool().Config().ConnString()
	}))
}
