package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Tx is the record-and-review surface the ledger runtime writes through. A
// Tx is either the store itself or one database transaction on it; the
// runtime cannot tell the difference, which is what lets a review transaction
// be all-or-nothing without the policy code knowing about transactions.
type Tx interface {
	// InsertRecord appends one record. Implementations must reject a
	// duplicate id rather than overwrite: records are immutable.
	InsertRecord(ctx context.Context, rec Record) error
	// GetRecord returns one record by id, or ErrRecordNotFound.
	GetRecord(ctx context.Context, id string) (Record, error)
	// RunRecords returns every record of a run, ordered by id.
	RunRecords(ctx context.Context, runID string) ([]Record, error)
	// LedgerVersion returns the number of records appended to a run. See
	// the package doc for why a count is the version.
	LedgerVersion(ctx context.Context, runID string) (int64, error)
	// LiveSupersessors returns the records that supersede recordID and are
	// not themselves superseded.
	LiveSupersessors(ctx context.Context, recordID string) ([]Record, error)
	// Lock takes a transaction-scoped advisory lock on key, serialising
	// concurrent writers that would otherwise read the same state and both
	// act on it. Implementations that are not inside a transaction must
	// return an error rather than take a lock they cannot scope.
	Lock(ctx context.Context, key string) error
	// InsertReviewRequest stores a new, uncommitted review request.
	InsertReviewRequest(ctx context.Context, req ReviewRequest) error
	// GetReviewRequest returns one review request, or ErrReviewNotFound.
	GetReviewRequest(ctx context.Context, id string) (ReviewRequest, error)
	// MarkReviewCommitted flips a review request from requested to
	// committed, reporting whether it was the call that did so. A false
	// return means some other commit got there first.
	MarkReviewCommitted(ctx context.Context, id string) (bool, error)
	// ActorKind returns the registered kind of an actor ("human", "agent",
	// "runner", …), or ErrActorNotFound when no actor has that id.
	//
	// It is on this interface because CommitReview stamps the reviewer as
	// the human origin of the review records it appends, and an origin the
	// ledger asserts on the caller's behalf is a claim it has to be able to
	// check. See the reviewer guard in CommitReview.
	ActorKind(ctx context.Context, actorID string) (string, error)
}

// ActorKindHuman is the registered actor kind a reviewer must have. It is the
// same vocabulary POST /v1alpha1/actors accepts, and it deliberately reads as
// a string rather than as an OriginKind: an actor's kind is a fact about the
// registry, and OriginKind is what a record asserts about itself.
const ActorKindHuman = "human"

// Store is a Tx that can also open one.
type Store interface {
	Tx
	// InTx runs fn inside a single transaction. When fn returns an error,
	// nothing fn wrote is applied.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// ReviewStatus is the lifecycle of a review request. A request is
// non-authoritative until it is acted upon (PRD §10.8).
type ReviewStatus string

const (
	// ReviewRequested is a review that has been asked for and not yet
	// acted upon. It carries no authority.
	ReviewRequested ReviewStatus = "requested"
	// ReviewCommitted is a review whose decisions have been appended.
	ReviewCommitted ReviewStatus = "committed"
)

// ReviewRequest is a batch of records placed in front of a reviewer,
// together with the ledger version and frame checksum it was read at
// (PRD §10.8).
type ReviewRequest struct {
	ID string
	// RunID scopes the review; ledger version is per run.
	RunID string
	// ReviewerActorID is the human the review is bound to. CommitReview
	// stamps it as the origin of the review records it appends.
	ReviewerActorID string
	// LedgerVersion is the run's record count when the request was made.
	LedgerVersion int64
	// FrameChecksum is the digest of the content digests of the reviewed
	// records, in id order — what the reviewer actually looked at.
	FrameChecksum string
	// Status is ReviewRequested until CommitReview applies the batch.
	Status ReviewStatus
	// RecordIDs are the records under review, sorted and deduplicated.
	RecordIDs []string
	CreatedAt time.Time
}

// Clone returns a deep copy.
func (r ReviewRequest) Clone() ReviewRequest {
	out := r
	if r.RecordIDs != nil {
		out.RecordIDs = append([]string(nil), r.RecordIDs...)
	}
	return out
}

// Verdict is a per-record review decision.
type Verdict string

const (
	// VerdictConfirm accepts the record, producing a confirmed review
	// record that references it.
	VerdictConfirm Verdict = "confirm"
	// VerdictReject refuses the record, producing a rejected review record
	// that references it.
	VerdictReject Verdict = "reject"
)

// Valid reports whether v is a decision the ledger accepts.
func (v Verdict) Valid() bool { return v == VerdictConfirm || v == VerdictReject }

func (v Verdict) authority() Authority {
	if v == VerdictReject {
		return AuthorityRejected
	}
	return AuthorityConfirmed
}

// ReviewResult is what a committed review produced.
type ReviewResult struct {
	// ReviewID is the request that was committed.
	ReviewID string
	// Records are the review records appended, ordered by target id.
	Records []Record
	// LedgerVersion is the run's version after the commit.
	LedgerVersion int64
}

// Option configures a Ledger.
type Option func(*Ledger)

// WithClock replaces the source of append timestamps. Timestamps are
// truncated to microseconds regardless, because that is the resolution
// PostgreSQL stores — a nanosecond a round trip cannot return would break
// digest verification after a read.
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) {
		if now != nil {
			l.now = now
		}
	}
}

