package handover

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// A suite's verdict over a handed-over commit, as derived evidence (task
// t11, issue #101).
//
// # Why this lives beside the fetch
//
// The rest of this package answers "what is in the ref the session handed
// over" — the ref, the commit, the paths. This answers the next question an
// operator actually gates a merge on: "and does the suite pass on it". The
// two are one sentence in the ledger only if they name the SAME commit, and
// the only way to keep them from drifting is to compose the verdict where the
// measurement already lives.
//
// # The authority is not a detail
//
// PRD §10.4 gives four producers four powers, and a merge gate has been
// getting this wrong by habit: an operator looking at a green tick and
// deciding to merge is not evidence of anything — it is an operator's opinion
// about a rendering. A test suite, in contrast, is a DETERMINISTIC VALIDATOR:
// it takes a commit and a command and produces a number, the same number
// every time. That is exactly the producer §10.4 admits `derived` records
// from, so this composes a validator-origin, derived-authority record and
// nothing else. It cannot produce `proposed` (that is what an agent's word is
// worth), `observed` (a trusted runner reporting what it directly measured),
// or `confirmed` (only a human review transaction).
//
// # Why a `review` record and not an `evidence` one
//
// It follows internal/worker/acceptance.go's appendAcceptanceVerdict, which
// is the closer of the two existing derived-record paths. Both that one and
// this compose a VERDICT ABOUT an already-appended record — a judgement whose
// SubjectRef is the evidence it was computed against — and `review` is the
// record type the ledger's own vocabulary gives that shape. The other path,
// internal/worker/successsignal.go, is the same record type but its subject
// is a `success_signal` the run itself authored ahead of time; a merge gate
// has no such record to point at, so following it would have meant inventing
// one.
//
// # The commit sha is the whole point
//
// A verdict that does not name what it tested is not evidence. `go test`
// prints `ok` for a package where every test skipped (the go job in
// .github/workflows/tests.yml carries a guard against exactly that), a CI run
// can be green against a stale checkout, and neither is visible afterwards
// from the word "passed". So Record refuses anything but a full 40-character
// lowercase hex sha: not "HEAD", not a branch name, not an abbreviation that
// could later become ambiguous, not an empty string. The refusal is the
// feature — an unrecordable verdict is a verdict somebody has to look at.

// VerdictCollectionMethod names how a verdict here was produced, for the
// record's own `collection_method` field. It is distinct from this package's
// CollectionMethod (`git_ref_fetch`) because the act is different: a fetch
// measures what a ref contains, and this runs a command against it and reads
// the number it exited with.
const VerdictCollectionMethod = "suite_exit_status"

// VerdictError is a refusal to compose a verdict record, naming the payload
// field that made the verdict unrecordable. Field is a JSON pointer so an API
// layer can hand it straight back to the caller.
type VerdictError struct {
	Field  string
	Detail string
}

func (e *VerdictError) Error() string {
	return fmt.Sprintf("handover: %s: %s", strings.TrimPrefix(e.Field, "/"), e.Detail)
}

// SuiteVerdict is one deterministic validator's run of one named suite
// against one named commit.
//
// Every field is a fact about the RUN OF THE SUITE. There is deliberately no
// field for anyone's opinion of the result, and none for a summary of what
// the change does: the exit code is the entire finding, and the reason line
// Record composes says so.
type SuiteVerdict struct {
	// RunID, NodeRunID and AttemptID place the verdict on the run whose
	// handover it judged. Only RunID is required — a gate run over a whole
	// run's collected handover has no single attempt to name.
	RunID     string
	NodeRunID string
	AttemptID string

	// Suite names what ran, in whatever spelling the operator's lane uses
	// for it ("go test ./...", "pytest -n auto", a CI job name). It must be
	// re-runnable-by-a-reader, which is why an empty one is refused.
	Suite string
	// Command is the argv the gate actually executed, when it executed one
	// itself. Absent when the verdict came from a suite run elsewhere.
	Command []string
	// ExitCode is the number the suite exited with. Zero confirms, anything
	// else rejects; the label is computed here and never supplied.
	ExitCode int

	// CommitSHA is the commit the suite ran against, full 40-hex.
	CommitSHA string
	// Ref is the handover ref that carried the commit, when one did. It is
	// validated against the same fence a fetch is (ValidateRef), so a
	// verdict cannot name a ref the collector would never have been allowed
	// to fetch.
	Ref string

	// ValidatorActorID identifies the deterministic producer. §10.4 admits
	// derived records only from an IDENTIFIED one.
	ValidatorActorID  string
	ValidatorRevision string

	// EvidenceRecordID is the observed handover-evidence record this verdict
	// judges, when the control plane has one. It becomes the record's
	// SubjectRef and its provenance — the same shape appendAcceptanceVerdict
	// uses for the evidence its own verdict was computed from.
	EvidenceRecordID string

	// EvaluatedAt is when the suite ran. Zero means now.
	EvaluatedAt time.Time
}

