package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// ErrRecordNotFound is returned by stores and by lookups when no record has
// the requested id.
var ErrRecordNotFound = errors.New("ledger: record not found")

// ErrReviewNotFound is returned when no review request has the requested id.
var ErrReviewNotFound = errors.New("ledger: review request not found")

// ErrStaleReview reports that a review was written against a ledger that has
// since moved. A stale review is rejected in full rather than applied to
// changed work (PRD §10.8): when CommitReview returns an error matching this
// sentinel, nothing at all was appended.
var ErrStaleReview = errors.New("ledger: stale review")

// ErrReviewAlreadyCommitted reports a second commit of a review that has
// already been applied. Reviews are transactions, not toggles.
var ErrReviewAlreadyCommitted = errors.New("ledger: review already committed")

// ErrAlreadySuperseded reports an attempt to supersede a record that a live
// record already replaces. Two live replacements for one record would leave
// the projections with no defensible answer about which one holds.
var ErrAlreadySuperseded = errors.New("ledger: record is already superseded")

// Authority rules, quoted by AuthorityError.Rule. The identifiers are stable:
// callers, tests, and API error bodies match on them, so they are API.
const (
	// RuleAgentProposesOnly — agents may create proposed records only
	// (PRD §10.4). An agent saying "done" is a completion claim, not
	// verified evidence.
	RuleAgentProposesOnly = "agent_proposes_only"
	// RuleHumanReviewOnly — a human may write confirmed or rejected
	// authority only inside a review transaction (PRD §10.8).
	RuleHumanReviewOnly = "human_confirms_only_through_review"
	// RuleHumanAuthorityLimited — a human may not write observed or derived
	// authority: those belong to measuring runners and deterministic
	// computation, not to a person asserting them.
	RuleHumanAuthorityLimited = "human_authority_limited"
	// RuleReviewRecordOnly — confirmed and rejected authority is carried by
	// review records; a review transaction never rewrites its target.
	RuleReviewRecordOnly = "review_authority_requires_review_record"
	// RuleRunnerEvidenceOnly — a runner may only produce evidence records.
	RuleRunnerEvidenceOnly = "runner_produces_evidence_only"
	// RuleRunnerObservedOnly — a runner's evidence carries observed
	// authority; a runner has no standing to propose, confirm, or derive.
	RuleRunnerObservedOnly = "runner_authority_observed_only"
	// RuleRunnerManifestRequired — observed evidence must be accompanied by
	// the runner manifest that declares what the runner measures.
	RuleRunnerManifestRequired = "runner_manifest_required"
	// RuleRunnerActorMismatch — the manifest must belong to the actor named
	// in the record's origin.
	RuleRunnerActorMismatch = "runner_manifest_actor_mismatch"
	// RuleRunnerFieldNotDeclared — a runner may only observe the fields its
	// manifest declares it directly measured (PRD §10.4, §10.5). This is
	// the rule that keeps process-reported text from becoming
	// runner-observed evidence.
	RuleRunnerFieldNotDeclared = "runner_field_not_declared"
	// RuleDeterministicDerivedOnly — engine projections and validators
	// create derived records and nothing else.
	RuleDeterministicDerivedOnly = "deterministic_producer_derives_only"
	// RuleUnknownOrigin — no producer rule exists for this origin kind, so
	// there is no authority it is permitted to write.
	RuleUnknownOrigin = "no_producer_rule_for_origin"
	// RuleSupersededNotAppendable — superseded is a derived state read from
	// the Supersedes pointers of later records, never an authority a
	// producer writes onto a record of its own.
	RuleSupersededNotAppendable = "superseded_is_not_appendable"
	// RuleInvalidAuthority — the authority value is not one the ledger
	// recognises at all.
	RuleInvalidAuthority = "authority_not_recognised"
)

// AuthorityError reports that the producer/authority matrix (PRD §10.4)
// refused an append, and names the rule that refused it. It carries the
// producer and the authority it asked for so a caller can report the refusal
// without re-deriving it.
type AuthorityError struct {
	// Rule is one of the Rule* constants above.
	Rule string
	// RecordID is the record that was refused, when it already had an id.
	RecordID string
	// Origin is the producer kind that attempted the append.
	Origin OriginKind
	// ActorID is the producer that attempted the append.
	ActorID string
	// Authority is the authority the record asked for.
	Authority Authority
	// RecordType is the record type that was refused.
	RecordType RecordType
	// Field names the offending payload location for field-scoped rules —
	// a JSON Pointer into the record's data, e.g. "/measurements/exit_code".
	// It is empty for rules that are not field-scoped.
	Field string
	// Detail explains the refusal in prose.
	Detail string
}

func (e *AuthorityError) Error() string {
	var b strings.Builder
	b.WriteString("ledger: authority refused [")
	b.WriteString(e.Rule)
	b.WriteString("]: origin ")
	b.WriteString(string(e.Origin))
	if e.ActorID != "" {
		b.WriteString(" (")
		b.WriteString(e.ActorID)
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " may not append a %s record with authority %s", e.RecordType, e.Authority)
	if e.Field != "" {
		b.WriteString(" at ")
		b.WriteString(e.Field)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Stale-review reasons, quoted by StaleReviewError.Reason.
const (
	// StaleLedgerMoved — records were appended to the run since the review
	// was requested.
	StaleLedgerMoved = "ledger_version_moved"
	// StaleRequestVersionMismatch — the caller's expected version is not
	// the version the review request recorded reading.
	StaleRequestVersionMismatch = "request_version_mismatch"
	// StaleFrameChecksum — the reviewed records are not the records the
	// request checksummed.
	StaleFrameChecksum = "frame_checksum_mismatch"
	// StaleTargetSuperseded — a record under review has been superseded.
	StaleTargetSuperseded = "target_superseded"
)

// StaleReviewError reports which staleness guard refused a review commit and
// what it saw. It matches ErrStaleReview under errors.Is.
type StaleReviewError struct {
	ReviewID string
	// Reason is one of the Stale* constants above.
	Reason string
	// Expected is the ledger version the reviewer reviewed at, when the
	// reason is version-shaped.
	Expected int64
	// Actual is the ledger version found at commit time.
	Actual int64
	// Detail explains the refusal in prose.
	Detail string
}

func (e *StaleReviewError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ledger: stale review %s [%s]", e.ReviewID, e.Reason)
	if e.Expected != e.Actual {
		fmt.Fprintf(&b, ": reviewed at ledger version %d, current version is %d", e.Expected, e.Actual)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	b.WriteString(" — nothing was applied")
	return b.String()
}

// Is reports StaleReviewError as ErrStaleReview so callers can branch on the
// sentinel without type-asserting for the detail.
func (e *StaleReviewError) Is(target error) bool { return target == ErrStaleReview }

// ReviewTargetError reports a review whose decision set does not match the
// records the request named. A review is a transaction over an agreed set;
// deciding a record nobody asked about, or leaving one undecided, is not a
// partial success.
type ReviewTargetError struct {
	ReviewID string
	// Unknown lists decided record ids the request did not name.
	Unknown []string
	// Undecided lists requested record ids the decision set left out.
	Undecided []string
}

func (e *ReviewTargetError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ledger: review %s decision set does not match the request", e.ReviewID)
	if len(e.Unknown) > 0 {
		fmt.Fprintf(&b, ": decided records not under review: %s", strings.Join(e.Unknown, ", "))
	}
	if len(e.Undecided) > 0 {
		fmt.Fprintf(&b, ": records left undecided: %s", strings.Join(e.Undecided, ", "))
	}
	return b.String()
}
