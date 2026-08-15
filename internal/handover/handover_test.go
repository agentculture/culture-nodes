package handover_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// recordingAppender stands in for the ledger where a test needs to inject an
// append failure. The REAL ledger runtime — schema validation, the §10.4
// authority matrix, the runner-manifest check — is exercised separately by
// TestARecordFromThisPackageIsAcceptedByTheRealLedgerRuntime, so nothing here
// can pass by being checked against a laxer stand-in.
type recordingAppender struct {
	records []ledger.Record
	err     error
}

func (a *recordingAppender) Append(_ context.Context, rec ledger.Record, _ ...ledger.AppendOption) (ledger.Record, error) {
	if a.err != nil {
		return ledger.Record{}, a.err
	}
	rec.ID = fmt.Sprintf("ledger_test_%d", len(a.records)+1)
	a.records = append(a.records, rec)
	return rec, nil
}

type failingFetcher struct{ err error }

func (f failingFetcher) Fetch(context.Context, string) (handover.Measurement, error) {
	return handover.Measurement{}, f.err
}

type fixedFetcher struct {
	m       handover.Measurement
	seenRef string
}

func (f *fixedFetcher) Fetch(_ context.Context, ref string) (handover.Measurement, error) {
	f.seenRef = ref
	m := f.m
	m.Ref = ref
	return m, nil
}

func newObserver(fetcher handover.Fetcher, appender handover.Appender) *handover.Observer {
	return &handover.Observer{
		Fetcher:       fetcher,
		Ledger:        appender,
		ActorID:       "culture-nodes/handover-fetch",
		ActorRevision: "test",
		OnError:       func(error) {},
	}
}

func claim() handover.Claim {
	return handover.Claim{
		RunID:     "run_01",
		NodeRunID: "nr_01",
		AttemptID: "att_01",
		Ref:       "refs/culture-nodes/run_01/fix-1730000000-abcd",
	}
}

func measurementsOf(t *testing.T, rec ledger.Record) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode record payload: %v", err)
	}
	m, ok := data["measurements"].(map[string]any)
	if !ok {
		t.Fatalf("record payload has no measurements object: %s", rec.Data)
	}
	return m
}

// ---------------------------------------------------------------------------
// acceptance 1: a fetched ref lands as observed evidence citing ref + sha
// ---------------------------------------------------------------------------

func TestAFetchedRefIsRecordedAsObservedEvidenceCitingItsRefAndCommit(t *testing.T) {
	appender := &recordingAppender{}
	fetcher := &fixedFetcher{m: handover.Measurement{
		CommitSHA:    "0f1e2d3c4b5a69788796a5b4c3d2e1f001122334",
		ChangedPaths: []string{"internal/engine/workflow.go", "CHANGELOG.md"},
		Source:       "git@example.invalid:agentculture/culture-nodes.git",
		FetchedAt:    time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	}}

	rec, ok := newObserver(fetcher, appender).Observe(context.Background(), claim())
	if !ok {
		t.Fatal("a fetchable ref must produce a record")
	}
	if len(appender.records) != 1 {
		t.Fatalf("expected exactly one appended record, got %d", len(appender.records))
	}
	if rec.Authority != ledger.AuthorityObserved {
		t.Fatalf("authority = %q, want observed", rec.Authority)
	}
	if rec.RecordType != ledger.RecordEvidence {
		t.Fatalf("record type = %q, want evidence", rec.RecordType)
	}
	if rec.Origin.Kind != ledger.OriginRunner {
		t.Fatalf("origin kind = %q, want runner", rec.Origin.Kind)
	}
	if rec.RunID != "run_01" || rec.AttemptID.String() != "att_01" {
		t.Fatalf("record is not stamped against the attempt: run=%q attempt=%q", rec.RunID, rec.AttemptID)
	}

	m := measurementsOf(t, rec)
	if m["ref"] != claim().Ref {
		t.Fatalf("record cites ref %v, want %v", m["ref"], claim().Ref)
	}
	if m["commit_sha"] != fetcher.m.CommitSHA {
		t.Fatalf("record cites commit %v, want %v", m["commit_sha"], fetcher.m.CommitSHA)
	}
	paths, _ := json.Marshal(m["changed_paths"])
	if !strings.Contains(string(paths), "internal/engine/workflow.go") {
		t.Fatalf("record does not carry the changed paths: %s", paths)
	}
}