// WithIDFactory replaces the record and review id generator.
func WithIDFactory(newID func() string) Option {
	return func(l *Ledger) {
		if newID != nil {
			l.newID = newID
		}
	}
}

// WithValidator supplies an already-compiled schema validator, so a process
// holding several ledgers compiles the schemas once.
func WithValidator(v *contracts.Validator) Option {
	return func(l *Ledger) {
		if v != nil {
			l.validator = v
		}
	}
}

// Ledger is the append-only work-ledger runtime. It is safe for concurrent
// use as long as its Store is.
type Ledger struct {
	store     Store
	validator *contracts.Validator
	now       func() time.Time
	newID     func() string
}

// New returns a ledger over store. It compiles the embedded schemas unless a
// validator is supplied, so a malformed schema is a construction failure
// rather than a surprise on the first append.
func New(s Store, opts ...Option) (*Ledger, error) {
	if s == nil {
		return nil, errors.New("ledger: New requires a store")
	}
	l := &Ledger{
		store: s,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return IDPrefix + store.NewULID() },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	if l.validator == nil {
		v, err := contracts.NewValidator()
		if err != nil {
			return nil, fmt.Errorf("ledger: compile schemas: %w", err)
		}
		l.validator = v
	}
	return l, nil
}

// Record returns one record by id.
func (l *Ledger) Record(ctx context.Context, id string) (Record, error) {
	return l.store.GetRecord(ctx, id)
}

// Records returns every record of a run, ordered by id.
func (l *Ledger) Records(ctx context.Context, runID string) ([]Record, error) {
	return l.store.RunRecords(ctx, runID)
}

// ReviewRequest returns one review request by id.
func (l *Ledger) ReviewRequest(ctx context.Context, id string) (ReviewRequest, error) {
	return l.store.GetReviewRequest(ctx, id)
}

// LedgerVersion returns the run's current ledger version — the number of
// records appended to it. Callers read it before building a review request
// and hand it back at commit time.
func (l *Ledger) LedgerVersion(ctx context.Context, runID string) (int64, error) {
	return l.store.LedgerVersion(ctx, runID)
}

// Append validates and appends one record, enforcing the PRD §10.4
// producer/authority matrix. It returns the record as stored: with its id,
// schema version, timestamp, and content digest filled in.
//
// A record whose producer is a runner must carry WithRunnerManifest.
//
// The append is serialised against other writers of the same run. That is
// what makes the ledger version a usable optimistic-concurrency token: a
// review holds the same lock while it checks the version and writes its
// decisions, so an append cannot slip in between the check and the commit
// and leave a review applied to work that changed underneath it. Runs do not
// contend with each other.
func (l *Ledger) Append(ctx context.Context, rec Record, opts ...AppendOption) (Record, error) {
	options := buildAppendOptions(opts)

	var appended Record
	err := l.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Lock(ctx, RunLockKey(rec.RunID)); err != nil {
			return err
		}
		var err error
		appended, err = l.appendThrough(ctx, tx, rec, options)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return appended, nil
}

// RunLockKey is the advisory-lock key every writer of a run's records takes:
// Append, AppendSuperseding, CreateReviewRequest, and CommitReview. It is
// exported because the key is a convention shared with any other component
// that writes to the same run and must queue behind them, and because a
// convention nobody can name is a convention nobody can join.
func RunLockKey(runID string) string { return "ledger:run:" + runID }

