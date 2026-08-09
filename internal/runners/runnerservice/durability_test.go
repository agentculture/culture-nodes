package runnerservice_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/runnerservice"
)

// The retention promise the protocol makes is about the completion path
// itself: "Never let an operation's status disappear before that retention
// elapses." A process restart is the obvious way a status disappears, so the
// deployment store is on disk and the memory store says out loud that it is
// not.

func TestTheMemoryStoreDeclaresItIsNotDurable(t *testing.T) {
	if runnerservice.NewMemoryStore().Durable() {
		t.Fatal("the in-memory store claims durability it cannot deliver across a restart")
	}
}

func TestTheFileStoreDeclaresItIsDurable(t *testing.T) {
	store, err := runnerservice.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if !store.Durable() {
		t.Fatal("the file store does not declare durability")
	}
}

func newFileStoreService(t *testing.T, dir string, runner runners.Runner) (*runnerservice.Service, *httptest.Server) {
	t.Helper()
	store, err := runnerservice.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc, err := runnerservice.New(runnerservice.Config{
		Runner:  runner,
		Store:   store,
		Secret:  testSecret,
		OnError: func(err error) { t.Logf("service diagnostic: %v", err) },
	})
	if err != nil {
		t.Fatalf("runnerservice.New: %v", err)
	}
	server := httptest.NewServer(svc.Handler())
	return svc, server
}

// A terminal status written by one process is readable by the next one, which
// is what "durably enough to answer the status endpoint for at least the
// declared retention" means when the process can restart inside that window.
func TestATerminalStatusSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	op := testOperation("op-durable-1")

	svc, server := newFileStoreService(t, dir, completingRunner())
	h := &harness{svc: svc, server: server}
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, body)
	}
	before := h.waitTerminal(t, op.OperationID)
	_, beforeBody := h.status(t, op.OperationID, testSecret)
	server.Close()
	svc.Close()

	// A second process over the same state directory: same status, no
	// re-execution, nothing forgotten.
	svc2, server2 := newFileStoreService(t, dir, runnerFunc(
		func(_ context.Context, _ runners.Operation) (runners.Result, error) {
			t.Error("the restarted service re-executed an operation it had already finished")
			return runners.Result{}, nil
		}))
	defer func() {
		server2.Close()
		svc2.Close()
	}()
	h2 := &harness{svc: svc2, server: server2}

	code, afterBody := h2.status(t, op.OperationID, testSecret)
	if code != http.StatusOK {
		t.Fatalf("status after restart answered %d, want 200: %s", code, afterBody)
	}
	var after runners.OperationStatus
	if err := json.Unmarshal(afterBody, &after); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if after.State != before.State {
		t.Fatalf("state after restart = %s, want the recorded %s", after.State, before.State)
	}
	if string(beforeBody) != string(afterBody) {
		t.Errorf("the terminal status changed across a restart\nbefore: %s\nafter:  %s", beforeBody, afterBody)
	}
}

// An operation that was in flight when the process died cannot be reported as
// running (the runner no longer holds it) and cannot be reported as completed
// (nothing observed its outcome). It becomes terminal with an error that says
// the outcome was never observed, and observations that claim nothing.
func TestAnInterruptedOperationBecomesTerminalWithNothingMeasured(t *testing.T) {
	dir := t.TempDir()
	op := testOperation("op-interrupted-1")

	blocked := make(chan struct{})
	started := make(chan struct{})
	svc, server := newFileStoreService(t, dir, runnerFunc(func(_ context.Context, o runners.Operation) (runners.Result, error) {
		close(started)
		<-blocked
		return completedResult(o), nil
	}))
	h := &harness{svc: svc, server: server}
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, body)
	}
	<-started
	// Simulate a crash: drop the HTTP surface without letting the worker
	// finish, leaving a `running` record on disk.
	server.Close()

	svc2, server2 := newFileStoreService(t, dir, completingRunner())
	defer func() {
		server2.Close()
		svc2.Close()
		close(blocked)
		svc.Close()
	}()
	h2 := &harness{svc: svc2, server: server2}

	code, body := h2.status(t, op.OperationID, testSecret)
	if code != http.StatusOK {
		t.Fatalf("status after restart answered %d, want 200: %s", code, body)
	}
	var observed runners.OperationStatus
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if err := observed.Validate(); err != nil {
		t.Fatalf("the recovered status envelope is invalid: %v\n%s", err, body)
	}
	if !observed.Terminal() {
		t.Fatalf("state = %s after a restart; the runner no longer holds the work, so it cannot report running",
			observed.State)
	}
	if observed.State != runners.StateFailed {
		t.Fatalf("state = %s, want failed", observed.State)
	}
	if observed.Result.Error == nil || observed.Result.Error.Kind != runners.ErrorRunnerUnavailable {
		t.Fatalf("recovered result error = %+v, want kind runner_unavailable", observed.Result.Error)
	}
	for _, name := range []string{"exit_status", "changed_paths", "logs", "resource_usage"} {
		obs, _ := observed.Result.Observations.Get(name)
		if obs.Measured || obs.Complete {
			t.Errorf("recovered observation %q claims measured=%v complete=%v; nothing observed the outcome",
				name, obs.Measured, obs.Complete)
		}
		if obs.Note == "" {
			t.Errorf("recovered observation %q gives no note explaining why it is empty", name)
		}
	}
}

func TestAFileStoreRefusesToStartOverAnUnreadableRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-a-record.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("seed a corrupt record: %v", err)
	}
	if _, err := runnerservice.NewFileStore(dir); err == nil {
		t.Fatal("NewFileStore accepted a corrupt state directory; silently forgetting an operation makes its outcome unlearnable")
	}
}

func TestNewRefusesARetentionBelowTheProtocolMinimum(t *testing.T) {
	_, err := runnerservice.New(runnerservice.Config{
		Runner:          completingRunner(),
		Store:           runnerservice.NewMemoryStore(),
		Secret:          testSecret,
		StatusRetention: time.Minute,
	})
	if err == nil {
		t.Fatalf("New accepted a %s retention; the protocol minimum is %s", time.Minute, runners.MinStatusRetention)
	}
}