// ---------------------------------------------------------------------------
// acceptance 2 (the load-bearing one): no fetchable ref means NO record
// ---------------------------------------------------------------------------

func TestAClaimWithNoFetchableRefProducesNoRecordAtAll(t *testing.T) {
	appender := &recordingAppender{}
	var reported []error
	observer := newObserver(failingFetcher{err: errors.New("couldn't find remote ref")}, appender)
	observer.OnError = func(err error) { reported = append(reported, err) }

	if _, ok := observer.Observe(context.Background(), claim()); ok {
		t.Fatal("an unfetchable ref must not produce a record")
	}
	if len(appender.records) != 0 {
		t.Fatalf("expected NO ledger record, got %d: %s", len(appender.records), appender.records[0].Data)
	}
	// The reason exists — as a diagnostic for an operator, never as a row.
	if len(reported) != 1 {
		t.Fatalf("expected the failure to be reported once to OnError, got %d", len(reported))
	}
}

func TestAnAttemptThatClaimedNoHandoverProducesNoRecord(t *testing.T) {
	appender := &recordingAppender{}
	fetcher := &fixedFetcher{m: handover.Measurement{CommitSHA: "deadbeef"}}
	c := claim()
	c.Ref = ""

	if _, ok := newObserver(fetcher, appender).Observe(context.Background(), c); ok {
		t.Fatal("an attempt that handed nothing over must produce no record")
	}
	if len(appender.records) != 0 {
		t.Fatalf("expected NO ledger record, got %d", len(appender.records))
	}
	if fetcher.seenRef != "" {
		t.Fatalf("nothing should have been fetched, but %q was", fetcher.seenRef)
	}
}

func TestARefOutsideTheFencedNamespaceIsNeverFetched(t *testing.T) {
	appender := &recordingAppender{}
	fetcher := &fixedFetcher{m: handover.Measurement{CommitSHA: "deadbeef"}}
	observer := newObserver(fetcher, appender)

	for _, ref := range []string{
		"refs/heads/main",
		"refs/culture-nodes/../heads/main",
		"--upload-pack=touch /tmp/pwned",
		"refs/culture-nodes/run/-oops",
		"refs/culture-nodes/run/ref with spaces",
	} {
		c := claim()
		c.Ref = ref
		if _, ok := observer.Observe(context.Background(), c); ok {
			t.Fatalf("ref %q must be refused", ref)
		}
		if fetcher.seenRef != "" {
			t.Fatalf("ref %q reached the fetcher", ref)
		}
		if len(appender.records) != 0 {
			t.Fatalf("ref %q produced a record", ref)
		}
	}
}

func TestAnAppendFailureLeavesNoRecordAndIsReported(t *testing.T) {
	appender := &recordingAppender{err: errors.New("ledger is down")}
	var reported []error
	observer := newObserver(&fixedFetcher{m: handover.Measurement{CommitSHA: "abc123"}}, appender)
	observer.OnError = func(err error) { reported = append(reported, err) }

	if _, ok := observer.Observe(context.Background(), claim()); ok {
		t.Fatal("a failed append must not report a written record")
	}
	if len(reported) != 1 {
		t.Fatalf("expected the append failure to be reported, got %d", len(reported))
	}
}

// ---------------------------------------------------------------------------
// acceptance 3: what is recorded is what was measured, never what was reported
// ---------------------------------------------------------------------------

