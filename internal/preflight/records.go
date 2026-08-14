package preflight

import (
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The two ledger records the gate writes, built in ONE place.
//
// The two halves of the protocol are produced by different processes — the
// worker composes and appends the briefing at the dispatch site, the API
// appends the acknowledgement when the confirm verb is called — and the
// records have to agree about their own shape: an acknowledgement that named
// the briefing differently from how the briefing named itself would make
// "the actor was told THIS" unverifiable, which is the whole point of the
// record. Two builders here, rather than two hand-assembled ledger.Records
// at two call sites, is what keeps that from being a matter of care.
//
// Neither builder chooses an authority the caller could override: the
// preflight is derived and the acknowledgement is proposed, full stop. The
// ledger's own authority matrix refuses anything else for these record types
// (RulePreflightDerivedOnly, RuleAcknowledgementNeverDerived), so a caller
// that tried would be refused at the append — these builders simply mean
// nobody has to try.

// DispatchGateActorID is the producer identity the engine's dispatch gate
// derives preflights under.
//
// It names the GATE, not the worker process that happened to run it: a
// derived record's producer is the deterministic computation that produced
// it (PRD §10.4), and two workers composing the same briefing from the same
// inputs are the same producer in every sense that matters to a reader.
const DispatchGateActorID = "engine_dispatch_gate"

// NewPreflightRecord builds the DERIVED record stating what an actor is
// being told before its first billable turn.
//
// producerActorID defaults to DispatchGateActorID. Provenance is left to the
// caller: the composition's inputs are an actor registration and a pinned
// workflow definition, neither of which is a ledger record to point at.
func NewPreflightRecord(doc Document, producerActorID string) (ledger.Record, error) {
	if producerActorID == "" {
		producerActorID = DispatchGateActorID
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("preflight: encode preflight document: %w", err)
	}
	return ledger.Record{
		RecordType: ledger.RecordDispatchPreflight,
		RunID:      doc.Task.RunID,
		NodeRunID:  ledger.NullableID(doc.Task.NodeRunID),
		Origin: ledger.Origin{
			Kind:    ledger.OriginEngine,
			ActorID: producerActorID,
		},
		Authority: ledger.AuthorityDerived,
		Data:      payload,
	}, nil
}

// AcknowledgementInput carries what an acknowledgement has to name.
type AcknowledgementInput struct {
	RunID     string
	NodeRunID string
	// PreflightRecordID and PreflightDigest identify the briefing being
	// acknowledged — WHICH one and WHAT it said. The digest is not
	// decoration: without it, "the actor acknowledged this briefing" could
	// later be true of a briefing whose content the reader never saw.
	PreflightRecordID string
	PreflightDigest   string
	// OriginKind is the producer that wrote the acknowledgement: agent when
	// a bridge acknowledges for itself, human when an operator acknowledges
	// on its behalf. It is never engine or validator — a deterministic
	// producer cannot acknowledge for somebody else, and the ledger refuses
	// it (RuleAcknowledgementNeverDerived).
	OriginKind ledger.OriginKind
	// OriginActorID is that producer's identity; AcknowledgedBy is the actor
	// the acknowledgement is recorded FOR. They are usually the same and are
	// kept separate anyway, because when an operator acknowledges for a
	// bridge the two must stay distinguishable afterwards.
	OriginActorID  string
	AcknowledgedBy string
	Note           string
}

// NewAcknowledgementRecord builds the PROPOSED record carrying an actor's
// claim to have been told and understood.
//
// It is proposed and stays proposed. An acknowledgement is a
// completion-claim-shaped statement about the actor's own state of mind, and
// nothing on this path — least of all the actor — promotes it. A human who
// wants to attest that an actor genuinely understood does it the ordinary
// way: a review transaction against this record.
func NewAcknowledgementRecord(in AcknowledgementInput) (ledger.Record, error) {
	if in.PreflightRecordID == "" || in.PreflightDigest == "" {
		return ledger.Record{}, fmt.Errorf(
			"preflight: an acknowledgement must name the preflight record and its digest")
	}
	origin := in.OriginKind
	if origin == "" {
		origin = ledger.OriginAgent
	}

	data := map[string]any{
		"verdict":          VerdictProceed,
		"preflight_ref":    in.PreflightRecordID,
		"preflight_digest": in.PreflightDigest,
	}
	if in.AcknowledgedBy != "" {
		data["acknowledged_by"] = in.AcknowledgedBy
	}
	if in.Note != "" {
		data["note"] = in.Note
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("preflight: encode acknowledgement: %w", err)
	}

	return ledger.Record{
		RecordType: ledger.RecordDispatchAcknowledgement,
		RunID:      in.RunID,
		NodeRunID:  ledger.NullableID(in.NodeRunID),
		Origin: ledger.Origin{
			Kind:    origin,
			ActorID: in.OriginActorID,
		},
		Authority:  ledger.AuthorityProposed,
		SubjectRef: ledger.NullableID(in.PreflightRecordID),
		// The briefing is this record's provenance in the literal sense:
		// the acknowledgement exists because that record was read.
		ProvenanceRefs: []string{in.PreflightRecordID},
		Data:           payload,
	}, nil
}
