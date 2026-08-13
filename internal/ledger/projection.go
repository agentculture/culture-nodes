package ledger

import (
	"context"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// ProjectionKind names one of the standard projections (PRD §10.9). Agents
// and UI views consume projections, not the whole ledger.
type ProjectionKind string

const (
	KindCurrentScope    ProjectionKind = "current_scope"
	KindConfirmedClaims ProjectionKind = "confirmed_claims"
	KindOpenAssumptions ProjectionKind = "open_assumptions_and_questions"
	KindReadyTasks      ProjectionKind = "ready_tasks"
	KindActiveTasks     ProjectionKind = "active_tasks"
	KindVerificationQ   ProjectionKind = "verification_queue"
	KindDecisionHistory ProjectionKind = "decision_history"
	KindEvidenceFor     ProjectionKind = "evidence_for_subject"
	KindDeliverySummary ProjectionKind = "delivery_summary"
)

// ProjectionKinds returns the standard projections in PRD §10.9 order.
func ProjectionKinds() []ProjectionKind {
	return []ProjectionKind{
		KindCurrentScope, KindConfirmedClaims, KindOpenAssumptions,
		KindReadyTasks, KindActiveTasks, KindVerificationQ,
		KindDecisionHistory, KindEvidenceFor, KindDeliverySummary,
	}
}

// Projection is the result of one projection over a record set.
//
// Every field is serialised, always, including the ones that are empty for
// this kind: a projection's shape does not change with its content, so two
// runs of the same projection are comparable byte for byte.
type Projection struct {
	Kind ProjectionKind `json:"kind"`
	// Subject is the reference a subject-scoped projection was asked
	// about, and "" for the ones that are not.
	Subject string `json:"subject"`
	// Items are the records the projection selects, ordered by id.
	Items []Record `json:"items"`
	// Summary is populated only by DeliverySummary.
	Summary *DeliveryCounts `json:"summary"`
	// Digest is the sha256 of this projection's canonical form with the
	// digest field itself removed — the same envelope-minus-digest rule a
	// record follows. Identical record sets produce an identical digest
	// whatever order the store returned them in, which is what makes a
	// projection digest comparable across processes and checkpointable.
	Digest string `json:"digest"`
}

// ComputeDigest returns the digest of this projection's content, ignoring
// whatever is currently in Digest.
func (p Projection) ComputeDigest() (string, error) {
	p.Digest = ""
	if p.Items == nil {
		p.Items = []Record{}
	}
	digest, err := contracts.DigestValue(p)
	if err != nil {
		return "", fmt.Errorf("ledger: digest %s projection: %w", p.Kind, err)
	}
	return digest, nil
}

// VerifyDigest reports whether the projection's stored digest still matches
// its content.
func (p Projection) VerifyDigest() error {
	want, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.Digest != want {
		return fmt.Errorf("ledger: %s projection digest mismatch: stored %s, computed %s", p.Kind, p.Digest, want)
	}
	return nil
}

// DeliveryCounts is the counted state of a run: what is claimed, what is
// confirmed, what is still open, and what completed without verification
// (PRD §10.9).
type DeliveryCounts struct {
	// RunID is the run these records belong to, or "" if they span more
	// than one.
	RunID string `json:"run_id"`
	// LiveRecords and SupersededRecords partition the input.
	LiveRecords       int `json:"live_records"`
	SupersededRecords int `json:"superseded_records"`
	// TasksByStatus counts live task records by execution status, and
	// TasksByAssurance by assurance state. The two axes are separate: an
	// actor drives execution to completed, but only acceptance checks, a
	// verifier, or an authorized review make assurance verified (§10.6).
	TasksByStatus    map[string]int `json:"tasks_by_status"`
	TasksByAssurance map[string]int `json:"tasks_by_assurance_state"`
	// CompletedUnverifiedTasks is the honest headline: work an actor
	// declared finished that nothing has verified.
	CompletedUnverifiedTasks int `json:"completed_unverified_tasks"`
	ConfirmedClaims          int `json:"confirmed_claims"`
	RejectedClaims           int `json:"rejected_claims"`
	UndecidedClaims          int `json:"undecided_claims"`
	OpenAssumptions          int `json:"open_assumptions"`
	OpenQuestions            int `json:"open_questions"`
	BlockingOpenQuestions    int `json:"blocking_open_questions"`
	EvidenceRecords          int `json:"evidence_records"`
	// EvidenceByCompleteness counts evidence by declared completeness.
	// Evidence that declares none is counted as "unstated" rather than
	// assumed complete.
	EvidenceByCompleteness map[string]int `json:"evidence_by_completeness"`
	ResultsAwaitingReview  int            `json:"results_awaiting_review"`
}

// Live returns the records that no other record in the set supersedes,
// ordered by id.
//
// Supersession is read from the replacing record's Supersedes pointer, which
// is the only place it is ever written: the replaced record is immutable and
// says nothing about having been replaced.
func Live(records []Record) []Record {
	superseded := make(map[string]bool, len(records))
	for _, rec := range records {
		if id := rec.Supersedes.String(); id != "" {
			superseded[id] = true
		}
	}

	out := make([]Record, 0, len(records))
	for _, rec := range records {
		if !superseded[rec.ID] {
			out = append(out, rec.Clone())
		}
	}
	sortByID(out)
	return out
}

// Superseded returns the records some other record in the set replaces,
// ordered by id. It is the complement of Live.
func Superseded(records []Record) []Record {
	superseding := make(map[string]bool, len(records))
	for _, rec := range records {
		if id := rec.Supersedes.String(); id != "" {
			superseding[id] = true
		}
	}

	out := make([]Record, 0)
	for _, rec := range records {
		if superseding[rec.ID] {
			out = append(out, rec.Clone())
		}
	}
	sortByID(out)
	return out
}

// reviewDecisions maps a record id to the authority of the live review record
// that decided it. Where several live reviews name the same target, the one
// with the highest id wins — ids are ULIDs, so that is the most recent
// decision.
func reviewDecisions(live []Record) map[string]Authority {
	out := make(map[string]Authority)
	for _, rec := range live {
		if rec.RecordType != RecordReview {
			continue
		}
		target := rec.SubjectRef.String()
		if target == "" {
			continue
		}
		switch rec.Authority {
		case AuthorityConfirmed, AuthorityRejected:
			out[target] = rec.Authority
		default:
			// A review record that is itself only proposed (a requested
			// revision, say) decides nothing.
		}
	}
	return out
}

// CurrentScope projects the announcements in force: what was requested and
// what the work is bounded to (PRD §10.1, §10.9).
func CurrentScope(records []Record) (Projection, error) {
	return finish(Projection{
		Kind:  KindCurrentScope,
		Items: selectType(Live(records), RecordAnnouncement),
	})
}

// ConfirmedClaims projects the claims a human review has confirmed.
//
// A claim's own authority is never confirmed — no producer may write that,
// and a review transaction appends a decision rather than rewriting its
// target — so confirmation is read from the live review records pointing at
// it. That indirection is the point: the claim stays exactly as its author
// proposed it, with the confirmation attributable to whoever made it.
func ConfirmedClaims(records []Record) (Projection, error) {
	live := Live(records)
	decisions := reviewDecisions(live)

	items := make([]Record, 0)
	for _, rec := range live {
		if rec.RecordType == RecordClaim && decisions[rec.ID] == AuthorityConfirmed {
			items = append(items, rec)
		}
	}
	sortByID(items)
	return finish(Projection{Kind: KindConfirmedClaims, Items: items})
}

// OpenAssumptionsAndQuestions projects the premises still riding on trust and
// the questions still unanswered: live assumptions and questions that no
// review has decided, and questions that carry no answer.
func OpenAssumptionsAndQuestions(records []Record) (Projection, error) {
	live := Live(records)
	decisions := reviewDecisions(live)

	items := make([]Record, 0)
	for _, rec := range live {
		if rec.RecordType != RecordAssumption && rec.RecordType != RecordQuestion {
			continue
		}
		if _, decided := decisions[rec.ID]; decided {
			continue
		}
		if rec.RecordType == RecordQuestion {
			answered, err := questionIsAnswered(rec)
			if err != nil {
				return Projection{}, err
			}
			if answered {
				continue
			}
		}
		items = append(items, rec)
	}
	sortByID(items)
	return finish(Projection{Kind: KindOpenAssumptions, Items: items})
}

func questionIsAnswered(rec Record) (bool, error) {
	data, err := rec.DataMap()
	if err != nil {
		return false, err
	}
	return dataString(data, "answer") != "", nil
}

// ReadyTasks projects the live task records whose execution status is ready.
//
// A task's status changes by appending a superseding task record, so the
// current status of a task is the status of its live record and nothing else
// has to be reconciled.
func ReadyTasks(records []Record) (Projection, error) {
	items, err := tasksWithStatus(records, "ready")
	if err != nil {
		return Projection{}, err
	}
	return finish(Projection{Kind: KindReadyTasks, Items: items})
}

// ActiveTasks projects the live task records currently in an actor's hands —
// claimed or running. Blocked tasks are deliberately not active: something is
// in their way, and calling that active would hide it.
func ActiveTasks(records []Record) (Projection, error) {
	items, err := tasksWithStatus(records, "claimed", "running")
	if err != nil {
		return Projection{}, err
	}
	return finish(Projection{Kind: KindActiveTasks, Items: items})
}

func tasksWithStatus(records []Record, statuses ...string) ([]Record, error) {
	wanted := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		wanted[s] = true
	}

	items := make([]Record, 0)
	for _, rec := range Live(records) {
		if rec.RecordType != RecordTask {
			continue
		}
		data, err := rec.DataMap()
		if err != nil {
			return nil, err
		}
		if wanted[dataString(data, "status")] {
			items = append(items, rec)
		}
	}
	sortByID(items)
	return items, nil
}