func TestTheRecordCarriesTheFetchedFactsNotTheAgentsClaim(t *testing.T) {
	// The agent's own account of its work — a commit sha and a file list it
	// asserts. There is deliberately no route for any of it into Observe:
	// Claim carries a ref NAME and nothing else, so this variable can only
	// be used by the test, which is the point being asserted.
	agentClaimedCommit := "1111111111111111111111111111111111111111"
	agentClaimedPaths := []string{"docs/RESULT.md"}

	appender := &recordingAppender{}
	fetcher := &fixedFetcher{m: handover.Measurement{
		CommitSHA:    "2222222222222222222222222222222222222222",
		ChangedPaths: []string{"internal/api/server.go"},
		Source:       "git@example.invalid:agentculture/culture-nodes.git",
		FetchedAt:    time.Now().UTC(),
	}}

	rec, ok := newObserver(fetcher, appender).Observe(context.Background(), claim())
	if !ok {
		t.Fatal("expected a record")
	}
	body := string(rec.Data)
	if strings.Contains(body, agentClaimedCommit) {
		t.Fatalf("the record carries the agent's claimed commit: %s", body)
	}
	for _, p := range agentClaimedPaths {
		if strings.Contains(body, p) {
			t.Fatalf("the record carries the agent's claimed path %q: %s", p, body)
		}
	}
	if !strings.Contains(body, "internal/api/server.go") {
		t.Fatalf("the record does not carry the fetched path list: %s", body)
	}
	m := measurementsOf(t, rec)
	if m["source_remote"] != fetcher.m.Source {
		t.Fatalf("the record does not name the remote the control plane fetched from: %v", m["source_remote"])
	}
}

// ---------------------------------------------------------------------------
// the manifest: observed evidence may only declare what was measured
// ---------------------------------------------------------------------------

func TestTheManifestDeclaresExactlyTheFieldsTheRecordCarries(t *testing.T) {
	observer := newObserver(nil, nil)
	rec, manifest, err := observer.BuildRecord(claim(), handover.Measurement{
		Ref:          claim().Ref,
		CommitSHA:    "abc",
		ChangedPaths: []string{"a.go"},
		Source:       "remote",
		FetchedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("build record: %v", err)
	}
	// The real matrix: runner origin + observed authority + a manifest that
	// covers every leaf. A field added to the payload without being declared
	// fails right here.
	if err := ledger.CheckAuthority(rec, &manifest); err != nil {
		t.Fatalf("the record this package builds would be refused by the ledger: %v", err)
	}
	if manifest.ActorID != rec.Origin.ActorID {
		t.Fatalf("manifest actor %q != record origin %q", manifest.ActorID, rec.Origin.ActorID)
	}
	for _, declared := range manifest.ObservableFields {
		if declared == "" {
			t.Fatal("the manifest declares the whole payload, which would license every future field")
		}
	}
}

func TestAnObserverWithNoActorIdentityObservesNothing(t *testing.T) {
	appender := &recordingAppender{}
	observer := newObserver(&fixedFetcher{m: handover.Measurement{CommitSHA: "abc"}}, appender)
	observer.ActorID = ""
	if _, ok := observer.Observe(context.Background(), claim()); ok {
		t.Fatal("an unidentified observer must not write observed evidence")
	}
	if len(appender.records) != 0 {
		t.Fatal("an unidentified observer wrote a record")
	}
}

func TestANilObserverIsAUsableNoOp(t *testing.T) {
	var observer *handover.Observer
	if _, ok := observer.Observe(context.Background(), claim()); ok {
		t.Fatal("a nil observer must observe nothing")
	}
}

// ---------------------------------------------------------------------------
// the real git fetcher, against a real repository
// ---------------------------------------------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// originWithHandoverRef builds a repository holding one handover ref, exactly
// as a bridge's preserve.handover_ref would leave it: a commit reachable from
// no branch, under refs/culture-nodes/.
func originWithHandoverRef(t *testing.T, ref string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	git(t, dir, "config", "user.email", "t10@example.com")
	git(t, dir, "config", "user.name", "t10")
	writeFile(t, dir, "README.md", "# base\n")
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "--quiet", "-m", "base")

	for name, body := range files {
		writeFile(t, dir, name, body)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "handover")
	sha := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "update-ref", ref, sha)
	// Put the branch back where it was, so the handover commit is reachable
	// ONLY through the ref — the shape the bridge actually produces.
	git(t, dir, "reset", "--quiet", "--hard", "HEAD~1")
	return dir
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGitFetcherMeasuresTheRefTheCommitAndTheChangedPaths(t *testing.T) {
	ref := "refs/culture-nodes/run_01/fix-1730000000-abcd"
	origin := originWithHandoverRef(t, ref, map[string]string{
		"internal/engine/workflow.go": "package engine\n",
		"CHANGELOG.md":                "## next\n",
	})
	want := git(t, origin, "rev-parse", ref)

	fetcher := &handover.GitFetcher{Remote: origin, Timeout: 60 * time.Second}
	measured, err := fetcher.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if measured.CommitSHA != want {
		t.Fatalf("commit = %q, want %q", measured.CommitSHA, want)
	}
	if measured.Ref != ref {
		t.Fatalf("ref = %q, want %q", measured.Ref, ref)
	}
	if measured.Source != origin {
		t.Fatalf("source = %q, want the configured remote %q", measured.Source, origin)
	}
	got := strings.Join(measured.ChangedPaths, ",")
	if !strings.Contains(got, "internal/engine/workflow.go") || !strings.Contains(got, "CHANGELOG.md") {
		t.Fatalf("changed paths = %v, want both changed files", measured.ChangedPaths)
	}
	if measured.PathsTruncated {
		t.Fatal("two paths must not be reported as truncated")
	}
}