// SupersedeLockKey is the advisory-lock key taken while deciding whether a
// record already has a live replacement, so two corrections of the same
// record cannot both find it unreplaced.
func SupersedeLockKey(recordID string) string { return "ledger:supersede:" + recordID }

// AppendSuperseding appends rec as the replacement for supersedesID.
//
// The superseded record is not touched — it cannot be, the store forbids it.
// What changes is that the new record names it, which is what removes it from
// every projection. The append is refused if the target does not exist, if it
// already has a live replacement, or if rec belongs to a different run than
// the record it claims to replace.
func (l *Ledger) AppendSuperseding(ctx context.Context, rec Record, supersedesID string, opts ...AppendOption) (Record, error) {
	if supersedesID == "" {
		return Record{}, errors.New("ledger: AppendSuperseding requires the id of the record being superseded")
	}
	options := buildAppendOptions(opts)

	var appended Record
	err := l.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Lock(ctx, SupersedeLockKey(supersedesID)); err != nil {
			return err
		}

		target, err := tx.GetRecord(ctx, supersedesID)
		if err != nil {
			return fmt.Errorf("ledger: supersede %s: %w", supersedesID, err)
		}

		live, err := tx.LiveSupersessors(ctx, supersedesID)
		if err != nil {
			return fmt.Errorf("ledger: supersede %s: %w", supersedesID, err)
		}
		if len(live) > 0 {
			return fmt.Errorf("ledger: supersede %s: already replaced by %s: %w",
				supersedesID, live[0].ID, ErrAlreadySuperseded)
		}

		switch {
		case rec.RunID == "":
			rec.RunID = target.RunID
		case rec.RunID != target.RunID:
			return fmt.Errorf("ledger: supersede %s: record belongs to run %s but the superseded record belongs to run %s",
				supersedesID, rec.RunID, target.RunID)
		}
		if err := tx.Lock(ctx, RunLockKey(rec.RunID)); err != nil {
			return err
		}
		rec.Supersedes = NullableID(supersedesID)

		appended, err = l.appendThrough(ctx, tx, rec, options)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return appended, nil
}

// appendThrough is the single append path: normalise, digest, schema-validate,
// authority-check, insert. Every exported append — including the review
// records CommitReview writes — goes through it, so there is one place where
// a record can enter the ledger and one place the rules are applied.
func (l *Ledger) appendThrough(ctx context.Context, tx Tx, rec Record, o appendOptions) (Record, error) {
	rec = l.normalize(rec)

	digest, err := rec.ComputeDigest()
	if err != nil {
		return Record{}, err
	}
	rec.ContentDigest = digest

	if err := l.validator.Validate(contracts.SchemaLedgerRecord, rec); err != nil {
		return Record{}, fmt.Errorf("ledger: append %s: %w", rec.ID, err)
	}
	if err := checkAuthority(rec, o); err != nil {
		return Record{}, err
	}
	if err := tx.InsertRecord(ctx, rec); err != nil {
		return Record{}, fmt.Errorf("ledger: append %s: %w", rec.ID, err)
	}
	return rec, nil
}

// normalize fills in what the runtime owns and leaves everything else alone.
// It never rewrites a caller-supplied value: an id, timestamp, or schema
// version the caller set is the caller's statement, not a default to
// override.
func (l *Ledger) normalize(rec Record) Record {
	rec = rec.Clone()
	if rec.ID == "" {
		rec.ID = l.newID()
	}
	if rec.SchemaVersion == "" {
		rec.SchemaVersion = SchemaVersion
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = l.now()
	}
	// PostgreSQL timestamptz keeps microseconds. Truncating here means the
	// digest is computed over a timestamp the store can return unchanged,
	// so VerifyDigest still holds after a round trip.
	rec.CreatedAt = rec.CreatedAt.UTC().Truncate(time.Microsecond)
	if len(rec.Data) == 0 {
		rec.Data = json.RawMessage(`{}`)
	}
	if rec.ProvenanceRefs == nil {
		rec.ProvenanceRefs = []string{}
	}
	return rec
}