// VerificationQueue projects what is waiting on verification: tasks an actor
// drove to completed whose assurance state is not yet verified or rejected,
// and results no review has decided.
//
// An actor may mark execution completed by producing a result. Only
// acceptance checks, a verifier, or an authorized review can make the
// assurance state verified (PRD §10.6) — everything in this queue is the gap
// between those two facts.
func VerificationQueue(records []Record) (Projection, error) {
	live := Live(records)
	decisions := reviewDecisions(live)

	items := make([]Record, 0)
	for _, rec := range live {
		switch rec.RecordType {
		case RecordTask:
			data, err := rec.DataMap()
			if err != nil {
				return Projection{}, err
			}
			if dataString(data, "status") != "completed" {
				continue
			}
			switch dataString(data, "assurance_state") {
			case "verified", "rejected":
				continue
			}
			items = append(items, rec)
		case RecordResult:
			if _, decided := decisions[rec.ID]; !decided {
				items = append(items, rec)
			}
		}
	}
	sortByID(items)
	return finish(Projection{Kind: KindVerificationQ, Items: items})
}

// DecisionHistory projects the live decision records in id order, which for
// ULIDs is the order they were taken.
//
// A superseded decision is not history here: it was replaced, and the record
// that replaced it names it, so the chain remains auditable in the raw ledger
// while the projection shows only what currently holds.
func DecisionHistory(records []Record) (Projection, error) {
	return finish(Projection{
		Kind:  KindDecisionHistory,
		Items: selectType(Live(records), RecordDecision),
	})
}