func TestGitFetcherFailsOnARefTheRemoteDoesNotHave(t *testing.T) {
	origin := originWithHandoverRef(t, "refs/culture-nodes/run_01/real", map[string]string{"a.go": "package a\n"})
	fetcher := &handover.GitFetcher{Remote: origin, Timeout: 60 * time.Second}

	if _, err := fetcher.Fetch(context.Background(), "refs/culture-nodes/run_01/never-pushed"); err == nil {
		t.Fatal("fetching a ref the remote does not have must fail, so no record is written")
	}
}

// The end-to-end shape of the second acceptance criterion, with a real git:
// the agent says it succeeded and names a ref, the ref was never published,
// and the ledger stays empty.
func TestAClaimedButUnpublishedRefWritesNothingWithARealGit(t *testing.T) {
	origin := originWithHandoverRef(t, "refs/culture-nodes/run_01/real", map[string]string{"a.go": "package a\n"})
	appender := &recordingAppender{}
	observer := newObserver(&handover.GitFetcher{Remote: origin, Timeout: 60 * time.Second}, appender)

	c := claim()
	c.Ref = "refs/culture-nodes/run_01/claimed-but-never-pushed"
	if _, ok := observer.Observe(context.Background(), c); ok {
		t.Fatal("an unpublished ref must produce no record")
	}
	if len(appender.records) != 0 {
		t.Fatalf("expected NO ledger record, got %d", len(appender.records))
	}
}

func TestGitFetcherReportsTruncationRatherThanShorteningSilently(t *testing.T) {
	ref := "refs/culture-nodes/run_01/big"
	files := map[string]string{}
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("file-%d.txt", i)] = "x\n"
	}
	origin := originWithHandoverRef(t, ref, files)

	fetcher := &handover.GitFetcher{Remote: origin, MaxChangedPaths: 3, Timeout: 60 * time.Second}
	measured, err := fetcher.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(measured.ChangedPaths) != 3 || !measured.PathsTruncated {
		t.Fatalf("expected 3 paths and a truncation flag, got %d paths truncated=%v",
			len(measured.ChangedPaths), measured.PathsTruncated)
	}

	rec, _, err := newObserver(nil, nil).BuildRecord(claim(), measured)
	if err != nil {
		t.Fatalf("build record: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if data["completeness"] != "partial" {
		t.Fatalf("a capped path list must be reported partial, got %v", data["completeness"])
	}
}

func TestAFetcherWithNoRemoteMeasuresNothing(t *testing.T) {
	if _, err := (&handover.GitFetcher{}).Fetch(context.Background(), claim().Ref); err == nil {
		t.Fatal("a fetcher with no configured remote must refuse rather than invent a source")
	}
}

// ---------------------------------------------------------------------------
// through the REAL ledger runtime
// ---------------------------------------------------------------------------

// memLedgerStore is a ledger.Store just complete enough to append through the
// real runtime. It exists so the end-to-end test below runs the production
// path — normalize, digest, JSON-Schema validation against
// schemas/ledger/{envelope,evidence}.schema.json, and the §10.4 authority
// matrix with this package's own manifest — rather than a stand-in that could
// accept a record PostgreSQL would refuse.
type memLedgerStore struct {
	records []ledger.Record
}

// ActorKind satisfies ledger.Tx, which task t30 widened so CommitReview can
// check that a reviewer is a registered human rather than trusting the id it
// was handed. Nothing in this package reviews anything, so the fake answers
// the kind that cannot decide a claim — an accidental dependency on it would
// then fail rather than quietly pass.
func (m *memLedgerStore) ActorKind(_ context.Context, _ string) (string, error) {
	return "agent", nil
}

