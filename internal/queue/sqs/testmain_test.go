package sqs_test

import (
	"os"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is shared across every test in this package that needs real
// PostgreSQL (the duplicate-delivery and drop chaos tests, which prove
// properties about the fenced claim and the outbox relay respectively). It
// is nil only when neither NODES_TEST_DATABASE_URL nor a usable Docker
// install is available; individual tests call pgtest.RequireStore(t,
// testStore), which t.Skip()s in that case. Tests that only need the fake
// SQS server (driver_test.go's round-trip tests, the reorder and
// unknown-schema-version chaos tests) never touch testStore at all.
var testStore *storepg.Store

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *storepg.Store) { testStore = s }))
}
