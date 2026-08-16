package handover

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// RefNamespace is the only ref namespace this package will ever fetch, and
// the one the bridges' own preserve.HANDOVER_REF_NAMESPACE mints into. It is
// the client-side half of t9's server-side fence: a ruleset that refuses a
// push outside refs/culture-nodes/* keeps a session from PUTTING work
// anywhere else, and this keeps the control plane from LOOKING anywhere else.
// Both halves are needed — a fence nothing is tested against gets trusted.
const RefNamespace = "refs/culture-nodes/"

// CollectionMethod names how a measurement here was made, for the evidence
// record's own `collection_method` field. It is a distinct value from the
// runner-side methods (`runner_exit_status`, `workspace_snapshot_diff`)
// because the act is different: the control plane fetched a ref over the
// network and read the commit it resolved to.
const CollectionMethod = "git_ref_fetch"

// Claim is what a finished attempt asserted, reduced to the single field this
// package is willing to take from an agent: the NAME of a ref it says it
// created. The commit, the changed paths and the remote are all measured
// here; a bridge's own report of them reaches this package by no route at
// all.
type Claim struct {
	RunID     string
	NodeRunID string
	AttemptID string
	// Ref is the agent-claimed ref name. Empty means the attempt claimed no
	// handover, which is not an error and produces no record.
	Ref string
}

// Measurement is what a fetch directly established. Every field is this
// package's own observation; there is deliberately no field for anything the
// agent said.
type Measurement struct {
	// Ref is the ref that was fetched, as it exists on the remote.
	Ref string
	// CommitSHA is what the fetch resolved that ref to.
	CommitSHA string
	// ChangedPaths are the paths the commit changed against its first
	// parent (the whole tree, for a root commit), sorted.
	ChangedPaths []string
	// PathsTruncated is true when the commit changed more paths than the
	// fetcher's cap, so a reader never mistakes a capped list for a
	// complete one. It rides into the record's `completeness`.
	PathsTruncated bool
	// Source is the remote the CONTROL PLANE fetched from — its own
	// configuration, never a url the agent reported.
	Source string
	// FetchedAt is when the measurement was taken.
	FetchedAt time.Time
}

// Fetcher fetches one ref and reports what it measured. An error means no
// measurement was made — never a partial or assumed one.
type Fetcher interface {
	Fetch(ctx context.Context, ref string) (Measurement, error)
}

// Appender is the slice of *ledger.Ledger this package needs. Narrow on
// purpose: this package may append records and may do nothing else to the
// ledger — it cannot read, review, or supersede.
type Appender interface {
	Append(ctx context.Context, rec ledger.Record, opts ...ledger.AppendOption) (ledger.Record, error)
}

// ValidateRef refuses any ref this package must not fetch.
//
// The namespace check is the fence. The character and shape checks are
// ordinary git ref hygiene, but they matter more here than usual because this
// string reaches an argument vector: a value starting with "-" would be read
// by git as an option rather than a ref, and `..` is how a refname escapes
// its namespace.
func ValidateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("handover: no ref was claimed")
	}
	if !strings.HasPrefix(ref, RefNamespace) {
		return fmt.Errorf(
			"handover: ref %q is outside %s; the control plane fetches only the namespace the handover fence covers",
			ref, RefNamespace)
	}
	if len(ref) > 512 {
		return fmt.Errorf("handover: ref %q is longer than a ref this package will fetch", ref)
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("handover: ref %q contains %q, which is not a character a handover ref is minted with", ref, r)
		}
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") {
		return fmt.Errorf("handover: ref %q is not a well-formed ref name", ref)
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, "-") || strings.HasPrefix(part, ".") {
			return fmt.Errorf("handover: ref %q has a component git would not accept (%q)", ref, part)
		}
	}
	return nil
}

// Observer turns a claimed ref into measured evidence, or into nothing.
//
// The zero value, and a nil *Observer, are both usable and both do nothing —
// so a worker or callback path built without one behaves exactly as it did
// before this package existed.
type Observer struct {
	// Fetcher measures the ref. Nil disables observation entirely: a
	// deployment that has configured no remote to fetch from cannot measure
	// anything, and must therefore record nothing.
	Fetcher Fetcher
	// Ledger is where the record is appended.
	Ledger Appender
	// ActorID identifies the measuring producer. PRD §10.4 admits observed
	// evidence only from an IDENTIFIED trusted runner, so an Observer with
	// no actor id observes nothing rather than writing an anonymous
	// observation.
	ActorID string
	// ActorRevision is the measuring producer's revision, when it has one.
	ActorRevision string
	// Now is the clock, defaulting to time.Now().UTC().
	Now func() time.Time
	// OnError receives the reason a measurement did not happen. It is
	// operator-facing logging: a failed fetch is a fact about this process,
	// not a fact about the run, and writing it to the ledger would be
	// exactly the "record marked unmeasured" this package refuses.
	OnError func(error)
}

