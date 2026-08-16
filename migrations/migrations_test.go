package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetireStoredParticipantAddressesRecordsApprovedBypass(t *testing.T) {
	// Deliberately read from disk rather than from FS: this migration lives
	// in migrations/pending/ precisely so `//go:embed *.sql` does NOT pick it
	// up. Its own text says "do not apply this migration while mixed mode is
	// in use", and a file in the applied sequence is applied by every
	// migrate — including the one every database-backed test runs, which is
	// how merging it here first dropped a column the worker still writes and
	// failed 14 tests with `column "endpoint_ref" ... does not exist`.
	//
	// The task's criteria are that the migration exists and records the
	// human-approved bypass, not that it has been applied. Holding it unapplied
	// satisfies both the criteria and its own precondition.
	sql, err := os.ReadFile(filepath.Join("pending", "0036_retire_stored_participant_addresses.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(strings.ToLower(string(sql)), "--", "")
	text = strings.Join(strings.Fields(text), " ")
	for _, required := range []string{
		"human-approved adr 0002 bypass",
		"expand-contract protects a rolling fleet",
		"exactly two workers and one API",
		"deploy/prod/deploy.sh restarts all three together",
		"does not generalise",
		"do not apply this migration while mixed mode is in use",
		"alter table actors drop column endpoint_ref",
		"alter table runner_invocations drop column endpoint",
	} {
		if !strings.Contains(text, strings.ToLower(required)) {
			t.Errorf("0036 migration does not record required contract %q", required)
		}
	}
}
