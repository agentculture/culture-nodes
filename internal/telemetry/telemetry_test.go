package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ---- acceptance criterion 2: env-gated, no exporter/no dial when unset ----

// TestNew_NoEndpoint_ReturnsNoOp proves that with OTEL_EXPORTER_OTLP_ENDPOINT
// unset -- the state of every existing deployment and every other test in
// this repo -- New builds a Provider whose spans are the SDK's own no-op
// spans (IsRecording() false), never a real exporter pipeline. That is the
// behavioural proof that nothing here dials out or spawns an exporter
// goroutine when unconfigured.
func TestNew_NoEndpoint_ReturnsNoOp(t *testing.T) {
	t.Setenv(EndpointEnvVar, "")

	p, err := New(context.Background())
	if err != nil {
		t.Fatalf("New with no endpoint configured returned an error: %v", err)
	}
	if p == nil {
		t.Fatal("New with no endpoint configured returned a nil Provider")
	}

	ctx, op := p.Start(context.Background(), SeamEngineTransitionCommit, RunID("run-1"))
	if op.span == nil {
		t.Fatal("Start on an unconfigured Provider produced no span at all")
	}
	if op.span.IsRecording() {
		t.Fatal("Start on an unconfigured Provider produced a recording span; " +
			"a no-op provider must never build a real tracer")
	}

	// End must not panic, dial, or block.
	op.End(ctx, true, Outcome("passed"))

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on a no-op Provider returned an error: %v", err)
	}
}

// TestNew_WithEndpoint_BuildsRealPipeline proves the other half of the gate:
// setting the endpoint does switch New onto the real SDK path (a recording
// tracer), and does so without New itself blocking -- otlptracegrpc/
// otlpmetricgrpc dial lazily, so construction must return promptly even
// against an address nothing is listening on.
func TestNew_WithEndpoint_BuildsRealPipeline(t *testing.T) {
	// A well-formed but unreachable endpoint (port 0 is never listening).
	// gRPC dials lazily by default, so this must not make New block waiting
	// for a connection.
	t.Setenv(EndpointEnvVar, "http://127.0.0.1:0")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p, err := New(ctx)
	if err != nil {
		t.Fatalf("New with an endpoint configured returned an error: %v", err)
	}
	// Shutdown against an unreachable endpoint legitimately spends its
	// export-flush budget trying, so bound it tightly here rather than
	// asserting on how long that takes -- production wiring (cmd/nodes)
	// gives it its own real shutdown timeout instead.
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer shutdownCancel()
		_ = p.Shutdown(shutdownCtx)
	}()

	_, op := p.Start(context.Background(), SeamWorkerDispatch, RunID("run-1"))
	if op.span == nil || !op.span.IsRecording() {
		t.Fatal("Start on a configured Provider did not produce a recording span")
	}
}

// ---- nil-safety: every instrumented package's default (unset) field ----

func TestProvider_NilReceiver_IsSafe(t *testing.T) {
	var p *Provider

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on a nil Provider returned an error: %v", err)
	}

	ctx := context.Background()
	gotCtx, op := p.Start(ctx, SeamActorCallback, RunID("run-1"))
	if gotCtx != ctx {
		t.Fatal("Start on a nil Provider must return the input context unchanged")
	}
	if op == nil {
		t.Fatal("Start on a nil Provider must still return a non-nil Operation")
	}
	// Must not panic.
	op.End(ctx, false, Outcome("failed"))
}

func TestOperation_NilReceiver_IsSafe(t *testing.T) {
	var op *Operation
	// Must not panic.
	op.End(context.Background(), true)
}

// ---- acceptance criterion 3: the attribute allowlist ----

// TestAllowedAttributeKeys_ConstructorsAreAllowlisted proves every typed
// constructor in attributes.go produces a key from AllowedAttributeKeys --
// the "reviewable const/whitelist" acceptance criterion holds for the whole
// public constructor surface, not just the handful this test happens to
// exercise elsewhere.
func TestAllowedAttributeKeys_ConstructorsAreAllowlisted(t *testing.T) {
	kvs := []attribute.KeyValue{
		RunID("r"), NodeID("n"), AttemptID("a"), ActorID("act"),
		TechStatus("succeeded"), Outcome("passed"), NodeRunState("completed"),
		RunState("running"), Disposition("committed"),
		AttemptNumber(1),
		AuthRefusalReason("no_principal"),
		durationMs(1.5),
	}
	for _, kv := range kvs {
		if _, ok := allowedSet[kv.Key]; !ok {
			t.Errorf("constructor for key %q produced a key outside AllowedAttributeKeys", kv.Key)
		}
	}
	if len(kvs) != len(AllowedAttributeKeys) {
		t.Errorf("this test exercises %d constructors but AllowedAttributeKeys has %d keys; "+
			"a key was added to one list and not the other", len(kvs), len(AllowedAttributeKeys))
	}
}

