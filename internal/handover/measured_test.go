package handover_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// evidenceRecord is one live handover-evidence record shaped exactly as
// buildRecord writes it, so the reader under test is exercised against the
// writer's real payload rather than a hand-drawn approximation of it.
func evidenceRecord(t *testing.T, id, sha string, paths []string) ledger.Record {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"producer_id":       "act_control_plane",
		"collection_method": handover.CollectionMethod,
		"measurements": map[string]any{
			"ref":           testHandoverRef,
			"commit_sha":    sha,
			"changed_paths": paths,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return ledger.Record{
		ID:         id,
		RecordType: ledger.RecordEvidence,
		Authority:  ledger.AuthorityObserved,
		Data:       payload,
	}
}

func TestMeasuredReportsThePathsTheFetchSaw(t *testing.T) {
	records := []ledger.Record{
		evidenceRecord(t, "rec_1", strings.Repeat("a", 40),
			[]string{"internal/api/server.go", ".github/workflows/tests.yml"}),
	}

	measured, ok := handover.Measured(records)
	if !ok {
		t.Fatal("Measured found no handover; want the one that is there")
	}
	if measured.RecordID != "rec_1" {
		t.Fatalf("record id = %q, want rec_1", measured.RecordID)
	}
	if measured.CommitSHA != strings.Repeat("a", 40) {
		t.Fatalf("commit = %q", measured.CommitSHA)
	}
	if len(measured.ChangedPaths) != 2 || measured.ChangedPaths[1] != ".github/workflows/tests.yml" {
		t.Fatalf("changed paths = %v, want both paths in the order they were recorded", measured.ChangedPaths)
	}
	if measured.Ref != testHandoverRef {
		t.Fatalf("ref = %q, want the measured ref", measured.Ref)
	}
}

func TestMeasuredFindsNothingWhenNoHandoverWasFetched(t *testing.T) {
	if _, ok := handover.Measured(nil); ok {
		t.Fatal("Measured reported a handover from an empty ledger")
	}
}

// A record that is not this package's fetch must not be read as one. A
// runner's ordinary workspace evidence carries a different collection method
// and different measurements, and mistaking it for a handover would feed a
// routing decision paths from the wrong measurement.
func TestMeasuredIgnoresEvidenceFromAnotherCollectionMethod(t *testing.T) {
	other := evidenceRecord(t, "rec_1", strings.Repeat("a", 40), []string{"x.go"})
	other.Data = json.RawMessage(`{"collection_method":"workspace_snapshot_diff",` +
		`"measurements":{"commit_sha":"` + strings.Repeat("a", 40) + `","changed_paths":["x.go"]}}`)

	if _, ok := handover.Measured([]ledger.Record{other}); ok {
		t.Fatal("Measured read a runner's workspace evidence as a fetched handover")
	}
}

// The newest measurement wins: records are immutable and a re-fetch appends,
// so an older path list must not be the one a routing decision reads.
func TestMeasuredTakesTheLastMeasurement(t *testing.T) {
	records := []ledger.Record{
		evidenceRecord(t, "rec_1", strings.Repeat("a", 40), []string{"old.go"}),
		evidenceRecord(t, "rec_2", strings.Repeat("b", 40), []string{"new.go"}),
	}

	measured, ok := handover.Measured(records)
	if !ok {
		t.Fatal("Measured found nothing")
	}
	if measured.RecordID != "rec_2" || measured.ChangedPaths[0] != "new.go" {
		t.Fatalf("Measured = %+v, want the later record", measured)
	}
}

// MeasuredCommit is now a projection of Measured. It must keep answering
// exactly as it did, because internal/api's verdict handler gates on it.
func TestMeasuredCommitStillAgreesWithMeasured(t *testing.T) {
	records := []ledger.Record{evidenceRecord(t, "rec_1", strings.Repeat("c", 40), []string{"x.go"})}

	id, sha := handover.MeasuredCommit(records)
	measured, _ := handover.Measured(records)
	if id != measured.RecordID || sha != measured.CommitSHA {
		t.Fatalf("MeasuredCommit = (%q, %q), Measured = (%q, %q)", id, sha, measured.RecordID, measured.CommitSHA)
	}
}
