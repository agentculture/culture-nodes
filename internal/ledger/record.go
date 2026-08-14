package ledger

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// SchemaVersion is the ledger schema version this runtime writes. It is the
// value stamped on every record whose SchemaVersion is left empty.
const SchemaVersion = "nodes.culture.dev/ledger/v1alpha1"

// IDPrefix prefixes generated record ids, matching the PRD §10.3 envelope
// example ("ledger_01J...").
const IDPrefix = "ledger_"

// ReviewIDPrefix prefixes generated review-request ids.
const ReviewIDPrefix = "review_"

// RecordType is one of the MVP record types (PRD §10.2). Additional
// domain-specific types may be registered by schema later, but they may not
// redefine core authority semantics.
type RecordType string

// The MVP record types, in PRD §10.2 order.
const (
	RecordAnnouncement  RecordType = "announcement"
	RecordClaim         RecordType = "claim"
	RecordAssumption    RecordType = "assumption"
	RecordQuestion      RecordType = "question"
	RecordTask          RecordType = "task"
	RecordDecision      RecordType = "decision"
	RecordSuccessSignal RecordType = "success_signal"
	RecordEvidence      RecordType = "evidence"
	RecordResult        RecordType = "result"
	RecordReview        RecordType = "review"
	// RecordGrade is a domain record type registered additively, after the
	// PRD §10.2 MVP set (issue #28 item 1): a first-class opinion — rating
	// plus rationale — evaluating a completed run/attempt against the
	// evaluated actor. It carries the same envelope and follows the same
	// producer/authority matrix, extended by two grade-specific rules (see
	// RuleGradeNeverObservedOrDerived and RuleNoSelfGrade in errors.go).
	RecordGrade RecordType = "grade"
	// RecordDispatchPreflight is the clarify-then-commit gate's briefing
	// (issue #67, task t14): the operating facts a dispatch depends on,
	// composed by the engine from the actor's advertised host capabilities
	// and the task declaration, handed over BEFORE the first billable turn.
	// It is registered additively after the PRD §10.2 MVP set, like
	// RecordGrade, and carries derived authority only (see
	// RulePreflightDerivedOnly).
	RecordDispatchPreflight RecordType = "dispatch_preflight"
	// RecordDispatchAcknowledgement is the actor's answer to a
	// RecordDispatchPreflight: the second, separate action that commits the
	// dispatch. It is the actor's own claim to have been told and to have
	// understood — proposed authority only, never derived (see
	// RuleAcknowledgementNeverDerived), because an acknowledgement nobody
	// made is not an acknowledgement.
	RecordDispatchAcknowledgement RecordType = "dispatch_acknowledgement"
)

// RecordTypes returns the registered record types: the PRD §10.2 MVP set in
// its order, followed by the additively-registered ones.
func RecordTypes() []RecordType {
	return []RecordType{
		RecordAnnouncement, RecordClaim, RecordAssumption, RecordQuestion,
		RecordTask, RecordDecision, RecordSuccessSignal, RecordEvidence,
		RecordResult, RecordReview, RecordGrade,
		RecordDispatchPreflight, RecordDispatchAcknowledgement,
	}
}

// Valid reports whether t is a registered record type.
func (t RecordType) Valid() bool {
	for _, known := range RecordTypes() {
		if t == known {
			return true
		}
	}
	return false
}

// Authority is a core authority value (PRD §10.4).
type Authority string

// The core authority values.
const (
	// AuthorityProposed marks a suggestion by an actor. It is not
	// authoritative.
	AuthorityProposed Authority = "proposed"
	// AuthorityConfirmed marks explicit acceptance by an authorized human
	// or policy gate. Reachable only through CommitReview.
	AuthorityConfirmed Authority = "confirmed"
	// AuthorityObserved marks a fact directly measured by an identified
	// trusted runner or tool.
	AuthorityObserved Authority = "observed"
	// AuthorityDerived marks a value deterministically computed from
	// referenced confirmed or observed records.
	AuthorityDerived Authority = "derived"
	// AuthorityRejected marks explicit rejection by an authorized reviewer
	// or validator. Reachable only through CommitReview.
	AuthorityRejected Authority = "rejected"
	// AuthoritySuperseded appears in the schema's enum for completeness but
	// is never appendable: replacement is expressed by the replacing
	// record's Supersedes pointer. See RuleSupersededNotAppendable.
	AuthoritySuperseded Authority = "superseded"
)

// Authorities returns the core authority values in PRD §10.4 order.
func Authorities() []Authority {
	return []Authority{
		AuthorityProposed, AuthorityConfirmed, AuthorityObserved,
		AuthorityDerived, AuthorityRejected, AuthoritySuperseded,
	}
}

// Valid reports whether a is a core authority value.
func (a Authority) Valid() bool {
	for _, known := range Authorities() {
		if a == known {
			return true
		}
	}
	return false
}

// OriginKind identifies the class of producer that created a record.
type OriginKind string

// The producer kinds the envelope schema admits. PRD §10.4 states a producer
// rule for agent, human, runner, and deterministic engine/validator
// producers. It states none for service, so a service-origin append is
// refused until one exists — an unstated rule is not a permission.
const (
	OriginAgent     OriginKind = "agent"
	OriginHuman     OriginKind = "human"
	OriginRunner    OriginKind = "runner"
	OriginService   OriginKind = "service"
	OriginEngine    OriginKind = "engine"
	OriginValidator OriginKind = "validator"
)

