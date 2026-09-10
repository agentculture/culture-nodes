package mesh

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
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
	// Concurrency is measured by request START times, not by handlers alive:
	// the collector releases a slot the moment its client gives up (the
	// probe timeout), while the server-side handler only learns of the
	// disconnect afterwards, so counting live handlers over-reports by one
	// exactly at the timeout boundary. What the bound guarantees is that the
	// third probe cannot start before the first probe has timed out.
	var startMu sync.Mutex
	var starts []time.Time
	blocked := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		startMu.Lock()
		starts = append(starts, time.Now())
		startMu.Unlock()
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
	startMu.Lock()
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	startMu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("probe starts = %d, want 3", len(starts))
	}
	if gap := starts[2].Sub(starts[0]); gap < 15*time.Millisecond {
		t.Fatalf("third probe started %s after the first; with MaxConcurrency 2 it must wait for a 20ms probe timeout", gap)
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

// #295: the reference bridges are single-threaded and serve one connection
// at a time; a poller that keeps its connection alive between polls holds
// the accept loop and every other client hangs. The collector must release
// the bridge after each probe -- both by default (no keep-alive pool) and
// per request (Connection: close), so a caller-supplied client cannot
// reintroduce the starvation.
func TestCollectorProbeReleasesSingleThreadedBridge(t *testing.T) {
	// A bridge shaped like the adapters: one connection served at a time,
	// keep-alive honoured until the peer closes. Serving happens in the
	// accept loop, so a held connection blocks the next accept exactly as
	// http.server.HTTPServer does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan string, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			reader := bufio.NewReader(conn)
			for {
				req, err := http.ReadRequest(reader)
				if err != nil {
					break
				}
				served <- req.RemoteAddr
				body := `{"preflight":{"host":{"hostname":"thor","deployment":{"revision":"abc"}}}}`
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				if req.Close {
					break
				}
			}
			conn.Close()
		}
	}()

	c := New(Config{ProbeTimeout: time.Second, MaxConcurrency: 1})
	c.SetTargets([]Target{{Key: "codex-thor", URL: "http://" + ln.Addr().String()}})
	c.Collect(context.Background())
	if got := c.Snapshot()["codex-thor"]; got.Hostname != "thor" {
		t.Fatalf("probe did not observe the bridge: %+v", got)
	}
	<-served

	// A second, unrelated client -- its own transport, as curl or the worker
	// would be, never the poller's pool -- must be served promptly: the
	// probe's connection is gone, so the accept loop is free.
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{}}
	resp, err := client.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("second client starved behind the collector's connection: %v", err)
	}
	resp.Body.Close()

	// And a caller-supplied keep-alive client is still forced to close per
	// request, so the starvation cannot come back through Config.HTTPClient.
	keepAlive := New(Config{ProbeTimeout: time.Second, MaxConcurrency: 1, HTTPClient: &http.Client{Transport: &http.Transport{}}})
	keepAlive.SetTargets([]Target{{Key: "codex-thor", URL: "http://" + ln.Addr().String()}})
	keepAlive.Collect(context.Background())
	<-served
	resp, err = client.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("second client starved behind a caller-supplied keep-alive client: %v", err)
	}
	resp.Body.Close()
}

// A recording sink for the liveness fact (plan loop-closure t10, spec c23):
// what the collector would persist for the worker to read.
type recordingLivenessSink struct {
	mu    sync.Mutex
	facts map[string][]Liveness
	err   error
}

func (s *recordingLivenessSink) RecordLiveness(_ context.Context, key string, fact Liveness) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.facts == nil {
		s.facts = map[string][]Liveness{}
	}
	s.facts[key] = append(s.facts[key], fact)
	return s.err
}

