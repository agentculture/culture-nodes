package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Task t10 / issue #13, wired end to end: a real PostgreSQL, a real engine, a
// real HTTP actor reporting a §13.2/§13.4 handover block, and a REAL git
// repository the control plane fetches from.
//
// The gap being closed, measured rather than assumed: internal/runners/
// dispatch.go's buildEvidence can turn a runner's answer into an `observed`
// record, and both shipped runners say in their own package docs that they
// cannot answer — so no production run had ever carried an evidence record.
// An agent node makes it sharper still: it produces no runners.Result at all,
// only the actor's own claims, which §10.4 caps at `proposed`.
//
// The load-bearing assertion in this file is the NEGATIVE one
// (TestAnAgentClaimingSuccessWithNoFetchableRefProducesNoObservedRecord): no
// fetchable ref must mean NO record, not a record marked unmeasured.

func handoverGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// handoverOrigin builds the repository the control plane fetches from, with
// one handover ref in it — reachable from no branch, exactly as a bridge's
// preserve.handover_ref leaves one. Returns the remote path and the sha the
// ref really points at, which is the value the ledger record must carry.
func handoverOrigin(t *testing.T, ref string, changed []string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	handoverGit(t, dir, "init", "--quiet", "--initial-branch=main")
	handoverGit(t, dir, "config", "user.email", "t10@example.com")
	handoverGit(t, dir, "config", "user.name", "t10")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# base\n"), 0o600); err != nil {
		t.Fatalf("seed the origin: %v", err)
	}
	handoverGit(t, dir, "add", "README.md")
	handoverGit(t, dir, "commit", "--quiet", "-m", "base")

	for _, name := range changed {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("delivered\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	handoverGit(t, dir, "add", "-A")
	handoverGit(t, dir, "commit", "--quiet", "-m", "handover")
	sha := handoverGit(t, dir, "rev-parse", "HEAD")
	handoverGit(t, dir, "update-ref", ref, sha)
	handoverGit(t, dir, "reset", "--quiet", "--hard", "HEAD~1")
	return dir, sha
}

// withHandoverObserver wires a real handover.Observer — a real GitFetcher
// over *remote*, and the real ledger runtime over the harness's own store —
// into both terminal paths (see newClockedHarness).
//
// The measuring identity is a registered actors row because
// ledger_records.origin_actor_id has a foreign key to actors(id): even the
// control plane's own observation is attributed to an identity somebody
// registered, exactly as the dispatch gate's producer is.
func withHandoverObserver(t *testing.T, remote string) harnessOption {
	t.Helper()
	return func(o *worker.Options) {
		actorID := "handover-fetch-" + idstore.NewULID()
		if _, err := testStore.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
			VALUES ($1, $2, $3, 1, 'runner', 'internal')
		`, actorID, o.NamespaceID, actorID); err != nil {
			t.Fatalf("register the handover measuring identity: %v", err)
		}
		runtime, err := storepg.NewLedger(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewLedger: %v", err)
		}
		o.Handover = &handover.Observer{
			Fetcher:       &handover.GitFetcher{Remote: remote, Timeout: 60 * time.Second},
			Ledger:        runtime,
			ActorID:       actorID,
			ActorRevision: "test",
			OnError:       func(err error) { t.Logf("handover observer: %v", err) },
		}
	}
}

// observedRecords returns the run's evidence records that carry `observed`
// authority — the thing this whole task exists to make exist.
func observedRecords(t *testing.T, h *harness, runID string) []ledger.Record {
	t.Helper()
	runtime, err := storepg.NewLedger(testStore, h.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	all, err := runtime.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger records: %v", err)
	}
	var out []ledger.Record
	for _, rec := range all {
		if rec.Authority == ledger.AuthorityObserved {
			out = append(out, rec)
		}
	}
	return out
}

func measurements(t *testing.T, rec ledger.Record) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode record payload: %v", err)
	}
	m, ok := data["measurements"].(map[string]any)
	if !ok {
		t.Fatalf("record carries no measurements: %s", rec.Data)
	}
	return m
}

// syncResultWithHandover is the §13.2 body a bridge sends after a successful
// dispatch that asked for a handover — the block adapters/*/src/*/server.py
// now attaches, field for field from its own HandoverResult.to_dict().
func syncResultWithHandover(w http.ResponseWriter, ref, claimedCommit string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{
		"outcome":"completed",
		"output":{"score":0.91,"summary":"fixed it"},
		"ledger_delta":{"records":[]},
		"handover":{
			"attempted":true,
			"created":true,
			"ref":%q,
			"commit":%q,
			"remote":"origin",
			"handle":{"kind":"git_ref","ref":"git+ssh://example.invalid/repo#%s","commit":%q,"publication":"pending","media_type":"application/vnd.culture-nodes.git-commit"},
			"missing_capability":null,
			"reason":null
		}
	}`, ref, claimedCommit, ref, claimedCommit)
}

// ---------------------------------------------------------------------------
// acceptance 1: a handed-over ref is recorded as observed evidence
// ---------------------------------------------------------------------------

func TestAHandedOverRefIsRecordedAsObservedEvidenceCitingTheRefAndItsCommit(t *testing.T) {
	ref := "refs/culture-nodes/run_t10/fix-1730000000-abcd"
	origin, realSHA := handoverOrigin(t, ref, []string{"internal/engine/workflow.go", "CHANGELOG.md"})

	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		syncResultWithHandover(w, ref, realSHA)
	}, withHandoverObserver(t, origin))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if final := h.run(run.ID); final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}

	records := observedRecords(t, h, run.ID)
	if len(records) != 1 {
		t.Fatalf("expected exactly one observed record, got %d", len(records))
	}
	rec := records[0]
	if rec.RecordType != ledger.RecordEvidence || rec.Origin.Kind != ledger.OriginRunner {
		t.Fatalf("record is %s/%s, want evidence from a runner origin", rec.RecordType, rec.Origin.Kind)
	}
	if rec.AttemptID.String() == "" {
		t.Error("the observation is not stamped against the attempt it describes")
	}

	m := measurements(t, rec)
	if m["ref"] != ref {
		t.Errorf("record cites ref %v, want %v", m["ref"], ref)
	}
	if m["commit_sha"] != realSHA {
		t.Errorf("record cites commit %v, want the sha git resolved (%s)", m["commit_sha"], realSHA)
	}
	paths, _ := json.Marshal(m["changed_paths"])
	if !bytes.Contains(paths, []byte("internal/engine/workflow.go")) ||
		!bytes.Contains(paths, []byte("CHANGELOG.md")) {
		t.Errorf("record does not carry the paths the commit changed: %s", paths)
	}
	if m["source_remote"] != origin {
		t.Errorf("record names %v as its source, want the remote the CONTROL PLANE is configured with (%s)",
			m["source_remote"], origin)
	}
}