func TestFilterAllowed_DropsUnknownKeys(t *testing.T) {
	attrs := []attribute.KeyValue{
		RunID("run-1"),
		attribute.String("instruction_text", "do the dangerous thing"),
		Outcome("passed"),
		attribute.String("ledger_payload", `{"secret":"value"}`),
		attribute.String("run_input", `{"prompt":"..."}`),
	}

	got := filterAllowed(attrs)

	if len(got) != 2 {
		t.Fatalf("filterAllowed kept %d attributes, want 2 (run_id, outcome): %v", len(got), got)
	}
	for _, kv := range got {
		if kv.Key != KeyRunID && kv.Key != KeyOutcome {
			t.Errorf("filterAllowed kept disallowed key %q", kv.Key)
		}
	}
}

// TestOperation_End_EmitsOnlyAllowlistedAttributes is the acceptance test
// for criterion 3 end to end: a caller that (mistakenly, or maliciously)
// passes run-input/instruction/ledger-payload-shaped attributes into
// Start/End must never see them land on an emitted span or an emitted
// metric data point. It wires a real (non-SDK-noop) tracer and meter to
// in-memory sinks so the assertion is over what actually got recorded, not
// over this package's own filtering logic in isolation.
func TestOperation_End_EmitsOnlyAllowlistedAttributes(t *testing.T) {
	spanExporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	p, err := newProvider(tp.Tracer(instrumentationName), mp.Meter(instrumentationName))
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}

	disallowed := attribute.String("instruction_text", "run `rm -rf /`")
	ctx, op := p.Start(context.Background(), SeamEngineTransitionCommit,
		RunID("run-1"), attribute.String("run_input_payload", "leak me"))
	op.End(ctx, true,
		NodeID("node-1"), AttemptID("att-1"), ActorID("actor-1"),
		TechStatus("succeeded"), Outcome("passed"),
		NodeRunState("completed"), RunState("running"), Disposition("committed"),
		AttemptNumber(2), disallowed,
	)

	// ---- spans ----
	spans := spanExporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != string(SeamEngineTransitionCommit) {
		t.Errorf("span name = %q, want %q", span.Name, SeamEngineTransitionCommit)
	}
	if span.Status.Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", span.Status.Code)
	}
	assertOnlyAllowedKeys(t, "span", span.Attributes)
	assertHasKey(t, "span", span.Attributes, KeyRunID)
	assertHasKey(t, "span", span.Attributes, KeyOutcome)
	assertHasKey(t, "span", span.Attributes, KeyDurationMs)

	// ---- metrics ----
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rm.ScopeMetrics) == 0 {
		t.Fatal("no metrics were recorded")
	}
	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					found[m.Name] = true
					assertOnlyAllowedKeys(t, "metric "+m.Name, dp.Attributes.ToSlice())
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					found[m.Name] = true
					assertOnlyAllowedKeys(t, "metric "+m.Name, dp.Attributes.ToSlice())
				}
			}
		}
	}
	wantCount := string(SeamEngineTransitionCommit) + ".count"
	wantDuration := string(SeamEngineTransitionCommit) + ".duration_ms"
	if !found[wantCount] {
		t.Errorf("counter %q was not recorded", wantCount)
	}
	if !found[wantDuration] {
		t.Errorf("histogram %q was not recorded", wantDuration)
	}
}

func assertOnlyAllowedKeys(t *testing.T, what string, attrs []attribute.KeyValue) {
	t.Helper()
	for _, kv := range attrs {
		if _, ok := allowedSet[kv.Key]; !ok {
			t.Errorf("%s carries disallowed attribute key %q (value %q)", what, kv.Key, kv.Value.Emit())
		}
	}
}

func assertHasKey(t *testing.T, what string, attrs []attribute.KeyValue, key attribute.Key) {
	t.Helper()
	for _, kv := range attrs {
		if kv.Key == key {
			return
		}
	}
	t.Errorf("%s is missing expected attribute key %q", what, key)
}

// ---- seam coverage ----

func TestSeams_MatchInstrumentedPaths(t *testing.T) {
	want := map[Seam]bool{
		SeamEngineTransitionCommit: true,
		SeamWorkerDispatch:         true,
		SeamActorCallback:          true,
	}
	if len(seams) != len(want) {
		t.Fatalf("seams has %d entries, want %d", len(seams), len(want))
	}
	for _, s := range seams {
		if !want[s] {
			t.Errorf("unexpected seam %q in the seams list", s)
		}
	}

	p := NoOp()
	for s := range want {
		if p.instruments[s] == nil {
			t.Errorf("NoOp provider built no instruments for seam %q", s)
		}
	}
}