func TestCollectorParsesTheLivenessFactAndPersistsItThroughTheSink(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"thor","deployment":{"version":"1.2.3"},` +
			`"liveness":{"session_ok":false,"reason":"refresh_token_spent","checked_at":"2026-09-07T10:00:00Z","mode":"LOCK"}}}}`))
	}))
	defer bridge.Close()
	unmeasured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"orin","deployment":{},` +
			`"liveness":{"session_ok":null,"reason":"unmeasured","checked_at":"2026-09-07T10:00:00Z","mode":"CHECK"}}}}`))
	}))
	defer unmeasured.Close()
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"spark","deployment":{}}}}`))
	}))
	defer silent.Close()

	sink := &recordingLivenessSink{}
	c := New(Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 1, Liveness: sink})
	c.SetTargets([]Target{{Key: "bridge-a", URL: bridge.URL}, {Key: "bridge-b", URL: unmeasured.URL}, {Key: "bridge-c", URL: silent.URL}})
	c.Collect(context.Background())

	snap := c.Snapshot()
	a := snap["bridge-a"].Liveness
	if a == nil || a.SessionOK == nil || *a.SessionOK || a.Reason != "refresh_token_spent" || a.Mode != "LOCK" || a.CheckedAt.IsZero() {
		t.Fatalf("bridge-a liveness = %#v, want session_ok=false reason=refresh_token_spent mode=LOCK", a)
	}
	if snap["bridge-a"].Hostname != "thor" || snap["bridge-a"].Class != "" {
		t.Fatalf("a liveness fact must not change the deployment observation: %#v", snap["bridge-a"])
	}
	b := snap["bridge-b"].Liveness
	if b == nil || b.SessionOK != nil || b.Reason != "unmeasured" {
		t.Fatalf("bridge-b liveness = %#v, want session_ok=null (unmeasured is neither true nor false)", b)
	}
	if snap["bridge-c"].Liveness != nil {
		t.Fatalf("bridge-c advertised no liveness and got %#v; absent stays absent", snap["bridge-c"].Liveness)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if got := sink.facts["bridge-a"]; len(got) != 1 || got[0].Reason != "refresh_token_spent" {
		t.Fatalf("sink saw %v for bridge-a, want exactly the one fact", got)
	}
	if got := sink.facts["bridge-b"]; len(got) != 1 || got[0].SessionOK != nil {
		t.Fatalf("sink saw %v for bridge-b, want the unmeasured fact with a nil session_ok", got)
	}
	if _, saw := sink.facts["bridge-c"]; saw {
		t.Fatal("sink was handed a fact for a bridge that advertised none")
	}
}

func TestCollectorTreatsAMalformedLivenessBlockAsAbsentAndKeepsTheObservation(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"thor","deployment":{},"liveness":{"session_ok":"yes","reason":"ok"}}}}`))
	}))
	defer bridge.Close()
	var logs bytes.Buffer
	sink := &recordingLivenessSink{}
	c := New(Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 1, Liveness: sink,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	c.SetTargets([]Target{{Key: "bridge-a", URL: bridge.URL}})
	c.Collect(context.Background())
	got := c.Snapshot()["bridge-a"]
	if got.Hostname != "thor" || got.Liveness != nil {
		t.Fatalf("observation = %#v, want the deployment kept and the unreadable liveness dropped", got)
	}
	if len(sink.facts) != 0 {
		t.Fatalf("sink received %v from an unreadable block", sink.facts)
	}
	if !strings.Contains(logs.String(), "liveness") {
		t.Fatalf("an unreadable liveness block was dropped silently; log = %s", logs.String())
	}
}

func TestCollectorSinkFailureIsLoggedAndDoesNotDropTheObservation(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"thor","deployment":{},` +
			`"liveness":{"session_ok":true,"reason":"ok","checked_at":"2026-09-07T10:00:00Z","mode":"CHECK"}}}}`))
	}))
	defer bridge.Close()
	var logs bytes.Buffer
	sink := &recordingLivenessSink{err: fmt.Errorf("database is away")}
	c := New(Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 1, Liveness: sink,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	c.SetTargets([]Target{{Key: "bridge-a", URL: bridge.URL}})
	c.Collect(context.Background())
	got := c.Snapshot()["bridge-a"]
	if got.Hostname != "thor" || got.Liveness == nil {
		t.Fatalf("observation = %#v, want cached with its liveness even though persisting failed", got)
	}
	if !strings.Contains(logs.String(), "database is away") {
		t.Fatalf("sink failure was not logged: %s", logs.String())
	}
}
