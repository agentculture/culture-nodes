package notifier

import (
	"os"
	"testing"
)

// TestMain unsets both webhook env vars before any test in this package
// (and the sibling notifier_test external test package, compiled into the
// same test binary) runs -- the same hermetic baseline internal/notify's
// own TestMain establishes, needed here too because daemon_test.go drives
// real notify.Notify calls and must never pick up an ambient production
// webhook URL from whatever environment `go test` happens to run in.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CULTURE_NODES_WEBHOOK_URL")
	_ = os.Unsetenv("DISCORD_WEBHOOK_URL")
	os.Exit(m.Run())
}