func (m *memLedgerStore) InsertRecord(_ context.Context, rec ledger.Record) error {
	m.records = append(m.records, rec)
	return nil
}

func (m *memLedgerStore) GetRecord(_ context.Context, id string) (ledger.Record, error) {
	for _, rec := range m.records {
		if rec.ID == id {
			return rec, nil
		}
	}
	return ledger.Record{}, ledger.ErrRecordNotFound
}

func (m *memLedgerStore) RunRecords(_ context.Context, runID string) ([]ledger.Record, error) {
	var out []ledger.Record
	for _, rec := range m.records {
		if rec.RunID == runID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (m *memLedgerStore) LedgerVersion(_ context.Context, runID string) (int64, error) {
	var n int64
	for _, rec := range m.records {
		if rec.RunID == runID {
			n++
		}
	}
	return n, nil
}

func (m *memLedgerStore) LiveSupersessors(context.Context, string) ([]ledger.Record, error) {
	return nil, nil
}
func (m *memLedgerStore) Lock(context.Context, string) error { return nil }
func (m *memLedgerStore) InsertReviewRequest(context.Context, ledger.ReviewRequest) error {
	return nil
}

func (m *memLedgerStore) GetReviewRequest(context.Context, string) (ledger.ReviewRequest, error) {
	return ledger.ReviewRequest{}, ledger.ErrReviewNotFound
}
func (m *memLedgerStore) MarkReviewCommitted(context.Context, string) (bool, error) {
	return false, nil
}

func (m *memLedgerStore) InTx(ctx context.Context, fn func(context.Context, ledger.Tx) error) error {
	return fn(ctx, m)
}

func TestARecordFromThisPackageIsAcceptedByTheRealLedgerRuntime(t *testing.T) {
	ref := "refs/culture-nodes/run_01/fix-1730000000-abcd"
	origin := originWithHandoverRef(t, ref, map[string]string{"internal/api/server.go": "package api\n"})
	wantSHA := git(t, origin, "rev-parse", ref)

	store := &memLedgerStore{}
	runtime, err := ledger.New(store)
	if err != nil {
		t.Fatalf("build ledger: %v", err)
	}
	observer := newObserver(&handover.GitFetcher{Remote: origin, Timeout: 60 * time.Second}, runtime)

	c := claim()
	c.Ref = ref
	rec, ok := observer.Observe(context.Background(), c)
	if !ok {
		t.Fatal("the real runtime refused a record this package built")
	}
	if len(store.records) != 1 {
		t.Fatalf("expected one stored record, got %d", len(store.records))
	}
	stored := store.records[0]
	if stored.Authority != ledger.AuthorityObserved || stored.Origin.Kind != ledger.OriginRunner {
		t.Fatalf("stored record is %s/%s, want runner/observed", stored.Origin.Kind, stored.Authority)
	}
	if stored.ID == "" || stored.ContentDigest == "" {
		t.Fatal("the runtime did not stamp the record")
	}
	m := measurementsOf(t, stored)
	if m["commit_sha"] != wantSHA {
		t.Fatalf("stored commit %v, want the sha git resolved (%s)", m["commit_sha"], wantSHA)
	}
	if rec.ID != stored.ID {
		t.Fatalf("Observe returned %q but %q was stored", rec.ID, stored.ID)
	}
}

// The same runtime, with an agent-origin record, still refuses to let anything
// promote itself — the refusal half of §10.4 that this package must not have
// weakened by teaching the control plane to write `observed` at all.
func TestNothingHereLetsAnAgentOriginRecordClaimObserved(t *testing.T) {
	rec := ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      "run_01",
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: "codex-thor"},
		Authority:  ledger.AuthorityObserved,
		Data:       json.RawMessage(`{"measurements":{"ref":"refs/culture-nodes/run_01/x"}}`),
	}
	manifest := ledger.RunnerManifest{ActorID: "codex-thor", ObservableFields: []string{"/measurements/ref"}}
	if err := ledger.CheckAuthority(rec, &manifest); err == nil {
		t.Fatal("an agent-origin record claiming observed authority must still be refused")
	}
}
