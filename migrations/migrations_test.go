package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetireStoredParticipantAddressesRecordsWithdrawnBypass(t *testing.T) {
	// Deliberately read from disk rather than from FS: this migration lives
	// in migrations/pending/ precisely so `//go:embed *.sql` does NOT pick it
	// up. A file in the applied sequence is applied by every migrate —
	// including the one every database-backed test runs, which is how merging
	// it here first dropped a column the worker still writes and failed 14
	// tests with `column "endpoint_ref" ... does not exist`.
	//
	// This test used to assert the migration recorded a HUMAN-APPROVED ADR
	// 0002 BYPASS. That bypass has been WITHDRAWN (issue #143), because its
	// factual premise was measured and found false: it claimed production was
	// "exactly two workers and one API" restarted together by deploy.sh, but
	// deploy.sh takes one host argument, thor and orin are two independent
	// deploy operations, the `migrate` service exists only in
	// compose.thor.yml, and the premise omitted the scheduler entirely.
	//
	// The N-1 window that opens is not survivable for THIS migration: the
	// previous orin worker reads actors.endpoint_ref and reads/writes
	// runner_invocations.endpoint. So the test now pins the opposite contract —
	// that the file records the withdrawal and the expand-contract sequence
	// that replaces it. Re-asserting a bypass here would be a test that
	// certifies a premise nobody re-measured.
	sql, err := os.ReadFile(filepath.Join("pending", "0036_retire_stored_participant_addresses.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(strings.ToLower(string(sql)), "--", "")
	text = strings.Join(strings.Fields(text), " ")
	for _, required := range []string{
		"the adr 0002 bypass is withdrawn",
		"independently deployed",
		"full expand-contract",
		"do not apply this migration while mixed mode is in use",
		"alter table actors drop column endpoint_ref",
		"alter table runner_invocations drop column endpoint",
	} {
		if !strings.Contains(text, strings.ToLower(required)) {
			t.Errorf("0036 migration does not record required contract %q", required)
		}
	}

	// The withdrawn bypass must not linger as prose that a later reader could
	// mistake for still-live authorization.
	for _, forbidden := range []string{
		"human-approved adr 0002 bypass",
		"exactly two workers and one api",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("0036 still asserts the withdrawn bypass premise %q", forbidden)
		}
	}
}