// EvidenceForSubject projects live evidence records. A non-empty ref selects
// the evidence bearing on that one reference — evidence whose subject is the
// reference, and evidence that names it in its provenance; both are how a
// task or result comes to be supported. An empty ref is unscoped and selects
// every live evidence record, which is what the `evidence` input binding asks
// for (internal/worker's projectionKindFor): a run's evidence, not one
// reference's.
func EvidenceForSubject(records []Record, ref string) (Projection, error) {
	items := make([]Record, 0)
	for _, rec := range Live(records) {
		if rec.RecordType != RecordEvidence {
			continue
		}
		if ref == "" || rec.SubjectRef.String() == ref || containsString(rec.ProvenanceRefs, ref) {
			items = append(items, rec)
		}
	}
	sortByID(items)
	return finish(Projection{Kind: KindEvidenceFor, Subject: ref, Items: items})
}

// DeliverySummary projects the counted state of the run.
func DeliverySummary(records []Record) (Projection, error) {
	live := Live(records)
	decisions := reviewDecisions(live)

	summary := &DeliveryCounts{
		RunID:                  singleRunID(records),
		LiveRecords:            len(live),
		SupersededRecords:      len(records) - len(live),
		TasksByStatus:          map[string]int{},
		TasksByAssurance:       map[string]int{},
		EvidenceByCompleteness: map[string]int{},
	}

	for _, rec := range live {
		data, err := rec.DataMap()
		if err != nil {
			return Projection{}, err
		}
		switch rec.RecordType {
		case RecordTask:
			summarizeTask(summary, data)
		case RecordClaim:
			switch decisions[rec.ID] {
			case AuthorityConfirmed:
				summary.ConfirmedClaims++
			case AuthorityRejected:
				summary.RejectedClaims++
			default:
				summary.UndecidedClaims++
			}
		case RecordAssumption:
			if _, decided := decisions[rec.ID]; !decided {
				summary.OpenAssumptions++
			}
		case RecordQuestion:
			if _, decided := decisions[rec.ID]; decided {
				continue
			}
			if dataString(data, "answer") != "" {
				continue
			}
			summary.OpenQuestions++
			if dataBool(data, "blocking") {
				summary.BlockingOpenQuestions++
			}
		case RecordEvidence:
			summary.EvidenceRecords++
			completeness := dataString(data, "completeness")
			if completeness == "" {
				completeness = "unstated"
			}
			summary.EvidenceByCompleteness[completeness]++
		case RecordResult:
			if _, decided := decisions[rec.ID]; !decided {
				summary.ResultsAwaitingReview++
			}
		}
	}

	return finish(Projection{Kind: KindDeliverySummary, Items: []Record{}, Summary: summary})
}

