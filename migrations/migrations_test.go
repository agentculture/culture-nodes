package migrations

import (
	"strings"
	"testing"
)

func TestRetireStoredParticipantAddressesRecordsApprovedBypass(t *testing.T) {
	sql, err := FS.ReadFile("0036_retire_stored_participant_addresses.sql")
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