// Validate applies every refusal without composing anything, so a caller can
// reject a request before it reaches a ledger.
func (v SuiteVerdict) Validate() error {
	if strings.TrimSpace(v.RunID) == "" {
		return &VerdictError{Field: "/run_id", Detail: "a verdict must name the run whose handover it judged"}
	}
	if strings.TrimSpace(v.Suite) == "" {
		return &VerdictError{
			Field:  "/suite",
			Detail: "a verdict must name the suite that produced it; an unnamed suite cannot be re-run by a reader",
		}
	}
	if strings.TrimSpace(v.ValidatorActorID) == "" {
		return &VerdictError{
			Field: "/validator_actor_id",
			Detail: "a derived record needs an identified deterministic producer (PRD §10.4); " +
				"an anonymous validator attests to nothing",
		}
	}
	if err := validateFullSHA(v.CommitSHA); err != nil {
		return err
	}
	if v.Ref != "" {
		if err := ValidateRef(v.Ref); err != nil {
			return &VerdictError{Field: "/ref", Detail: err.Error()}
		}
	}
	return nil
}

// validateFullSHA refuses anything that is not a complete, unambiguous,
// lowercase 40-hex commit id. Each rejected shape is one this cycle actually
// produced or could: "" (a verdict naming nothing), "HEAD" and a branch name
// (a moving target that means something different tomorrow), an abbreviation
// (unambiguous today, not necessarily later), and mixed case (the same commit
// spelled two ways compares unequal).
func validateFullSHA(sha string) error {
	fail := func(detail string) error {
		return &VerdictError{
			Field: "/commit_sha",
			Detail: detail + "; a verdict that does not name what it tested is not evidence " +
				"(give the full 40-character lowercase hex commit id)",
		}
	}
	if sha == "" {
		return fail("no commit was named")
	}
	if len(sha) != 40 {
		return fail(fmt.Sprintf("%q is %d characters", sha, len(sha)))
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fail(fmt.Sprintf("%q contains %q, which is not lowercase hex", sha, r))
		}
	}
	return nil
}

// Record composes the derived ledger record, or refuses.
//
// Nothing in the returned record is anybody's summary: the verdict label is
// computed from the exit code, the reason names the suite and the commit
// verbatim, and there is no field a caller could use to say "and it looks
// fine to me".
func (v SuiteVerdict) Record() (ledger.Record, error) {
	if err := v.Validate(); err != nil {
		return ledger.Record{}, err
	}

	evaluated := v.EvaluatedAt
	if evaluated.IsZero() {
		evaluated = time.Now()
	}

	data := map[string]any{
		"verdict":           verdictLabel(v.ExitCode),
		"reason":            v.reason(),
		"suite":             v.Suite,
		"exit_code":         v.ExitCode,
		"commit_sha":        v.CommitSHA,
		"collection_method": VerdictCollectionMethod,
		"evaluated_at":      evaluated.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	// Absent keys rather than empty ones, the same discipline
	// ledger.WithRationale states: "" would read as "a ref was recorded and
	// it was blank", where absence reads as "none was collected".
	if v.Ref != "" {
		data["ref"] = v.Ref
	}
	if len(v.Command) > 0 {
		data["command"] = v.Command
	}
	if v.EvidenceRecordID != "" {
		data["handover_evidence_ref"] = v.EvidenceRecordID
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("handover: encode suite verdict payload: %w", err)
	}

	rec := ledger.Record{
		RecordType: ledger.RecordReview,
		RunID:      v.RunID,
		NodeRunID:  ledger.NullableID(v.NodeRunID),
		AttemptID:  ledger.NullableID(v.AttemptID),
		Origin: ledger.Origin{
			Kind:          ledger.OriginValidator,
			ActorID:       v.ValidatorActorID,
			ActorRevision: v.ValidatorRevision,
		},
		Authority:      ledger.AuthorityDerived,
		SubjectRef:     ledger.NullableID(v.EvidenceRecordID),
		ProvenanceRefs: []string{},
		Data:           payload,
	}
	if v.EvidenceRecordID != "" {
		rec.ProvenanceRefs = []string{v.EvidenceRecordID}
	}
	return rec, nil
}

// verdictLabel mirrors internal/worker's acceptanceVerdictLabel: the review
// vocabulary's own confirm/reject strings, computed rather than accepted.
func verdictLabel(exitCode int) string {
	if exitCode == 0 {
		return "confirm"
	}
	return "reject"
}

// reason states exactly what the exit code establishes and, as importantly,
// what it does not. A green suite is not a review: it says these tests, as
// they exist in this commit, did not fail — which is silent about tests that
// were skipped, never written, or testing something else.
func (v SuiteVerdict) reason() string {
	outcome := fmt.Sprintf("exited %d", v.ExitCode)
	if v.ExitCode == 0 {
		outcome = "exited 0"
	}
	return fmt.Sprintf(
		"%q %s against commit %s. That exit code is the whole of this finding: it says the suite as it "+
			"exists in that commit did not fail, and says nothing about tests that were skipped, never "+
			"written, or whether the change is correct (PRD §10.4: derived, from a deterministic validator)",
		v.Suite, outcome, v.CommitSHA)
}