// CreateReviewRequest records a batch of records to be reviewed at a stated
// ledger version (PRD §10.8). The request itself carries no authority: it is
// a question, and CommitReview is the answer.
//
// An empty recordIDs set is legal and produces a valid, empty request —
// nothing to review is a real answer, not an error.
func (l *Ledger) CreateReviewRequest(ctx context.Context, runID string, recordIDs []string, ledgerVersion int64, opts ...ReviewOption) (ReviewRequest, error) {
	if runID == "" {
		return ReviewRequest{}, errors.New("ledger: CreateReviewRequest requires a run id")
	}
	options := buildReviewOptions(opts)

	targets := sortedUnique(recordIDs)

	var created ReviewRequest
	err := l.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Lock(ctx, RunLockKey(runID)); err != nil {
			return err
		}

		current, err := tx.LedgerVersion(ctx, runID)
		if err != nil {
			return err
		}
		if current != ledgerVersion {
			return &StaleReviewError{
				Reason:   StaleLedgerMoved,
				Expected: ledgerVersion,
				Actual:   current,
				Detail:   "the request names a ledger version that is not the run's current one, so it would be born stale",
			}
		}

		digests := make([]string, 0, len(targets))
		for _, id := range targets {
			rec, err := tx.GetRecord(ctx, id)
			if err != nil {
				return fmt.Errorf("ledger: review target %s: %w", id, err)
			}
			if rec.RunID != runID {
				return fmt.Errorf("ledger: review target %s belongs to run %s, not %s", id, rec.RunID, runID)
			}
			digests = append(digests, rec.ContentDigest)
		}

		checksum, err := frameChecksum(digests)
		if err != nil {
			return err
		}

		created = ReviewRequest{
			ID:              l.newReviewID(),
			RunID:           runID,
			ReviewerActorID: options.reviewer,
			LedgerVersion:   ledgerVersion,
			FrameChecksum:   checksum,
			Status:          ReviewRequested,
			RecordIDs:       targets,
			CreatedAt:       l.now().Truncate(time.Microsecond),
		}
		return tx.InsertReviewRequest(ctx, created)
	})
	if err != nil {
		return ReviewRequest{}, err
	}
	return created, nil
}

// ReviewOption configures a review request.
type ReviewOption func(*reviewOptions)

type reviewOptions struct {
	reviewer string
}

// WithReviewer binds the review request to the human actor who will decide
// it. CommitReview refuses a request without one: a confirmation nobody is
// accountable for is not a confirmation.
func WithReviewer(actorID string) ReviewOption {
	return func(o *reviewOptions) { o.reviewer = actorID }
}

func buildReviewOptions(opts []ReviewOption) reviewOptions {
	var o reviewOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// CommitReview applies a whole review batch in one transaction, or applies
// none of it (PRD §10.8).
//
// It refuses, and writes nothing, when: the run's current ledger version is
// not expectedLedgerVersion; the request recorded a different version than
// the caller expects; a reviewed record has been superseded; the reviewed
// records no longer checksum to the frame the reviewer read; the decision set
// does not exactly cover the records under review; or the request has already
// been committed.
//
// On success it appends one review record per decision — origin human,
// authority confirmed or rejected, subject_ref naming the target. The
// reviewed records themselves are untouched, so an agent's proposal remains
// a proposal with a human decision attached to it.
//
// The reviewer must be an actor the registry records as a human
// (ActorKindHuman). That check is what makes the human origin these records
// carry a fact rather than an assertion, and it is the reason an agent cannot
// decide its own claim by naming itself as the reviewer — the affirmative
// half of PRD §10.4 needs the same rigour as its refusal half.
//
// WithRationale records why the decision was made. It is optional here and
// required by the HTTP decision surface: see the option's own documentation.
func (l *Ledger) CommitReview(ctx context.Context, reviewID string, decisions map[string]Verdict, expectedLedgerVersion int64, opts ...CommitOption) (ReviewResult, error) {
	if reviewID == "" {
		return ReviewResult{}, errors.New("ledger: CommitReview requires a review id")
	}
	options := buildCommitOptions(opts)

	var result ReviewResult
	err := l.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		req, err := tx.GetReviewRequest(ctx, reviewID)
		if err != nil {
			return fmt.Errorf("ledger: commit review %s: %w", reviewID, err)
		}
		if err := tx.Lock(ctx, RunLockKey(req.RunID)); err != nil {
			return err
		}
		if req.Status != ReviewRequested {
			return fmt.Errorf("ledger: commit review %s: status is %s: %w", reviewID, req.Status, ErrReviewAlreadyCommitted)
		}
		if req.ReviewerActorID == "" {
			return fmt.Errorf("ledger: commit review %s: the request names no reviewer; a confirmation nobody is accountable for is not a confirmation", reviewID)
		}
		if err := checkReviewerIsHuman(ctx, tx, req); err != nil {
			return err
		}

		if err := l.checkReviewIsCurrent(ctx, tx, req, decisions, expectedLedgerVersion); err != nil {
			return err
		}

		appended, err := l.appendReviewRecords(ctx, tx, req, decisions, options)
		if err != nil {
			return err
		}

		committed, err := tx.MarkReviewCommitted(ctx, req.ID)
		if err != nil {
			return err
		}
		if !committed {
			return fmt.Errorf("ledger: commit review %s: %w", reviewID, ErrReviewAlreadyCommitted)
		}

		version, err := tx.LedgerVersion(ctx, req.RunID)
		if err != nil {
			return err
		}
		result = ReviewResult{ReviewID: req.ID, Records: appended, LedgerVersion: version}
		return nil
	})
	if err != nil {
		return ReviewResult{}, err
	}
	return result, nil
}