// ---------------------------------------------------------------------------
// acceptance 2 (load-bearing): a success claim with no fetchable ref writes
// NOTHING — not a record marked unmeasured
// ---------------------------------------------------------------------------

func TestAnAgentClaimingSuccessWithNoFetchableRefProducesNoObservedRecord(t *testing.T) {
	origin, _ := handoverOrigin(t, "refs/culture-nodes/run_t10/something-else", []string{"a.go"})
	claimed := "refs/culture-nodes/run_t10/never-published"

	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		// The agent says it succeeded AND names a ref. Neither is true of
		// anything the remote holds.
		syncResultWithHandover(w, claimed, "1111111111111111111111111111111111111111")
	}, withHandoverObserver(t, origin))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	// The run still completes: the agent's outcome is the agent's outcome,
	// and a missing handover is not an engine failure.
	if final := h.run(run.ID); final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed", final.State)
	}
	if records := observedRecords(t, h, run.ID); len(records) != 0 {
		t.Fatalf("expected NO observed record for an unfetchable ref, got %d: %s",
			len(records), records[0].Data)
	}
}

func TestADispatchThatHandedNothingOverProducesNoObservedRecord(t *testing.T) {
	origin, _ := handoverOrigin(t, "refs/culture-nodes/run_t10/unused", []string{"a.go"})

	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"no handover here"}`)
	}, withHandoverObserver(t, origin))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if records := observedRecords(t, h, run.ID); len(records) != 0 {
		t.Fatalf("expected NO observed record when nothing was handed over, got %d", len(records))
	}
}

// ---------------------------------------------------------------------------
// acceptance 3: what is recorded is what the control plane measured
// ---------------------------------------------------------------------------

func TestTheRecordCarriesTheFetchedCommitNotTheOneTheAgentReported(t *testing.T) {
	ref := "refs/culture-nodes/run_t10/fix-1730000000-beef"
	origin, realSHA := handoverOrigin(t, ref, []string{"internal/api/server.go"})
	// The agent reports a DIFFERENT commit for the same ref, and a file it
	// never touched. Both are wrong, and neither may reach the ledger.
	lyingSHA := "9999999999999999999999999999999999999999"

	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		syncResultWithHandover(w, ref, lyingSHA)
	}, withHandoverObserver(t, origin))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	records := observedRecords(t, h, run.ID)
	if len(records) != 1 {
		t.Fatalf("expected one observed record, got %d", len(records))
	}
	body := string(records[0].Data)
	if strings.Contains(body, lyingSHA) {
		t.Fatalf("the observed record repeated the agent's reported commit: %s", body)
	}
	if !strings.Contains(body, realSHA) {
		t.Fatalf("the observed record does not carry the commit the fetch resolved (%s): %s", realSHA, body)
	}
}

// ---------------------------------------------------------------------------
// the asynchronous path — the one production actually dispatches on
// ---------------------------------------------------------------------------

// asyncActor answers 202 and captures the §13.1 callback the control plane
// handed it, so the test can report the terminal event itself rather than
// racing a goroutine inside the actor handler.
func asyncActor(captured *actors.Callback, mu *sync.Mutex) func(*harness, http.ResponseWriter, actors.InvocationRequest) {
	return func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		mu.Lock()
		*captured = req.Callback
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"invocation_id":"inv-handover","heartbeat_after_seconds":30,"supports_cancellation":true}`)
	}
}

