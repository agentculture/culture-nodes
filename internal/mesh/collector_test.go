package mesh

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectorCachesDeploymentAndObservedAt(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"preflight":{"host":{"deployment":{"version":"1.2.3"}}}}`))
	}))
	defer bridge.Close()

	c := New(Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 1})
	c.SetTargets([]Target{{Key: "bridge-a", URL: bridge.URL}})
	c.Collect(context.Background())
	got, ok := c.Snapshot()["bridge-a"]
	if !ok || got.ObservedAt.IsZero() || got.Error != "" || string(got.Deployment) != `{"version":"1.2.3"}` {
		t.Fatalf("snapshot = %#v, want cached deployment with observed_at", got)
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

func TestRunCollectsOnTimer(t *testing.T) {
	var calls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"preflight":{"host":{"deployment":{}}}}`))
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