func summarizeTask(summary *DeliveryCounts, data map[string]any) {
	status := dataString(data, "status")
	if status == "" {
		status = "unstated"
	}
	summary.TasksByStatus[status]++

	assurance := dataString(data, "assurance_state")
	if assurance == "" {
		assurance = "unstated"
	}
	summary.TasksByAssurance[assurance]++

	if status == "completed" && assurance != "verified" {
		summary.CompletedUnverifiedTasks++
	}
}

// Project dispatches to the named projection. subject is used only by
// KindEvidenceFor and ignored by the rest.
func Project(records []Record, kind ProjectionKind, subject string) (Projection, error) {
	switch kind {
	case KindCurrentScope:
		return CurrentScope(records)
	case KindConfirmedClaims:
		return ConfirmedClaims(records)
	case KindOpenAssumptions:
		return OpenAssumptionsAndQuestions(records)
	case KindReadyTasks:
		return ReadyTasks(records)
	case KindActiveTasks:
		return ActiveTasks(records)
	case KindVerificationQ:
		return VerificationQueue(records)
	case KindDecisionHistory:
		return DecisionHistory(records)
	case KindEvidenceFor:
		return EvidenceForSubject(records, subject)
	case KindDeliverySummary:
		return DeliverySummary(records)
	default:
		return Projection{}, fmt.Errorf("ledger: unknown projection %q", kind)
	}
}

// ProjectRun reads a run's records and projects them. The projection itself
// stays a pure function of the records; this is only the read.
func (l *Ledger) ProjectRun(ctx context.Context, runID string, kind ProjectionKind, subject string) (Projection, error) {
	records, err := l.store.RunRecords(ctx, runID)
	if err != nil {
		return Projection{}, err
	}
	return Project(records, kind, subject)
}

func selectType(records []Record, recordType RecordType) []Record {
	out := make([]Record, 0)
	for _, rec := range records {
		if rec.RecordType == recordType {
			out = append(out, rec)
		}
	}
	sortByID(out)
	return out
}

// finish stamps the projection with the digest of its own content.
func finish(p Projection) (Projection, error) {
	if p.Items == nil {
		p.Items = []Record{}
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		return Projection{}, err
	}
	p.Digest = digest
	return p, nil
}

func sortByID(records []Record) {
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// singleRunID returns the run every record belongs to, or "" when they span
// more than one — the summary says nothing it cannot support.
func singleRunID(records []Record) string {
	runID := ""
	for _, rec := range records {
		if rec.RunID == "" {
			continue
		}
		if runID == "" {
			runID = rec.RunID
			continue
		}
		if runID != rec.RunID {
			return ""
		}
	}
	return runID
}