// DefaultMaxChangedPaths bounds the path list one evidence record carries. A
// commit that changed more than this many paths still produces a record; the
// list is cut and `completeness` says `partial`, because a silently capped
// list that reads as complete is worse than a short one that says so.
const DefaultMaxChangedPaths = 500

// Observe fetches the ref *claim* names and appends one observed evidence
// record describing what the fetch measured.
//
// It returns the appended record and true ONLY when a record was really
// written. Every other path — no claim, no fetcher, a ref outside the fence,
// a ref nothing can fetch, an append that failed — returns false and writes
// nothing at all. See this package's doc comment for why "no fetchable ref"
// is a refusal rather than a record marked unmeasured.
func (o *Observer) Observe(ctx context.Context, claim Claim) (ledger.Record, bool) {
	if o == nil || o.Fetcher == nil || o.Ledger == nil {
		return ledger.Record{}, false
	}
	if o.ActorID == "" {
		o.report(fmt.Errorf("handover: this observer names no measuring actor, so it can attest to nothing"))
		return ledger.Record{}, false
	}
	if claim.Ref == "" {
		// The overwhelmingly common case: the attempt handed nothing over.
		// Not an error, and deliberately not reported as one.
		return ledger.Record{}, false
	}
	if err := ValidateRef(claim.Ref); err != nil {
		o.report(err)
		return ledger.Record{}, false
	}

	measured, err := o.Fetcher.Fetch(ctx, claim.Ref)
	if err != nil {
		// The load-bearing branch. An attempt whose agent claimed success
		// but whose ref cannot be fetched leaves NO observed record — the
		// claim stands alone as the proposed record it always was.
		o.report(fmt.Errorf("handover: ref %s was claimed but could not be fetched, so nothing is recorded: %w", claim.Ref, err))
		return ledger.Record{}, false
	}
	if measured.CommitSHA == "" {
		o.report(fmt.Errorf("handover: the fetch of %s resolved no commit, so nothing is recorded", claim.Ref))
		return ledger.Record{}, false
	}

	record, manifest, err := o.buildRecord(claim, measured)
	if err != nil {
		o.report(err)
		return ledger.Record{}, false
	}

	appended, err := o.Ledger.Append(ctx, record, ledger.WithRunnerManifest(manifest))
	if err != nil {
		o.report(fmt.Errorf("handover: append observed evidence for attempt %s: %w", claim.AttemptID, err))
		return ledger.Record{}, false
	}
	return appended, true
}

// BuildRecord composes the evidence record and the manifest authorizing it,
// without appending either. Exported so a caller (or a test) can inspect what
// would be written, and check it against ledger.CheckAuthority, without a
// ledger.
func (o *Observer) BuildRecord(claim Claim, measured Measurement) (ledger.Record, ledger.RunnerManifest, error) {
	return o.buildRecord(claim, measured)
}

func (o *Observer) buildRecord(claim Claim, measured Measurement) (ledger.Record, ledger.RunnerManifest, error) {
	paths := append([]string(nil), measured.ChangedPaths...)
	sort.Strings(paths)
	if paths == nil {
		// A commit that changed nothing is a real measurement of nothing —
		// it must serialize as [] rather than null, so a reader can tell it
		// apart from a field that was never measured.
		paths = []string{}
	}

	// Every write below goes through the same pair of appends, so a field
	// cannot enter the payload without entering the manifest: the manifest is
	// the list of what this package directly measured, and this is the only
	// place it is built. (internal/runners/dispatch.go's evidenceBuilder makes
	// the identical argument for the runner-side record.)
	data := map[string]any{}
	var pointers []string
	set := func(key string, value any) {
		data[key] = value
		pointers = append(pointers, "/"+key)
	}
	measurements := map[string]any{}
	data["measurements"] = measurements
	measure := func(key string, value any) {
		measurements[key] = value
		pointers = append(pointers, "/measurements/"+key)
	}

	set("producer_id", o.ActorID)
	if o.ActorRevision != "" {
		set("producer_revision", o.ActorRevision)
	}
	set("collection_method", CollectionMethod)
	set("observed_at", measured.FetchedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	set("covered_scope", coveredScope(measured))
	set("completeness", completeness(measured))

	// The three facts t10 asks for, each one read out of the fetch.
	measure("ref", measured.Ref)
	measure("commit_sha", measured.CommitSHA)
	measure("changed_paths", paths)
	measure("changed_path_count", len(paths))
	// Where the measurement was taken FROM. It is the control plane's own
	// configuration, so it is as directly known as the rest — and recording
	// it is what lets a later reader check that the subject was not a
	// repository the agent chose.
	measure("source_remote", measured.Source)

	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, ledger.RunnerManifest{}, fmt.Errorf("handover: encode evidence payload: %w", err)
	}
	sort.Strings(pointers)

	record := ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      claim.RunID,
		NodeRunID:  ledger.NullableID(claim.NodeRunID),
		AttemptID:  ledger.NullableID(claim.AttemptID),
		Origin: ledger.Origin{
			Kind:          ledger.OriginRunner,
			ActorID:       o.ActorID,
			ActorRevision: o.ActorRevision,
		},
		Authority:      ledger.AuthorityObserved,
		Data:           payload,
		ProvenanceRefs: []string{},
	}
	manifest := ledger.RunnerManifest{ActorID: o.ActorID, ObservableFields: pointers}
	return record, manifest, nil
}