// CommitOption configures a review commit.
type CommitOption func(*commitOptions)

type commitOptions struct {
	rationale string
}

// WithRationale records the reviewer's stated reason on every review record
// the commit appends.
//
// It is optional at this layer and required by POST
// /v1alpha1/reviews/{id}/commit, and the asymmetry is deliberate. A
// confirmation with no stated reason cannot be told apart from an unread one,
// so the surface a person decides through demands one. The engine's
// human-task path (internal/engine/humandecision.go) reaches CommitReview
// with the decider's own reasoning already captured in the decision record's
// response payload, and making the option mandatory here would only make that
// path synthesise a sentence nobody wrote.
//
// An absent rationale leaves the key out of the payload entirely rather than
// writing an empty string: absence reads as "no reason was recorded", where
// "" would read as "a reason was given and it was blank".
func WithRationale(why string) CommitOption {
	return func(o *commitOptions) { o.rationale = why }
}

func buildCommitOptions(opts []CommitOption) commitOptions {
	var o commitOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// checkReviewerIsHuman resolves the named reviewer against the actor registry
// and refuses anything that is not a registered human.
//
// This is the affirmative half of PRD §10.4's authority model doing the same
// job its refusal half does. checkAuthority already stops an agent-ORIGIN
// record from carrying confirmed authority, but review records are stamped
// with human origin by appendReviewRecords from the request's reviewer id, so
// that check sees a human no matter who was named. The producer/authority
// matrix cannot catch this one: by the time it runs, the lie is already in the
// record. It has to be caught here, against the registry.
func checkReviewerIsHuman(ctx context.Context, tx Tx, req ReviewRequest) error {
	kind, err := tx.ActorKind(ctx, req.ReviewerActorID)
	if err != nil {
		return fmt.Errorf("ledger: commit review %s: reviewer %s: %w", req.ID, req.ReviewerActorID, err)
	}
	if kind == ActorKindHuman {
		return nil
	}
	return &AuthorityError{
		Rule: RuleReviewerNotHuman,
		// The reviewer's REGISTERED kind, not the human origin the review
		// record would have carried — naming `human` here would restate the
		// assumption being refused.
		Origin:     OriginKind(kind),
		ActorID:    req.ReviewerActorID,
		Authority:  AuthorityConfirmed,
		RecordType: RecordReview,
		Detail: "a decision on a proposed record is a human's to make (PRD §10.4); an actor registered as " +
			kind + " deciding a claim would be the producer side of the ledger promoting itself",
	}
}

// checkReviewIsCurrent runs every staleness and coverage guard before a
// single record is written.
func (l *Ledger) checkReviewIsCurrent(ctx context.Context, tx Tx, req ReviewRequest, decisions map[string]Verdict, expected int64) error {
	if req.LedgerVersion != expected {
		return &StaleReviewError{
			ReviewID: req.ID,
			Reason:   StaleRequestVersionMismatch,
			Expected: expected,
			Actual:   req.LedgerVersion,
			Detail:   "the caller expects a different ledger version than the request recorded reviewing",
		}
	}

	current, err := tx.LedgerVersion(ctx, req.RunID)
	if err != nil {
		return err
	}
	if current != expected {
		return &StaleReviewError{
			ReviewID: req.ID,
			Reason:   StaleLedgerMoved,
			Expected: expected,
			Actual:   current,
			Detail:   "records were appended to the run after the review was requested",
		}
	}

	if err := checkReviewCoverage(req, decisions); err != nil {
		return err
	}

	digests := make([]string, 0, len(req.RecordIDs))
	for _, id := range req.RecordIDs {
		rec, err := tx.GetRecord(ctx, id)
		if err != nil {
			return fmt.Errorf("ledger: commit review %s: target %s: %w", req.ID, id, err)
		}
		live, err := tx.LiveSupersessors(ctx, id)
		if err != nil {
			return err
		}
		if len(live) > 0 {
			return &StaleReviewError{
				ReviewID: req.ID,
				Reason:   StaleTargetSuperseded,
				Expected: expected,
				Actual:   current,
				Detail:   fmt.Sprintf("record %s under review has been replaced by %s", id, live[0].ID),
			}
		}
		digests = append(digests, rec.ContentDigest)
	}

	checksum, err := frameChecksum(digests)
	if err != nil {
		return err
	}
	if checksum != req.FrameChecksum {
		return &StaleReviewError{
			ReviewID: req.ID,
			Reason:   StaleFrameChecksum,
			Expected: expected,
			Actual:   current,
			Detail:   "the reviewed records are not the records the request checksummed",
		}
	}
	return nil
}

func checkReviewCoverage(req ReviewRequest, decisions map[string]Verdict) error {
	requested := make(map[string]bool, len(req.RecordIDs))
	for _, id := range req.RecordIDs {
		requested[id] = true
	}

	var unknown []string
	for id, verdict := range decisions {
		if !verdict.Valid() {
			return fmt.Errorf("ledger: review %s: decision for %s is %q, want %q or %q",
				req.ID, id, verdict, VerdictConfirm, VerdictReject)
		}
		if !requested[id] {
			unknown = append(unknown, id)
		}
	}

	var undecided []string
	for _, id := range req.RecordIDs {
		if _, ok := decisions[id]; !ok {
			undecided = append(undecided, id)
		}
	}

	if len(unknown) > 0 || len(undecided) > 0 {
		sort.Strings(unknown)
		sort.Strings(undecided)
		return &ReviewTargetError{ReviewID: req.ID, Unknown: unknown, Undecided: undecided}
	}
	return nil
}

func (l *Ledger) appendReviewRecords(ctx context.Context, tx Tx, req ReviewRequest, decisions map[string]Verdict, commit commitOptions) ([]Record, error) {
	options := appendOptions{reviewTransaction: true}
	appended := make([]Record, 0, len(req.RecordIDs))

	for _, targetID := range req.RecordIDs {
		verdict := decisions[targetID]
		data := map[string]any{
			"verdict":        string(verdict),
			"reviewed_refs":  []string{targetID},
			"ledger_version": strconv.FormatInt(req.LedgerVersion, 10),
			"frame_checksum": req.FrameChecksum,
		}
		if commit.rationale != "" {
			data["rationale"] = commit.rationale
		}
		payload, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("ledger: encode review payload for %s: %w", targetID, err)
		}

		rec, err := l.appendThrough(ctx, tx, Record{
			RecordType:     RecordReview,
			RunID:          req.RunID,
			Origin:         Origin{Kind: OriginHuman, ActorID: req.ReviewerActorID},
			Authority:      verdict.authority(),
			SubjectRef:     NullableID(targetID),
			Data:           payload,
			ProvenanceRefs: []string{targetID},
		}, options)
		if err != nil {
			return nil, err
		}
		appended = append(appended, rec)
	}
	return appended, nil
}

func (l *Ledger) newReviewID() string {
	id := l.newID()
	// A caller-supplied id factory owns its own naming; only rewrite the
	// default prefix, so review ids stay distinguishable from record ids
	// without overriding an explicit choice.
	if len(id) > len(IDPrefix) && id[:len(IDPrefix)] == IDPrefix {
		return ReviewIDPrefix + id[len(IDPrefix):]
	}
	return id
}

// frameChecksum digests the reviewed records' content digests, in the order
// their ids sort. It answers "are these still the records that were read?"
// without re-reading their contents.
func frameChecksum(digests []string) (string, error) {
	if digests == nil {
		digests = []string{}
	}
	checksum, err := contracts.DigestValue(digests)
	if err != nil {
		return "", fmt.Errorf("ledger: compute frame checksum: %w", err)
	}
	return checksum, nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