// postTerminal sends one §13.4 `completed` event through the REAL callback
// ingest handler the harness mounts — the same route a deployed bridge posts
// to, so the handover block travels the production path.
func postTerminal(t *testing.T, callback actors.Callback, payload string) string {
	t.Helper()
	event, _ := json.Marshal(actors.CallbackEvent{
		EventID:  "ev-handover-terminal",
		Sequence: 1,
		Kind:     actors.EventCompleted,
		Payload:  json.RawMessage(payload),
	})
	req, err := http.NewRequest(http.MethodPost, callback.URL, bytes.NewReader(event))
	if err != nil {
		t.Fatalf("build callback request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+callback.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("callback answered %d, want 202", resp.StatusCode)
	}
	var decoded struct {
		Disposition string `json:"disposition"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return decoded.Disposition
}

func TestAnAsyncCompletedEventsHandoverRefIsMeasuredToo(t *testing.T) {
	ref := "refs/culture-nodes/run_t10/async-1730000000-cafe"
	origin, realSHA := handoverOrigin(t, ref, []string{"internal/actors/callback.go"})

	var (
		mu       sync.Mutex
		callback actors.Callback
	)
	h := newHarness(t, asyncActor(&callback, &mu), withHandoverObserver(t, origin))

	run := h.createRun("async.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return callback.URL != ""
	})

	mu.Lock()
	cb := callback
	mu.Unlock()
	// The bridge reports a commit of its own. It is never read: the ledger
	// must carry what the fetch resolves, which is realSHA.
	if got := postTerminal(t, cb, fmt.Sprintf(`{
		"outcome":"completed",
		"output":{"summary":"fixed it late"},
		"ledger_delta":{"records":[]},
		"handover":{"attempted":true,"created":true,"ref":%q,
			"commit":"0000000000000000000000000000000000000000","remote":"origin",
			"handle":{"kind":"git_ref","ref":"git+ssh://example.invalid/repo#%s","commit":"0000000000000000000000000000000000000000","publication":"pending","media_type":"application/vnd.culture-nodes.git-commit"},
			"missing_capability":null,"reason":null}
	}`, ref, ref)); got != string(actors.DispositionCommitted) {
		t.Fatalf("terminal callback disposition = %q, want committed", got)
	}

	records := observedRecords(t, h, run.ID)
	if len(records) != 1 {
		t.Fatalf("expected one observed record from the async path, got %d", len(records))
	}
	m := measurements(t, records[0])
	if m["commit_sha"] != realSHA {
		t.Errorf("async record cites commit %v, want the FETCHED sha %s", m["commit_sha"], realSHA)
	}
	if m["ref"] != ref {
		t.Errorf("async record cites ref %v, want %v", m["ref"], ref)
	}
}

func TestAnAsyncClaimWithNoFetchableRefProducesNoObservedRecord(t *testing.T) {
	origin, _ := handoverOrigin(t, "refs/culture-nodes/run_t10/real", []string{"a.go"})
	claimed := "refs/culture-nodes/run_t10/async-never-published"

	var (
		mu       sync.Mutex
		callback actors.Callback
	)
	h := newHarness(t, asyncActor(&callback, &mu), withHandoverObserver(t, origin))

	run := h.createRun("async.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return callback.URL != ""
	})

	mu.Lock()
	cb := callback
	mu.Unlock()
	if got := postTerminal(t, cb, fmt.Sprintf(`{
		"outcome":"completed",
		"output":{"summary":"I promise I did the work"},
		"ledger_delta":{"records":[]},
		"handover":{"attempted":true,"created":true,"ref":%q,"commit":"abc","remote":"origin",
			"handle":null,"missing_capability":null,"reason":null}
	}`, claimed)); got != string(actors.DispositionCommitted) {
		t.Fatalf("terminal callback disposition = %q, want committed", got)
	}

	// The completion committed — the agent's outcome is its own — and the
	// ledger carries no observation, because nothing was observed.
	if final := h.run(run.ID); final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed", final.State)
	}
	if records := observedRecords(t, h, run.ID); len(records) != 0 {
		t.Fatalf("expected NO observed record, got %d: %s", len(records), records[0].Data)
	}
}

// ---------------------------------------------------------------------------
// the default: a deployment with no observer behaves exactly as before
// ---------------------------------------------------------------------------

func TestWithNoObserverConfiguredAHandoverBlockChangesNothing(t *testing.T) {
	ref := "refs/culture-nodes/run_t10/ignored"
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		syncResultWithHandover(w, ref, "abc123")
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if final := h.run(run.ID); final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed", final.State)
	}
	if records := observedRecords(t, h, run.ID); len(records) != 0 {
		t.Fatalf("a worker with no handover observer wrote %d observed records", len(records))
	}
}