// OriginKinds returns the producer kinds the envelope schema admits.
func OriginKinds() []OriginKind {
	return []OriginKind{
		OriginAgent, OriginHuman, OriginRunner,
		OriginService, OriginEngine, OriginValidator,
	}
}

// Valid reports whether k is a producer kind the envelope schema admits.
// Validity is not permission: see CheckAuthority.
func (k OriginKind) Valid() bool {
	for _, known := range OriginKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// NullableID is an identifier field that is explicitly null when unset, so
// the absence of a reference stays visible in the record rather than being
// spelled as an empty string the schema would reject.
type NullableID string

// String returns the identifier, or "" when it is null.
func (n NullableID) String() string { return string(n) }

// MarshalJSON emits null for the zero value and a JSON string otherwise.
func (n NullableID) MarshalJSON() ([]byte, error) {
	if n == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(n))
}

// UnmarshalJSON accepts null (yielding the zero value) or a JSON string.
func (n *NullableID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*n = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("ledger: decode nullable identifier: %w", err)
	}
	*n = NullableID(s)
	return nil
}

// Origin is who produced a record (PRD §10.3). Identity is not execution: the
// actor named here is the producer, not necessarily the executor of the
// underlying work.
type Origin struct {
	Kind    OriginKind `json:"kind"`
	ActorID string     `json:"actor_id"`
	// ActorRevision is the producer's revision (model build, runner
	// revision, validator version) when the producer can report one. It is
	// omitted rather than emitted empty, because the schema requires a
	// non-empty string when the field is present.
	ActorRevision string `json:"actor_revision,omitempty"`
}

// Record is the common work-ledger envelope (PRD §10.3). Field order here
// mirrors the PRD's example; the canonical JSON form sorts keys, so the
// declaration order carries no meaning beyond readability.
//
// The four nullable reference fields are always emitted — as null when
// unset — so a record's shape does not change with its content and its
// digest covers the fact that a reference was absent.
type Record struct {
	ID             string          `json:"id"`
	SchemaVersion  string          `json:"schema_version"`
	RecordType     RecordType      `json:"record_type"`
	RunID          string          `json:"run_id"`
	NodeRunID      NullableID      `json:"node_run_id"`
	AttemptID      NullableID      `json:"attempt_id"`
	Origin         Origin          `json:"origin"`
	Authority      Authority       `json:"authority"`
	SubjectRef     NullableID      `json:"subject_ref"`
	Data           json.RawMessage `json:"data"`
	ProvenanceRefs []string        `json:"provenance_refs"`
	Supersedes     NullableID      `json:"supersedes"`
	CreatedAt      time.Time       `json:"created_at"`
	ContentDigest  string          `json:"content_digest"`
}

// Clone returns a deep copy. Records are handed to callers and stores by
// value; the two slice-shaped fields would otherwise still be shared.
func (r Record) Clone() Record {
	out := r
	if r.Data != nil {
		out.Data = append(json.RawMessage(nil), r.Data...)
	}
	if r.ProvenanceRefs != nil {
		out.ProvenanceRefs = append([]string(nil), r.ProvenanceRefs...)
	}
	return out
}

// ComputeDigest returns the sha256 digest of the record's canonical JSON form
// with content_digest removed — the envelope minus the digest, so the digest
// is a statement about the record's content rather than about itself.
func (r Record) ComputeDigest() (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("ledger: marshal record %s: %w", r.ID, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("ledger: decode record %s: %w", r.ID, err)
	}
	delete(fields, "content_digest")

	digest, err := contracts.DigestValue(fields)
	if err != nil {
		return "", fmt.Errorf("ledger: digest record %s: %w", r.ID, err)
	}
	return digest, nil
}

// VerifyDigest recomputes the record's digest and reports whether the stored
// ContentDigest still matches it. A mismatch means the record was altered
// after it was appended, which the store is built to make impossible — so a
// mismatch is a corruption report, not a validation failure.
func (r Record) VerifyDigest() error {
	want, err := r.ComputeDigest()
	if err != nil {
		return err
	}
	if r.ContentDigest != want {
		return fmt.Errorf("ledger: record %s digest mismatch: stored %s, computed %s",
			r.ID, r.ContentDigest, want)
	}
	return nil
}

// DataMap decodes the record payload into a generic map. A record that has
// been through Append always carries a JSON object here, because the record
// schema requires one.
func (r Record) DataMap() (map[string]any, error) {
	if len(r.Data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(r.Data, &out); err != nil {
		return nil, fmt.Errorf("ledger: decode data of record %s: %w", r.ID, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// dataString reads a top-level string field from the payload, returning ""
// when it is absent or not a string. Projections use it to read documented
// payload fields (task status, question answer) without failing on records
// whose loose Phase-0 payload simply omits them.
func dataString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

// dataBool reads a top-level boolean field from the payload, returning false
// when it is absent or not a boolean.
func dataBool(data map[string]any, key string) bool {
	if v, ok := data[key].(bool); ok {
		return v
	}
	return false
}
