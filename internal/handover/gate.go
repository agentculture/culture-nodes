package handover

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The two gate records SuiteVerdict cannot compose (task t16, issue #101).
//
// verdict.go answers "the suite ran and exited N". That is the whole finding
// for a gate that RAN. A merge gate expressed as a node has two more things to
// say, and neither of them is an exit code:
//
//  1. **A gate that did not measure the change.** An instrument that does not
//     reach a changed tree — a coverage run that never sees Go, a Sonar
//     analysis that does not exist for the candidate, a `go` binary that is
//     not on this host's PATH — produced nothing. There is no exit code to
//     record, and the temptation the whole gate exists to resist is to record
//     0: a suite that never ran and a suite that passed are the same green
//     tick, and the difference is invisible a week later. So a not-applicable
//     gate gets its own record, with NO `verdict` key at all (the absence is
//     the statement), a reason from a closed vocabulary, and the paths the
//     instrument did not cover.
//
//  2. **The aggregate.** Per-gate records answer "what did this instrument
//     say". Nobody merges on one of those; they merge on "did the gate pass",
//     which is a computation OVER them. Computing it here — from the per-gate
//     statuses, not from a caller-supplied summary — is what stops an empty
//     scan from looking green: the counts are derived, and a report with no
//     applicable gate at all cannot produce `gates_passed` no matter what its
//     author believed.
//
// Both are `derived`, validator-origin records for the same reason the suite
// verdict is (PRD §10.4): a deterministic validator's own output. Neither can
// be `proposed` — that is what an agent's word is worth — and neither is
// `confirmed`, which only a human review transaction produces. The merge
// decision itself stays a person's, and it is made ON these records rather
// than on a recollection of a tick.

// GateCollectionMethod names how the aggregate was produced, distinguishing it
// from VerdictCollectionMethod (`suite_exit_status`, one suite's own exit) and
// from CollectionMethod (`git_ref_fetch`, what a ref contains). The aggregate
// measured no process at all: it is arithmetic over already-recorded findings.
const GateCollectionMethod = "gate_result_aggregation"

// NotApplicableCollectionMethod names how a not-applicable finding was made:
// the gate program compared its declared instruments against the changed-file
// set (and against what this host can actually run) and found no overlap. It
// ran no suite, which is exactly the fact being recorded.
const NotApplicableCollectionMethod = "gate_applicability_scan"

// The per-gate statuses. `not_applicable` is a first-class third answer, never
// a spelling of `passed`.
const (
	GateStatusPassed        = "passed"
	GateStatusFailed        = "failed"
	GateStatusNotApplicable = "not_applicable"
)

// The gate node's domain outcomes, computed by GateResults.Outcome and routed
// by the workflow. They are the names examples/merge-gate/workflow.yaml
// declares and internal/worker's gate vocabulary maps exit codes onto; the
// three lists are one contract and none of them may drift alone.
const (
	OutcomeGatesPassed           = "gates_passed"
	OutcomeChangesRequired       = "changes_required"
	OutcomeMeasurementIncomplete = "measurement_incomplete"
)

// The closed vocabulary of reasons a declared gate did not measure a change,
// taken from the t18 handover-validator design.
//
// It is closed on purpose. A free-text reason is a reason nobody can query,
// and the whole point of recording a not-applicable gate is that a reader — or
// issue #88, later — can ask "which trees does no instrument reach yet" and
// get an answer instead of a paragraph.
const (
	// ReasonInstrumentNotReachingTree: the instrument exists and ran, or
	// could have, but its configured scope does not include the changed
	// files. Today's coverage and complexity instruments over `internal/`,
	// the adapters and `web/` are this (issue #88).
	ReasonInstrumentNotReachingTree = "instrument_not_reaching_tree"
	// ReasonNoTestInstrument: the changed tree declares no suite at all. Not
	// zero failures — no measurement.
	ReasonNoTestInstrument = "no_test_instrument"
	// ReasonInstrumentUnavailable: the instrument itself could not be used
	// HERE — a toolchain absent from this host, or an analysis that does not
	// exist for the candidate commit. This is the reason that makes the whole
	// report `measurement_incomplete`: the instrument was expected to reach
	// the change and did not get the chance.
	ReasonInstrumentUnavailable = "instrument_unavailable"
	// ReasonNoSourceFiles: the change touches no file this instrument is
	// defined over — a docs-only or config-only change. The only reason that
	// may name no uncovered path, because there are none.
	ReasonNoSourceFiles = "no_source_files"
)

// notApplicableReasons is the vocabulary as a set, with whether a reason may
// omit the uncovered-path list.
var notApplicableReasons = map[string]bool{
	ReasonInstrumentNotReachingTree: false,
	ReasonNoTestInstrument:          false,
	ReasonInstrumentUnavailable:     false,
	ReasonNoSourceFiles:             true,
}

// NotApplicableReasons returns the closed reason vocabulary, sorted. Exported
// so an API layer can name the accepted values in its own refusal rather than
// duplicating the list.
func NotApplicableReasons() []string {
	out := make([]string, 0, len(notApplicableReasons))
	for reason := range notApplicableReasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

// GateNotApplicable is one declared gate that measured nothing, and why.
//
// Everything about it mirrors SuiteVerdict except the finding: same subject
// rules, same validator identity rules, same full-sha refusal. What differs is
// that there is no exit code, and no field a caller could use to imply the
// gate was satisfied anyway.
type GateNotApplicable struct {
	RunID     string
	NodeRunID string
	AttemptID string

	// Gate is the declared gate's name in the workflow's matrix ("go-test",
	// "coverage"). Suite is what WOULD have run, in a spelling a reader could
	// re-run; it may be empty when nothing was declared to run at all.
	Gate    string
	Suite   string
	Command []string

	// Instrument identifies the tool that did not reach the change
	// ("coverage.py", "go test", "npm run build"), with its version when one
	// could be read. A version is often absent precisely BECAUSE the
	// instrument was unavailable, so absence is expected rather than a fault.
	Instrument        string
	InstrumentVersion string

	// Reason is one of the closed vocabulary above.
	Reason string
	// UncoveredPaths are the changed files this instrument did not cover.
	// Required for every reason but ReasonNoSourceFiles: "not applicable"
	// without a subject is indistinguishable from "not bothered".
	UncoveredPaths []string
	// ChangedFilesConsidered is the changed-file set the applicability
	// decision was made against, so a reader can check the decision rather
	// than take it.
	ChangedFilesConsidered []string

	CommitSHA string
	Ref       string

	ValidatorActorID  string
	ValidatorRevision string

	EvidenceRecordID string
	EvaluatedAt      time.Time
}

// Validate applies every refusal without composing anything.
func (g GateNotApplicable) Validate() error {
	if strings.TrimSpace(g.RunID) == "" {
		return &VerdictError{Field: "/run_id", Detail: "a gate result must name the run whose handover it judged"}
	}
	if strings.TrimSpace(g.Gate) == "" {
		return &VerdictError{
			Field:  "/gate",
			Detail: "a gate result must name the declared gate it is about; an unnamed gate cannot be counted",
		}
	}
	if strings.TrimSpace(g.ValidatorActorID) == "" {
		return &VerdictError{
			Field: "/validator_actor_id",
			Detail: "a derived record needs an identified deterministic producer (PRD §10.4); " +
				"an anonymous validator attests to nothing",
		}
	}
	allowEmpty, known := notApplicableReasons[g.Reason]
	if !known {
		return &VerdictError{
			Field: "/reason",
			Detail: fmt.Sprintf("%q is not one of the recorded not-applicable reasons (%s); "+
				"a free-text reason is one no later query can group on",
				g.Reason, strings.Join(NotApplicableReasons(), ", ")),
		}
	}
	if !allowEmpty && len(g.UncoveredPaths) == 0 {
		return &VerdictError{
			Field: "/uncovered_paths",
			Detail: fmt.Sprintf("reason %q must name the changed files the instrument did not cover; "+
				"a not-applicable gate that names nothing is indistinguishable from a gate nobody ran, "+
				"and would read as a pass by omission", g.Reason),
		}
	}
	if err := validateFullSHA(g.CommitSHA); err != nil {
		return err
	}
	if g.Ref != "" {
		if err := ValidateRef(g.Ref); err != nil {
			return &VerdictError{Field: "/ref", Detail: err.Error()}
		}
	}
	return nil
}

// Record composes the derived ledger record, or refuses.
//
// There is deliberately no `verdict` key. The review vocabulary has three
// values — confirm, reject, changes_requested — and this finding is none of
// them: no verdict was reached, because nothing was measured. Writing one
// anyway would make an unmeasured gate sortable beside measured ones, which is
// the confusion the record exists to prevent.
func (g GateNotApplicable) Record() (ledger.Record, error) {
	if err := g.Validate(); err != nil {
		return ledger.Record{}, err
	}

	evaluated := g.EvaluatedAt
	if evaluated.IsZero() {
		evaluated = time.Now()
	}

	data := map[string]any{
		"gate":              g.Gate,
		"gate_status":       GateStatusNotApplicable,
		"reason":            g.Reason,
		"rationale":         g.rationale(),
		"uncovered_paths":   append([]string(nil), g.UncoveredPaths...),
		"commit_sha":        g.CommitSHA,
		"collection_method": NotApplicableCollectionMethod,
		"evaluated_at":      evaluated.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if data["uncovered_paths"] == nil {
		data["uncovered_paths"] = []string{}
	}
	for key, value := range map[string]string{
		"suite":              g.Suite,
		"instrument":         g.Instrument,
		"instrument_version": g.InstrumentVersion,
		"ref":                g.Ref,
	} {
		if value != "" {
			data[key] = value
		}
	}
	if len(g.Command) > 0 {
		data["command"] = g.Command
	}
	if len(g.ChangedFilesConsidered) > 0 {
		data["changed_files_considered"] = g.ChangedFilesConsidered
	}
	if g.EvidenceRecordID != "" {
		data["handover_evidence_ref"] = g.EvidenceRecordID
	}

	return derivedGateRecord(data, gateRecordIdentity{
		RunID:             g.RunID,
		NodeRunID:         g.NodeRunID,
		AttemptID:         g.AttemptID,
		ValidatorActorID:  g.ValidatorActorID,
		ValidatorRevision: g.ValidatorRevision,
		EvidenceRecordID:  g.EvidenceRecordID,
	})
}

func (g GateNotApplicable) rationale() string {
	subject := g.Instrument
	if subject == "" {
		subject = g.Gate
	}
	return fmt.Sprintf(
		"%q measured nothing against commit %s (%s), so this gate reached no verdict. "+
			"It is NOT a pass: %d changed file(s) it would have covered were not covered, and an "+
			"unmeasured gate counted as a satisfied one is the false green this record exists to prevent "+
			"(PRD §10.4: derived, from a deterministic validator)",
		subject, g.CommitSHA, g.Reason, len(g.UncoveredPaths))
}

// GateResult is one declared gate's status, as the aggregate reads it.
//
// It carries no numbers of its own: the value, threshold and instrument live
// on the per-gate record this refers to. The aggregate's job is counting, and
// counting needs exactly the status, the reason a not-applicable gate gave,
// and a pointer back to the record.
type GateResult struct {
	Gate     string `json:"gate"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	RecordID string `json:"record_id,omitempty"`
}

// GateResults is the whole report's per-gate statuses.
type GateResults []GateResult

// GateCounts is the four-number summary the aggregate must carry so an empty
// scan cannot look green.
//
// Applicable is passed+failed — the gates that actually measured something.
// Declared is all four, and reporting it alongside is what makes "of the seven
// gates this workflow declares, two measured anything" a readable sentence
// rather than an inference.
type GateCounts struct {
	Declared      int `json:"declared_gate_count"`
	Applicable    int `json:"applicable_gate_count"`
	Passed        int `json:"passed_gate_count"`
	Failed        int `json:"failed_gate_count"`
	NotApplicable int `json:"not_applicable_gate_count"`
}

// Counts summarises the results.
func (r GateResults) Counts() GateCounts {
	counts := GateCounts{Declared: len(r)}
	for _, result := range r {
		switch result.Status {
		case GateStatusPassed:
			counts.Passed++
		case GateStatusFailed:
			counts.Failed++
		case GateStatusNotApplicable:
			counts.NotApplicable++
		}
	}
	counts.Applicable = counts.Passed + counts.Failed
	return counts
}

// Outcome computes the gate node's domain outcome from the per-gate statuses.
//
// The order of the rules is the whole design, and each is a thing that would
// otherwise be a false green:
//
//  1. **A failing gate is `changes_required`.** A threshold miss is a domain
//     answer that follows an edge (PRD §3.4), never an engine failure and
//     never a retry.
//  2. **An UNAVAILABLE instrument is `measurement_incomplete`.** This is the
//     one not-applicable reason that means "this should have been measured
//     here and was not" — a missing toolchain, an analysis that does not
//     exist. Letting it pass would mean a lane without Go reports a green Go
//     gate; letting it fail would manufacture a defect nobody observed.
//  3. **A report with no applicable gate at all is `measurement_incomplete`.**
//     Zero failures out of zero measurements is arithmetically a pass and
//     substantively nothing. This is the empty-scan rule.
//  4. Otherwise `gates_passed`.
//
// Rules 2 and 3 deliberately do NOT fire for `instrument_not_reaching_tree`,
// `no_test_instrument` or `no_source_files` on their own. Those are honest,
// recorded gaps in the instrument matrix (issue #88), and treating today's
// known coverage boundary as a per-run measurement failure would make every
// run of the gate incomplete and the signal worthless. They are counted,
// named, and visible in the aggregate — which is what criterion 4 asks for —
// while rule 3 still catches the case where they are ALL there is.
func (r GateResults) Outcome() string {
	counts := r.Counts()
	if counts.Failed > 0 {
		return OutcomeChangesRequired
	}
	for _, result := range r {
		if result.Status == GateStatusNotApplicable && result.Reason == ReasonInstrumentUnavailable {
			return OutcomeMeasurementIncomplete
		}
	}
	if counts.Applicable == 0 {
		return OutcomeMeasurementIncomplete
	}
	return OutcomeGatesPassed
}

// gateOutcomeExitCodes is the gate program's published exit-status contract,
// mirrored from internal/worker/code.go's gateExitCodes. The two live in
// different packages (the worker must not import the ledger's handover
// vocabulary to resolve an outcome name) and are kept in step by
// TestGateExitCodesMatchTheWorkerVocabulary.
var gateOutcomeExitCodes = map[string]int{
	OutcomeGatesPassed:           0,
	OutcomeChangesRequired:       1,
	OutcomeMeasurementIncomplete: 2,
}

// GateExitCode returns the exit code a gate program uses to report outcome,
// and whether outcome is one this contract knows.
func GateExitCode(outcome string) (int, bool) {
	code, ok := gateOutcomeExitCodes[outcome]
	return code, ok
}

// reviewVerdicts maps a gate outcome onto the review vocabulary's own three
// values, where one applies. `measurement_incomplete` is deliberately absent:
// no verdict was reached, and the record says so by carrying no `verdict` key.
var reviewVerdicts = map[string]string{
	OutcomeGatesPassed:     "confirm",
	OutcomeChangesRequired: "changes_requested",
}

// GateAggregate is the whole gate's finding, computed over its parts.
//
// Note what it does NOT have: a field for the outcome, or for any of the four
// counts. Both are computed from Results, so a caller cannot report six passes
// over four gates, or a green aggregate over a report that measured nothing.
type GateAggregate struct {
	RunID     string
	NodeRunID string
	AttemptID string

	// Results are the per-gate statuses this aggregate is computed over.
	Results GateResults

	// BaseSHA and CommitSHA pin what was compared. ChangedFiles is the set
	// the applicability decisions were made against — the aggregate carries
	// it because "which files did you consider" is the first question anyone
	// asks of a not-applicable count.
	BaseSHA      string
	CommitSHA    string
	Ref          string
	ChangedFiles []string

	ValidatorActorID  string
	ValidatorRevision string

	EvidenceRecordID string
	EvaluatedAt      time.Time
}

// Validate applies every refusal without composing anything.
func (a GateAggregate) Validate() error {
	if strings.TrimSpace(a.RunID) == "" {
		return &VerdictError{Field: "/run_id", Detail: "an aggregate must name the run whose gates it summarises"}
	}
	if len(a.Results) == 0 {
		return &VerdictError{
			Field: "/gates",
			Detail: "an aggregate over zero gates is not a finding; a gate report must carry at least one " +
				"declared gate, or there is nothing for the counts to be counts OF",
		}
	}
	if strings.TrimSpace(a.ValidatorActorID) == "" {
		return &VerdictError{
			Field: "/validator_actor_id",
			Detail: "a derived record needs an identified deterministic producer (PRD §10.4); " +
				"an anonymous validator attests to nothing",
		}
	}
	for i, result := range a.Results {
		switch result.Status {
		case GateStatusPassed, GateStatusFailed, GateStatusNotApplicable:
		default:
			return &VerdictError{
				Field: fmt.Sprintf("/gates/%d/status", i),
				Detail: fmt.Sprintf("%q is not a gate status (%s, %s, %s)",
					result.Status, GateStatusPassed, GateStatusFailed, GateStatusNotApplicable),
			}
		}
		if strings.TrimSpace(result.Gate) == "" {
			return &VerdictError{
				Field:  fmt.Sprintf("/gates/%d/gate", i),
				Detail: "every counted gate must be named, or the counts cannot be checked against the report",
			}
		}
	}
	if err := validateFullSHA(a.CommitSHA); err != nil {
		return err
	}
	if a.Ref != "" {
		if err := ValidateRef(a.Ref); err != nil {
			return &VerdictError{Field: "/ref", Detail: err.Error()}
		}
	}
	return nil
}

// Record composes the derived aggregate record, or refuses.
func (a GateAggregate) Record() (ledger.Record, error) {
	if err := a.Validate(); err != nil {
		return ledger.Record{}, err
	}

	evaluated := a.EvaluatedAt
	if evaluated.IsZero() {
		evaluated = time.Now()
	}
	counts := a.Results.Counts()
	outcome := a.Results.Outcome()

	data := map[string]any{
		"gate_status":               outcome,
		"rationale":                 a.rationale(counts, outcome),
		"gates":                     a.Results,
		"declared_gate_count":       counts.Declared,
		"applicable_gate_count":     counts.Applicable,
		"passed_gate_count":         counts.Passed,
		"failed_gate_count":         counts.Failed,
		"not_applicable_gate_count": counts.NotApplicable,
		"commit_sha":                a.CommitSHA,
		"collection_method":         GateCollectionMethod,
		"evaluated_at":              evaluated.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if verdict, ok := reviewVerdicts[outcome]; ok {
		data["verdict"] = verdict
	}
	if a.BaseSHA != "" {
		data["base_sha"] = a.BaseSHA
	}
	if a.Ref != "" {
		data["ref"] = a.Ref
	}
	if len(a.ChangedFiles) > 0 {
		data["changed_files"] = a.ChangedFiles
	}
	if a.EvidenceRecordID != "" {
		data["handover_evidence_ref"] = a.EvidenceRecordID
	}

	refs := make([]string, 0, len(a.Results))
	for _, result := range a.Results {
		if result.RecordID != "" {
			refs = append(refs, result.RecordID)
		}
	}
	if len(refs) > 0 {
		data["gate_record_refs"] = refs
	}

	record, err := derivedGateRecord(data, gateRecordIdentity{
		RunID:             a.RunID,
		NodeRunID:         a.NodeRunID,
		AttemptID:         a.AttemptID,
		ValidatorActorID:  a.ValidatorActorID,
		ValidatorRevision: a.ValidatorRevision,
		EvidenceRecordID:  a.EvidenceRecordID,
	})
	if err != nil {
		return ledger.Record{}, err
	}
	// The aggregate's provenance is every record it counted, plus the
	// handover evidence when one was measured. That is what makes it a
	// derivation a reader can re-perform rather than a summary they have to
	// believe.
	record.ProvenanceRefs = append(record.ProvenanceRefs, refs...)
	return record, nil
}

func (a GateAggregate) rationale(counts GateCounts, outcome string) string {
	return fmt.Sprintf(
		"%d of %d declared gate(s) measured commit %s: %d passed, %d failed, %d not applicable. "+
			"Computed outcome %q. The not-applicable gates are NOT passes — they are gates that measured "+
			"nothing, each naming the files it did not cover, and an aggregate over zero applicable gates "+
			"is %q rather than green (PRD §10.4: derived, from a deterministic validator)",
		counts.Applicable, counts.Declared, a.CommitSHA,
		counts.Passed, counts.Failed, counts.NotApplicable, outcome, OutcomeMeasurementIncomplete)
}

// gateRecordIdentity is the placement and provenance every gate record shares.
type gateRecordIdentity struct {
	RunID             string
	NodeRunID         string
	AttemptID         string
	ValidatorActorID  string
	ValidatorRevision string
	EvidenceRecordID  string
}

// derivedGateRecord composes the one record shape both gate records use: a
// `review` record, validator origin, `derived` authority, subject and
// provenance pointing at the measured handover when there is one.
//
// It is the same shape SuiteVerdict.Record composes, and deliberately so —
// three record types for one gate would mean three things to query.
func derivedGateRecord(data map[string]any, id gateRecordIdentity) (ledger.Record, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("handover: encode gate record payload: %w", err)
	}

	rec := ledger.Record{
		RecordType: ledger.RecordReview,
		RunID:      id.RunID,
		NodeRunID:  ledger.NullableID(id.NodeRunID),
		AttemptID:  ledger.NullableID(id.AttemptID),
		Origin: ledger.Origin{
			Kind:          ledger.OriginValidator,
			ActorID:       id.ValidatorActorID,
			ActorRevision: id.ValidatorRevision,
		},
		Authority:      ledger.AuthorityDerived,
		SubjectRef:     ledger.NullableID(id.EvidenceRecordID),
		ProvenanceRefs: []string{},
		Data:           payload,
	}
	if id.EvidenceRecordID != "" {
		rec.ProvenanceRefs = []string{id.EvidenceRecordID}
	}
	return rec, nil
}
