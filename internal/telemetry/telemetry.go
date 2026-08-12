// Package telemetry is the sole seam through which culture-nodes's control
// plane touches go.opentelemetry.io. Instrumented packages
// (internal/engine, internal/worker, internal/actors) depend on this
// package's typed API, never on otel directly -- so what an operator sees
// exported, and what an attribute an emitted span or metric can possibly
// carry, is reviewable in one place.
//
// # Env-gated, off by default
//
// New builds a Provider from the environment. With
// OTEL_EXPORTER_OTLP_ENDPOINT unset -- the default for every existing
// deployment and every test -- New returns NoOp(): a Provider wrapping
// otel's own no-op tracer and meter. No gRPC exporter is constructed, no
// background goroutine starts, nothing dials out, nothing is logged. The
// control plane runs exactly as it did before this package existed.
//
// Only when OTEL_EXPORTER_OTLP_ENDPOINT is set does New build a real SDK
// TracerProvider and MeterProvider over an OTLP/gRPC exporter, reading the
// rest of the standard OTEL_EXPORTER_OTLP_* variables the exporter
// constructors already understand.
//
// # Attribute allowlist
//
// AllowedAttributeKeys is the complete, reviewable list of attribute keys
// any span or metric this package emits may carry: ids (run, node,
// attempt, actor), enum states and outcomes, counts, and durations --
// nothing else. In particular, nothing here ever carries a run's input
// payload, an instruction string, or ledger record content: those are
// exactly the fields Operation.Start/End never accept, because the only
// way to attach an attribute is through one of the typed constructors
// below (RunID, NodeID, TechStatus, ...), and every one of them is backed
// by a key in the allowlist. filterAllowed is a second, structural
// enforcement of the same rule: even a caller that built an
// attribute.KeyValue by hand (bypassing the constructors) has it silently
// dropped before it reaches a span or a metric.
//
// # Seams
//
// Three seams are instrumented (task t19): the engine's §12.5 completion
// transaction (SeamEngineTransitionCommit, internal/engine/complete.go),
// the worker's actor dispatch (SeamWorkerDispatch,
// internal/worker/dispatch.go), and the actor callback ingest
// (SeamActorCallback, internal/actors/callback.go). Each is one
// Provider.Start/Operation.End pair wrapping the seam's existing code, so
// none of the three needed to change what they do -- only to report that
// they did it.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// instrumentationName is this package's own instrumentation scope name,
// stamped on the tracer and meter it hands to the SDK.
const instrumentationName = "github.com/agentculture/culture-nodes/internal/telemetry"

// serviceName is the resource attribute every exported span and metric
// carries, identifying which binary produced them. It names the control
// plane as a whole rather than the individual cmd/nodes mode (serve,
// worker, scheduler) that happened to load this package, because a
// deployment scrapes traces across all of them as one system.
const serviceName = "culture-nodes"

// EndpointEnvVar is the variable that gates exporter construction. Its
// presence -- not any particular value beyond non-empty -- decides whether
// New builds a real OTLP pipeline or NoOp(). It is deliberately the same
// variable name the OTLP exporters themselves read (they re-read it to
// learn the endpoint), so setting it once configures both "should this
// process export at all" and "where to".
const EndpointEnvVar = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Provider is the thin wrapper every instrumented package depends on. Its
// zero value (a nil *Provider) is a valid, safe no-op -- every method here
// tolerates a nil receiver -- so a package field of type *Provider that is
// simply never set (as in every existing test) behaves identically to a
// Provider built by NoOp().
type Provider struct {
	tracer trace.Tracer
	meter  metric.Meter

	instruments map[Seam]*seamInstruments
	shutdown    func(context.Context) error
}

// seamInstruments are the metric instruments one seam records through,
// created once at construction (New/NoOp) rather than per-call: creating a
// same-named instrument repeatedly is wasteful and, on some SDK
// configurations, noisy.
type seamInstruments struct {
	count    metric.Int64Counter
	duration metric.Float64Histogram
}

// New builds a Provider from the environment (see the package doc's
// "Env-gated, off by default" section). It never returns a nil Provider
// paired with a nil error -- callers always get something Start-able.
func New(ctx context.Context) (*Provider, error) {
	if os.Getenv(EndpointEnvVar) == "" {
		return NoOp(), nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build OTLP trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)

	metricExporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: build OTLP metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)

	p, err := newProvider(tp.Tracer(instrumentationName), mp.Meter(instrumentationName))
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return nil, err
	}
	p.shutdown = func(ctx context.Context) error {
		tpErr := tp.Shutdown(ctx)
		mpErr := mp.Shutdown(ctx)
		if tpErr != nil {
			return tpErr
		}
		return mpErr
	}
	return p, nil
}

// NoOp returns a Provider wrapping otel's own no-op tracer and meter: every
// Start/End and every metric record is accepted and silently discarded,
// with no exporter, no goroutine, and no network activity anywhere behind
// it. It is what New returns when no collector is configured, and it is
// also the right Provider for a test that wants to exercise instrumented
// code paths without asserting on what they emit.
func NoOp() *Provider {
	p, err := newProvider(
		tracenoop.NewTracerProvider().Tracer(instrumentationName),
		metricnoop.NewMeterProvider().Meter(instrumentationName),
	)
	if err != nil {
		// The no-op meter never refuses to create an instrument, so this is
		// unreachable; a Provider that could fail to be no-op would be a
		// bug worth stopping for.
		panic(err)
	}
	return p
}

