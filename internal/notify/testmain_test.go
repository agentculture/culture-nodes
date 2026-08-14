package notify_test

import (
	"os"
	"testing"
)

// TestMain unsets both webhook env vars before any test in this package
// runs, so the suite can never pick up an ambient production webhook URL
// from whatever environment `go test` happens to run in — this is the
// package-wide equivalent of the "autouse fixture" the economy-discord-graphs
// non-goal describes (devex's hermetic pytest pattern), applied at the Go
// TestMain level since Go has no per-test autouse hook.
//
// Individual tests that need a specific value use t.Setenv, which layers
// cleanly on top of this unset baseline and restores it automatically when
// the (sub)test ends — so no test can leak a value into another test, and
// no test can leak one out into whatever ran this binary.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(envPrimary)
	_ = os.Unsetenv(envFallback)
	os.Exit(m.Run())
}
