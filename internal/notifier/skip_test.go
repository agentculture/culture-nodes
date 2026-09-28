package notifier_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/notifier"
	"github.com/agentculture/culture-nodes/internal/notify"
)

// This file is task t40f's (#328): the pr-upkeep sweep runs every five
// minutes and each run posted run.created + run.completed to Discord,
// flooding the channel. Config.SkipWorkflows names workflows whose
// lifecycle events are consumed -- the cursor advances past them -- and
// journaled as skipped, never posted.

// journalRecorder is a notify.JournalFunc that keeps every entry.
type journalRecorder struct {
	mu      sync.Mutex
	entries []notify.JournalEntry
}

func (j *journalRecorder) record(e notify.JournalEntry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, e)
}

func (j *journalRecorder) snapshot() []notify.JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]notify.JournalEntry(nil), j.entries...)
}

func newSkippingDaemon(t *testing.T, fcp *fakeControlPlane, cursorPath string, skip []string, journal notify.JournalFunc) *notifier.Daemon {
	t.Helper()
	cursor, err := notifier.LoadCursor(cursorPath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	d, err := notifier.NewDaemon(notifier.Config{
		APIBase:       fcp.server.URL,
		CursorPath:    cursorPath,
		DashboardBase: "http://dashboard.example",
		SkipWorkflows: skip,
		ReconnectMin:  5 * time.Millisecond,
		ReconnectMax:  20 * time.Millisecond,
		HTTPTimeout:   2 * time.Second,
	}, cursor, notifier.WithJournal(journal))
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	return d
}

var allLifecycleEvents = []string{
	"dev.culture.nodes.run.created",
	"dev.culture.nodes.run.completed",
	"dev.culture.nodes.run.failed",
	"dev.culture.nodes.run.cancelled",
	"dev.culture.nodes.run.bounded",
}

// The compose default mutes a notification declaration's own one-node
// envelope run while leaving unrelated declaration runs visible.
func TestDaemonSkipsNotifyEnvelopeRuns(t *testing.T) {
	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("notification-run", "sha256:notify-envelope")
	fcp.setFiring("notification-run", "notify-action-failed-pr-upkeep-fix")
	fcp.setWorkflow("ordinary-run", "sha256:ordinary-envelope")
	fcp.setFiring("ordinary-run", "pr-upkeep-fix")
	fcp.addEvent("00001", "dev.culture.nodes.run.created", "notification-run")
	fcp.addEvent("00002", "dev.culture.nodes.run.completed", "notification-run")
	fcp.addEvent("00003", "dev.culture.nodes.run.completed", "ordinary-run")
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	d := newSkippingDaemon(t, fcp, cursor, []string{"notify-*"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 1 })
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if posts := wc.snapshot(); len(posts) != 1 || posts[0].RunID != "ordinary-run" {
		t.Fatalf("posts = %+v, want only ordinary-run", posts)
	}
}

// TestDaemonSkipsEveryLifecycleEventOfASkippedWorkflow: an exact-match
// entry mutes all five lifecycle event types of that workflow's run, a
// different workflow still posts, every skipped event is journaled as
// skipped, and the cursor advances past them so a restarted daemon -- even
// one with no skip-list at all -- never reads them again.
func TestDaemonSkipsEveryLifecycleEventOfASkippedWorkflow(t *testing.T) {
	const sweepDigest = "sha256:fc5b77f0000000000000000000000000000000000000000000000000000000000"
	const otherDigest = "sha256:8d4c768f0bde3b02eea9d404046ff646b607a875d9063d13630787267f7d01ab"

	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("sweep-run", sweepDigest)
	fcp.setWorkflowKey(sweepDigest, "pr-upkeep-sweep-cycle")
	fcp.setWorkflow("other-run", otherDigest)
	fcp.setWorkflowKey(otherDigest, "parallel-live-proof")

	for i, eventType := range allLifecycleEvents {
		fcp.addEvent("0000"+string(rune('1'+i)), eventType, "sweep-run")
	}
	fcp.addEvent("00006", "dev.culture.nodes.run.created", "other-run")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	journal := &journalRecorder{}
	d := newSkippingDaemon(t, fcp, cursorPath, []string{"pr-upkeep-sweep-cycle"}, journal.record)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 1 })
	// Give a wrongly-posted sweep event a moment to arrive before asserting.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	posts := wc.snapshot()
	if len(posts) != 1 || posts[0].RunID != "other-run" {
		t.Fatalf("posts = %+v, want exactly one, for other-run (the sweep's five lifecycle events are skipped)", posts)
	}

	skipped := map[string]bool{}
	for _, e := range journal.snapshot() {
		if e.Outcome == notifier.OutcomeSkipped {
			if e.RunID != "sweep-run" {
				t.Errorf("journaled a skip for run %q, want only sweep-run", e.RunID)
			}
			skipped[e.Event] = true
		}
	}
	for _, eventType := range allLifecycleEvents {
		if !skipped[eventType] {
			t.Errorf("%s of the skipped workflow was not journaled as skipped (journal: %+v)", eventType, journal.snapshot())
		}
	}

	cursor, err := notifier.LoadCursor(cursorPath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got := cursor.Last(); got != "00006" {
		t.Fatalf("cursor.Last() = %q, want 00006 (the cursor advances past skipped events)", got)
	}

	// A restart with no skip-list must not read the skipped events again.
	d2 := newSkippingDaemon(t, fcp, cursorPath, nil, nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = d2.Run(ctx2); close(done2) }()
	time.Sleep(150 * time.Millisecond)
	cancel2()
	<-done2
	if got := wc.count(); got != 1 {
		t.Fatalf("after restart: %d posts, want still 1 (skipped events are consumed, never re-read)", got)
	}
}

