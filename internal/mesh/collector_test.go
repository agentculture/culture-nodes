package mesh

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectorCachesDeploymentAndObservedAt(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"thor","deployment":{"version":"1.2.3"}}}}`))
	}))
	defer bridge.Close()

	c := New(Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 1})
	c.SetTargets([]Target{{Key: "bridge-a", URL: bridge.URL}})
	c.Collect(context.Background())
	got, ok := c.Snapshot()["bridge-a"]
	if !ok || got.Hostname != "thor" || got.ObservedAt.IsZero() || got.Error != "" || string(got.Deployment) != `{"version":"1.2.3"}` {
		t.Fatalf("snapshot = %#v, want cached deployment with observed_at", got)
	}
}

func TestCollectorKeysSuccessfulBridgeByReportedHostnameAndKeepsTimeoutUnknown(t *testing.T) {
	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"reported-host","deployment":{}}}}`))
	}))
	defer answering.Close()
	timingOut := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer timingOut.Close()

	c := New(Config{ProbeTimeout: 20 * time.Millisecond, MaxConcurrency: 2})
	c.SetTargets([]Target{{Key: "answering", URL: answering.URL}, {Key: "timing-out", URL: timingOut.URL}})
	c.Collect(context.Background())
	got := c.Snapshot()
	if got["answering"].Hostname != "reported-host" || got["answering"].ObservedAt.IsZero() {
		t.Fatalf("answering observation = %#v", got["answering"])
	}
	if got["timing-out"].Hostname != "" || got["timing-out"].Error == "" || got["timing-out"].ObservedAt.IsZero() {
		t.Fatalf("timed-out observation = %#v, want unknown hostname, error, and observed_at", got["timing-out"])
	}
}

func TestCollectorTimeoutConcurrencyAndFailureCount(t *testing.T) {
	var active, maximum atomic.Int32
	blocked := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		<-r.Context().Done()
	}))
	defer blocked.Close()

	var logs bytes.Buffer
	c := New(Config{
		Interval:       time.Hour,
		ProbeTimeout:   20 * time.Millisecond,
		MaxConcurrency: 2,
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
	})
	c.SetTargets([]Target{{Key: "a", URL: blocked.URL}, {Key: "b", URL: blocked.URL}, {Key: "c", URL: blocked.URL}})
	started := time.Now()
	c.Collect(context.Background())
	if elapsed := time.Since(started); elapsed > 90*time.Millisecond {
		t.Fatalf("collection took %s; per-probe timeout was not enforced", elapsed)
	}
	if maximum.Load() > 2 {
		t.Fatalf("maximum concurrency = %d, want <= 2", maximum.Load())
	}
	for _, key := range []string{"a", "b", "c"} {
		if c.FailureCount(key) != 1 {
			t.Errorf("failure count for %s = %d, want 1", key, c.FailureCount(key))
		}
	}
	if logs.Len() == 0 {
		t.Fatal("probe failures were not logged")
	}
}

func TestCollectorClassifiesUnobservedUnsupportedAndFailed(t *testing.T) {
	var unobservedCalls atomic.Int32
	unobservedBridge := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		unobservedCalls.Add(1)
	}))
	defer unobservedBridge.Close()
	unsupportedBridge := httptest.NewServer(http.NotFoundHandler())
	defer unsupportedBridge.Close()
	failedBridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer failedBridge.Close()

	var logs bytes.Buffer
	c := New(Config{ProbeTimeout: time.Second, MaxConcurrency: 3, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	c.SetTargets([]Target{
		{Key: "unobserved", URL: unobservedBridge.URL, Error: "unobserved: no bearer configured"},
		{Key: "unsupported", URL: unsupportedBridge.URL},
		{Key: "failed", URL: failedBridge.URL},
	})
	c.Collect(context.Background())
	c.Collect(context.Background())

	got := c.Snapshot()
	if unobservedCalls.Load() != 0 || got["unobserved"].Class != "unobserved" || got["unobserved"].ObservedAt.IsZero() {
		t.Fatalf("unobserved = %#v, calls = %d", got["unobserved"], unobservedCalls.Load())
	}
	if got["unsupported"].Class != "unsupported" || got["unsupported"].Reason != "GET capabilities: 404 Not Found" || got["unsupported"].Error != got["unsupported"].Reason {
		t.Fatalf("unsupported = %#v", got["unsupported"])
	}
	if got["failed"].Class != "failed" || got["failed"].FailureCount != 2 || c.FailureCount("failed") != 2 {
		t.Fatalf("failed = %#v, count = %d", got["failed"], c.FailureCount("failed"))
	}
	if c.FailureCount("unobserved") != 0 || c.FailureCount("unsupported") != 0 {
		t.Fatalf("non-failure counts = %d/%d", c.FailureCount("unobserved"), c.FailureCount("unsupported"))
	}
	if strings.Count(logs.String(), "class=unsupported") != 1 || strings.Count(logs.String(), "class=failed") != 2 {
		t.Fatalf("logs did not report unsupported once and failed per tick:\n%s", logs.String())
	}
}

func TestCollectorClassifiesCapabilitiesWithoutHostAsUnsupported(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"verbs":["post_comment"]}`))
	}))
	defer bridge.Close()
	c := New(Config{ProbeTimeout: time.Second, MaxConcurrency: 1})
	c.SetTargets([]Target{{Key: "jira", URL: bridge.URL}})
	c.Collect(context.Background())
	got := c.Snapshot()["jira"]
	if got.Class != "unsupported" || got.Reason != "capabilities has no preflight.host.hostname" || c.FailureCount("jira") != 0 {
		t.Fatalf("observation = %#v, failure count = %d", got, c.FailureCount("jira"))
	}
}

func TestRunCollectsOnTimer(t *testing.T) {
	var calls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"thor","deployment":{}}}}`))
	}))
	defer bridge.Close()
	c := New(Config{Interval: 10 * time.Millisecond, ProbeTimeout: time.Second, MaxConcurrency: 1})
	c.SetTargets([]Target{{Key: "a", URL: bridge.URL}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	deadline := time.After(time.Second)
	for calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("collector did not run on its timer")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
