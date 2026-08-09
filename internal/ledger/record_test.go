package ledger_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestAppendStampsTheEnvelope pins what the runtime fills in and what it
// leaves alone.
func TestAppendStampsTheEnvelope(t *testing.T) {
	l, _ := newTestLedger(t)

	rec := mustAppend(t, l, claimRecord(t, "The change-set applies cleanly."))

	if rec.ID == "" {
		t.Fatal("appended record has no id")
	}
	if rec.SchemaVersion != ledger.SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", rec.SchemaVersion, ledger.SchemaVersion)
	}
	if rec.CreatedAt.IsZero() {
		t.Fatal("appended record has no created_at")
	}
	if rec.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at is in %v, want UTC", rec.CreatedAt.Location())
	}
	if rec.ProvenanceRefs == nil {
		t.Fatal("provenance_refs is nil; an absent provenance chain must stay explicit, not implied")
	}
	if rec.ContentDigest == "" {
		t.Fatal("appended record carries no content digest")
	}
	if err := rec.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest: %v", err)
	}
}

// TestAppendKeepsCallerSuppliedEnvelopeFields proves normalisation fills gaps
// rather than overwriting statements the caller made.
func TestAppendKeepsCallerSuppliedEnvelopeFields(t *testing.T) {
	l, _ := newTestLedger(t)

	when := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	rec := claimRecord(t, "A claim with a caller-chosen identity.")
	rec.ID = "ledger_CALLERCHOSEN0000000000001"
	rec.CreatedAt = when
	rec.SchemaVersion = ledger.SchemaVersion

	out := mustAppend(t, l, rec)

	if out.ID != rec.ID {
		t.Fatalf("id = %q, want the caller's %q", out.ID, rec.ID)
	}
	if !out.CreatedAt.Equal(when) {
		t.Fatalf("created_at = %v, want the caller's %v", out.CreatedAt, when)
	}
}

// TestContentDigestCoversContentNotItself proves the digest is computed over
// the envelope minus the digest field, and that it moves when any covered
// field moves.
func TestContentDigestCoversContentNotItself(t *testing.T) {
	base := ledger.Record{
		ID:             "ledger_01JAV3QK2M0000000000000001",
		SchemaVersion:  ledger.SchemaVersion,
		RecordType:     ledger.RecordClaim,
		RunID:          testRunID,
		Origin:         agentOrigin,
		Authority:      ledger.AuthorityProposed,
		Data:           json.RawMessage(`{"statement":"one"}`),
		ProvenanceRefs: []string{},
		CreatedAt:      time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC),
	}

	digest, err := base.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}

	// Stamping the digest onto the record must not change the digest: it is
	// a statement about the content, not about itself.
	stamped := base
	stamped.ContentDigest = digest
	restamped, err := stamped.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest after stamping: %v", err)
	}
	if restamped != digest {
		t.Fatalf("digest changed once stamped: %s -> %s", digest, restamped)
	}

	// A different payload is a different record.
	changed := base
	changed.Data = json.RawMessage(`{"statement":"two"}`)
	changedDigest, err := changed.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest of changed record: %v", err)
	}
	if changedDigest == digest {
		t.Fatal("changing the payload did not change the content digest")
	}

	// The digest must be independent of how the payload happened to be
	// spelled, because CanonicalJSON is what is hashed.
	respelled := base
	respelled.Data = json.RawMessage("{\n  \"statement\" : \"one\"\n}")
	respelledDigest, err := respelled.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest of respelled record: %v", err)
	}
	if respelledDigest != digest {
		t.Fatalf("re-spelling the payload changed the digest: %s != %s", respelledDigest, digest)
	}
}

// TestVerifyDigestDetectsAlteration proves a record that was changed after
// the fact fails verification — the corruption report the immutable store
// exists to make unnecessary.
func TestVerifyDigestDetectsAlteration(t *testing.T) {
	l, _ := newTestLedger(t)
	rec := mustAppend(t, l, claimRecord(t, "original"))

	tampered := rec
	tampered.Authority = ledger.AuthorityConfirmed
	if err := tampered.VerifyDigest(); err == nil {
		t.Fatal("VerifyDigest accepted a record whose authority was changed after append")
	}
}

// TestNullableReferencesSerialiseAsNull proves an absent reference is spelled
// null rather than "" — the schema rejects the empty string, and null is what
// says "there is no such reference" out loud.
func TestNullableReferencesSerialiseAsNull(t *testing.T) {
	l, _ := newTestLedger(t)
	rec := mustAppend(t, l, claimRecord(t, "no node run, no attempt, no subject"))

	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode record: %v", err)
	}

	for _, key := range []string{"node_run_id", "attempt_id", "subject_ref", "supersedes"} {
		value, present := fields[key]
		if !present {
			t.Fatalf("%s is absent from the serialised envelope; it must be present and null", key)
		}
		if string(value) != "null" {
			t.Fatalf("%s = %s, want null", key, value)
		}
	}

	// A round trip through JSON must reproduce the record exactly, digest
	// included: the envelope is the interchange format.
	var decoded ledger.Record
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode into Record: %v", err)
	}
	if err := decoded.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest after a JSON round trip: %v", err)
	}
}

// TestAppendedRecordsValidateAgainstTheShippedSchemas closes the loop between
// this package and schemas/ledger: every record type the runtime writes is
// checked against the same schema files that ship in the binary.
func TestAppendedRecordsValidateAgainstTheShippedSchemas(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("contracts.NewValidator: %v", err)
	}
	l, _ := newTestLedger(t)

	payloads := map[ledger.RecordType]map[string]any{
		ledger.RecordAnnouncement:  {"headline": "Deliver the change", "scope": "one repository"},
		ledger.RecordClaim:         {"statement": "It applies cleanly", "kind": "completion"},
		ledger.RecordAssumption:    {"statement": "The pinned image is current"},
		ledger.RecordQuestion:      {"question": "Which suite gates release?", "blocking": true},
		ledger.RecordTask:          {"goal": "Run the suite", "status": "ready", "assurance_state": "unverified"},
		ledger.RecordDecision:      {"question": "Which runner?", "selected": "headspace"},
		ledger.RecordSuccessSignal: {"statement": "suite exits zero", "mechanical": true},
		ledger.RecordResult:        {"outcome": "completed"},
	}

	for _, recordType := range ledger.RecordTypes() {
		payload, ok := payloads[recordType]
		if !ok {
			// evidence and review are produced by their own paths, covered
			// by the runner-manifest and review tests.
			continue
		}
		t.Run(string(recordType), func(t *testing.T) {
			rec, err := l.Append(context.Background(), ledger.Record{
				RecordType: recordType,
				RunID:      testRunID,
				Origin:     agentOrigin,
				Authority:  ledger.AuthorityProposed,
				Data:       mustJSON(t, payload),
			})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := validator.Validate(contracts.SchemaLedgerRecord, rec); err != nil {
				t.Fatalf("appended record fails the shipped record schema: %v", err)
			}
			if err := validator.Validate(contracts.LedgerRecordSchema(string(recordType)), rec); err != nil {
				t.Fatalf("appended record fails its own record-type schema: %v", err)
			}
		})
	}
}