// TestDaemonSkipListPrefixMatchesTheDeclarationSweep: an entry ending in
// `*` is a prefix. `pr-upkeep-sweep*` mutes both the graph sweep
// (workflow key pr-upkeep-sweep-cycle) and the declaration-lane sweep,
// whose envelope run the run view names after its declaration
// (pr-upkeep-sweep) -- while a neighbour that does not share the prefix
// (pr-upkeep-swept) still posts.
func TestDaemonSkipListPrefixMatchesTheDeclarationSweep(t *testing.T) {
	const sweepDigest = "sha256:fc5b77f0000000000000000000000000000000000000000000000000000000000"

	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("graph-sweep", sweepDigest)
	fcp.setWorkflowKey(sweepDigest, "pr-upkeep-sweep-cycle")
	fcp.setWorkflow("decl-sweep", "sha256:envelope-1")
	fcp.setFiring("decl-sweep", "pr-upkeep-sweep")
	fcp.setWorkflow("decl-swept", "sha256:envelope-2")
	fcp.setFiring("decl-swept", "pr-upkeep-swept")

	fcp.addEvent("00001", "dev.culture.nodes.run.created", "graph-sweep")
	fcp.addEvent("00002", "dev.culture.nodes.run.completed", "graph-sweep")
	fcp.addEvent("00003", "dev.culture.nodes.run.created", "decl-sweep")
	fcp.addEvent("00004", "dev.culture.nodes.run.completed", "decl-sweep")
	fcp.addEvent("00005", "dev.culture.nodes.run.completed", "decl-swept")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	d := newSkippingDaemon(t, fcp, cursorPath, []string{"pr-upkeep-sweep*"}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 1 })
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	posts := wc.snapshot()
	if len(posts) != 1 || posts[0].RunID != "decl-swept" {
		t.Fatalf("posts = %+v, want exactly one, for decl-swept", posts)
	}
	if posts[0].Workflow != "pr-upkeep-swept (envelop)" {
		t.Errorf("Workflow = %q, want the declaration name with a short digest", posts[0].Workflow)
	}
}

// TestDaemonWithAnEmptySkipListPostsEverything: no skip-list is today's
// behaviour -- the sweep's own events post like any other run's.
func TestDaemonWithAnEmptySkipListPostsEverything(t *testing.T) {
	const sweepDigest = "sha256:fc5b77f0000000000000000000000000000000000000000000000000000000000"

	t.Setenv(envPrimary, "")
	fcp := newFakeControlPlane(t)
	wc := newWebhookCapture(t)
	t.Setenv(envPrimary, wc.server.URL)
	fcp.setWorkflow("sweep-run", sweepDigest)
	fcp.setWorkflowKey(sweepDigest, "pr-upkeep-sweep-cycle")
	fcp.addEvent("00001", "dev.culture.nodes.run.created", "sweep-run")
	fcp.addEvent("00002", "dev.culture.nodes.run.completed", "sweep-run")

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	d := newSkippingDaemon(t, fcp, cursorPath, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return wc.count() >= 2 })
	cancel()
	<-done
	if got := wc.count(); got != 2 {
		t.Fatalf("%d posts, want 2", got)
	}
}