// coveredScope states what this evidence covers, in the same spirit as the
// runner-side record: it names the boundary rather than leaving a reader to
// assume one. It says nothing about whether the commit is correct, useful, or
// what the node asked for — only that it exists and contains these paths.
func coveredScope(m Measurement) string {
	scope := fmt.Sprintf(
		"the git commit %s reachable from %s on %s, and the paths it changes against its first parent",
		short(m.CommitSHA), m.Ref, m.Source)
	if m.PathsTruncated {
		return scope + "; the path list is capped and does not name every changed path"
	}
	return scope
}

func completeness(m Measurement) string {
	if m.PathsTruncated {
		return "partial"
	}
	return "complete"
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (o *Observer) report(err error) {
	if o != nil && o.OnError != nil && err != nil {
		o.OnError(err)
	}
}

// MeasuredHandover is one live handover-evidence record, read back.
//
// It carries the same three facts buildRecord measured, plus the id of the
// record they came out of, so a consumer can point at the evidence rather
// than restate it.
type MeasuredHandover struct {
	RecordID     string
	Ref          string
	CommitSHA    string
	ChangedPaths []string
}

// Measured reports the live handover-evidence record for a run, or ok=false
// when this package has measured no handover for it.
//
// It lives here, next to buildRecord, on purpose: it is the READER of exactly
// the payload that function writes, and the two would drift the moment a
// caller reimplemented the field names. It selects on `collection_method`
// rather than on record type alone, so a runner's ordinary workspace evidence
// is never mistaken for a ref this package fetched. The LAST match wins —
// records are immutable and a re-fetch appends, so the newest measurement is
// the current one.
//
// The changed-path list is what makes this more than MeasuredCommit's two
// return values: internal/repair checks it against the workflow-scope
// boundary, so a gate failure on a commit that touched CI configuration is
// routed to a person rather than at a dispatch that would be refused for
// touching it.
func Measured(records []ledger.Record) (MeasuredHandover, bool) {
	var found MeasuredHandover
	ok := false
	for _, rec := range ledger.Live(records) {
		if rec.RecordType != ledger.RecordEvidence || rec.Authority != ledger.AuthorityObserved {
			continue
		}
		data, err := rec.DataMap()
		if err != nil {
			continue
		}
		if method, _ := data["collection_method"].(string); method != CollectionMethod {
			continue
		}
		measurements, _ := data["measurements"].(map[string]any)
		sha, _ := measurements["commit_sha"].(string)
		if sha == "" {
			continue
		}
		ref, _ := measurements["ref"].(string)
		raw, _ := measurements["changed_paths"].([]any)
		paths := make([]string, 0, len(raw))
		for _, entry := range raw {
			if p, isString := entry.(string); isString {
				paths = append(paths, p)
			}
		}
		found = MeasuredHandover{RecordID: rec.ID, Ref: ref, CommitSHA: sha, ChangedPaths: paths}
		ok = true
	}
	return found, ok
}

// MeasuredCommit reports the id of the live handover-evidence record in
// records and the commit that record measured, or ("", "") when this package
// has measured no handover for the run.
//
// It is a projection of Measured rather than a second walk of the records:
// two recognition rules for one payload is how the drift this package's
// comment warns about actually happens.
func MeasuredCommit(records []ledger.Record) (recordID string, commitSHA string) {
	measured, ok := Measured(records)
	if !ok {
		return "", ""
	}
	return measured.RecordID, measured.CommitSHA
}
