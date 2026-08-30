package humanfanout_test

import (
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

var testStore *postgres.Store

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *postgres.Store) { testStore = s }))
}