// newProvider builds the per-seam metric instruments once and returns the
// assembled Provider. It is the shared tail of New and NoOp.
func newProvider(tracer trace.Tracer, meter metric.Meter) (*Provider, error) {
	p := &Provider{
		tracer:      tracer,
		meter:       meter,
		instruments: make(map[Seam]*seamInstruments, len(seams)),
		shutdown:    func(context.Context) error { return nil },
	}
	for _, seam := range seams {
		count, err := meter.Int64Counter(
			string(seam)+".count",
			metric.WithDescription("completions of "+string(seam)),
		)
		if err != nil {
			return nil, fmt.Errorf("telemetry: build counter for %s: %w", seam, err)
		}
		duration, err := meter.Float64Histogram(
			string(seam)+".duration_ms",
			metric.WithDescription("duration of "+string(seam)+" in milliseconds"),
			metric.WithUnit("ms"),
		)
		if err != nil {
			return nil, fmt.Errorf("telemetry: build duration histogram for %s: %w", seam, err)
		}
		p.instruments[seam] = &seamInstruments{count: count, duration: duration}
	}
	return p, nil
}

// Shutdown flushes and releases any exporter resources New constructed.
// NoOp's Shutdown (and a nil Provider's) is a no-op, matching the rest of
// this package's nil-receiver tolerance.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Seam identifies one of the instrumented commit/dispatch/callback paths.
// It selects the span name and the metric instrument an Operation uses; it
// is never itself emitted as a span or metric attribute.
type Seam string

// The three seams task t19 instruments.
const (
	// SeamEngineTransitionCommit is the PRD §12.5 completion transaction
	// (internal/engine/complete.go's CompleteAttempt): one node attempt's
	// report becoming committed orchestration state, or nothing at all.
	SeamEngineTransitionCommit Seam = "engine.transition_commit"
	// SeamWorkerDispatch is one actor dispatch (internal/worker/dispatch.go's
	// dispatchActor): resolving the endpoint, invoking the actor, and
	// handling whatever it answered.
	SeamWorkerDispatch Seam = "worker.dispatch"
	// SeamActorCallback is one §13.4 callback event ingest
	// (internal/actors/callback.go's HandleCallback).
	SeamActorCallback Seam = "actors.callback"
)

// seams is every Seam this package knows about -- the set newProvider
// builds instruments for. Adding a fourth instrumented seam means adding
// its constant here and nowhere else.
var seams = []Seam{SeamEngineTransitionCommit, SeamWorkerDispatch, SeamActorCallback}

// Operation is one in-flight instrumented seam: a started span plus the
// bookkeeping End needs to record its duration and outcome. Its zero value
// (returned by a nil Provider's Start) is a valid no-op.
type Operation struct {
	provider *Provider
	seam     Seam
	span     trace.Span
	start    time.Time
}

// Start begins one Operation for seam, opening a span and starting its
// duration clock. attrs are filtered to AllowedAttributeKeys before they
// touch the span (see filterAllowed) -- so passing an attribute this
// package does not recognize does not fail the call, it just never reaches
// a span or metric.
//
// A nil Provider (the zero value of every *Provider field this package's
// callers carry until they opt into telemetry) returns ctx unchanged and a
// zero Operation, whose End is a no-op. This is what makes wiring
// telemetry into a seam safe by construction: a package that never sets
// its Telemetry field behaves exactly as it did before this package
// existed.
func (p *Provider) Start(ctx context.Context, seam Seam, attrs ...attribute.KeyValue) (context.Context, *Operation) {
	if p == nil {
		return ctx, &Operation{}
	}
	ctx, span := p.tracer.Start(ctx, string(seam), trace.WithAttributes(filterAllowed(attrs)...))
	return ctx, &Operation{provider: p, seam: seam, span: span, start: time.Now()}
}

// End completes op: it sets the final (filtered) attributes on the span,
// marks the span's status ok or error -- never attaching an error's own
// message text, only the ok/error verdict, so a technical error string
// (which may echo request detail) is never at risk of becoming span
// content -- ends the span, and records the seam's count and duration
// metrics with the same filtered attributes.
//
// End on a nil Operation (from a nil Provider's Start, or a zero-value
// Operation from any other source) is a safe no-op.
func (o *Operation) End(ctx context.Context, ok bool, attrs ...attribute.KeyValue) {
	if o == nil || o.provider == nil {
		return
	}
	allowed := filterAllowed(attrs)
	elapsedMs := float64(time.Since(o.start).Microseconds()) / 1000.0

	if o.span != nil {
		if len(allowed) > 0 {
			o.span.SetAttributes(allowed...)
		}
		o.span.SetAttributes(durationMs(elapsedMs))
		if ok {
			o.span.SetStatus(codes.Ok, "")
		} else {
			o.span.SetStatus(codes.Error, "")
		}
		o.span.End()
	}

	instruments := o.provider.instruments[o.seam]
	if instruments == nil {
		return
	}
	measureOpts := metric.WithAttributes(allowed...)
	instruments.count.Add(ctx, 1, measureOpts)
	instruments.duration.Record(ctx, elapsedMs, measureOpts)
}
