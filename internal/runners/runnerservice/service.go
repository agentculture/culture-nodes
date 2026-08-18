package runnerservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// Defaults for a Config that leaves a field zero.
const (
	// DefaultConcurrency is how many operations execute at once. It is small
	// on purpose: each one is a container, and a runner that accepts
	// unbounded parallelism is a runner that discovers its own limits by
	// exhausting the host's.
	DefaultConcurrency = 4
	// DefaultQueueDepth is how many accepted operations may wait for a
	// worker. Past it, dispatch answers 429 rather than queueing without
	// bound: a queue with no limit is a promise the service cannot keep.
	DefaultQueueDepth = 64
	// DefaultMaxOperationBytes bounds an execute request body. The remedy for
	// a larger payload is an artifact ref, never truncation.
	DefaultMaxOperationBytes int64 = 1 << 20
	// DefaultRetryAfterSeconds is the Retry-After a rate-limited dispatch
	// asks for.
	DefaultRetryAfterSeconds = 5
	// maxSweepInterval bounds how long the expiry sweep sleeps between
	// passes.
	maxSweepInterval = 5 * time.Minute
)

// Config configures a Service.
type Config struct {
	// Runner is the wrapped execution boundary — the headspace bridge in the
	// reference deployment. Required.
	Runner runners.Runner
	// Store holds per-operation status for the declared retention. Required.
	// Use NewFileStore for a deployment; NewMemoryStore declares itself
	// non-durable and forgets everything across a restart.
	Store Store
	// Secret is the bearer credential every request must present. Required:
	// there is no unauthenticated deployment posture for a runner service.
	Secret string

	// Concurrency and QueueDepth size the bounded worker pool.
	Concurrency int
	QueueDepth  int
	// MaxOperationBytes bounds an execute request body.
	MaxOperationBytes int64
	// StatusRetention is how long a terminal status stays readable, and what
	// every acceptance declares. It must be at least
	// runners.MinStatusRetention.
	StatusRetention time.Duration
	// PollAfter is the sampling interval acceptances ask for. Advisory.
	PollAfter time.Duration
	// CallbackTimeout bounds one completion-notification attempt.
	CallbackTimeout time.Duration

	// HTTPClient sends completion notifications. Defaults to a client bounded
	// by CallbackTimeout.
	HTTPClient *http.Client
	// AllowCallbackURL vets a caller-supplied callback URL before this
	// service posts to it. Nil allows any absolute http/https URL that
	// carries no embedded credential. A deployment substitutes its egress
	// policy here.
	AllowCallbackURL func(*url.URL) bool

	// ArtifactSessions, when non-nil, is told each operation's attempt-scoped
	// callback credential just before Execute and told to forget it right
	// after (deviation d1, issue #189): the wrapped runner can then persist
	// attempt artifacts through the control plane's publication route without
	// this host ever holding a store credential. Operations that offered no
	// callback register nothing — the runner's artifact store then reports
	// "no upload target" for them, which is the honest outcome.
	ArtifactSessions ArtifactSessionRegistrar

	// Clock overrides time.Now, for tests that assert exact timings.
	Clock func() time.Time
	// OnError receives diagnostics — a callback that did not land, a status
	// record that could not be written. It is never how a caller learns an
	// operation's outcome; that is the status endpoint's job.
	OnError func(error)
}

// ArtifactSessionRegistrar brackets an operation's execution with its upload
// credential: Register before Execute, Release after. Implemented by
// internal/runners/artifactclient.Registry.
type ArtifactSessionRegistrar interface {
	Register(attemptID, callbackURL, token string) error
	Release(attemptID string)
}

// Service is an api/runner-protocol runner service wrapping one
// runners.Runner.
type Service struct {
	runner          runners.Runner
	store           Store
	auth            *authenticator
	validator       *contracts.Validator
	retention       time.Duration
	pollAfter       time.Duration
	maxBytes        int64
	callbackTimeout time.Duration
	httpClient      *http.Client
	allowCallback   func(*url.URL) bool
	now             func() time.Time
	onError         func(error)
	artifactSess    ArtifactSessionRegistrar

	queue   chan runners.Operation
	rootCtx context.Context //nolint:containedctx // the pool's lifetime context, cancelled by Close
	stop    context.CancelFunc
	workers sync.WaitGroup
	notices sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	inflight  map[string]context.CancelFunc
	cancelled map[string]bool
	callbacks map[string]callbackTarget

	closeOnce sync.Once
}

// New validates cfg, recovers any status left behind by a previous process,
// and starts the worker pool.
func New(cfg Config) (*Service, error) {
	if cfg.Runner == nil {
		return nil, errors.New("runnerservice: a Runner is required; a service with nothing behind it would accept " +
			"operations it can never answer")
	}
	if cfg.Store == nil {
		return nil, errors.New("runnerservice: a Store is required; status the service cannot hold is status it " +
			"cannot serve for the retention it declares")
	}
	auth, err := newAuthenticator(cfg.Secret)
	if err != nil {
		return nil, err
	}
	retention := cfg.StatusRetention
	if retention == 0 {
		retention = runners.MinStatusRetention
	}
	if retention < runners.MinStatusRetention {
		return nil, fmt.Errorf("runnerservice: a status retention of %s is below the protocol minimum of %s; "+
			"a runner that forgets an operation before it can be sampled makes its outcome unlearnable",
			retention, runners.MinStatusRetention)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("runnerservice: build the schema validator: %w", err)
	}

	svc := &Service{
		runner:          cfg.Runner,
		store:           cfg.Store,
		auth:            auth,
		validator:       validator,
		retention:       retention,
		pollAfter:       orDuration(cfg.PollAfter, runners.DefaultPollInterval),
		maxBytes:        orInt64(cfg.MaxOperationBytes, DefaultMaxOperationBytes),
		callbackTimeout: orDuration(cfg.CallbackTimeout, defaultCallbackTimeout),
		allowCallback:   cfg.AllowCallbackURL,
		now:             cfg.Clock,
		onError:         cfg.OnError,
		artifactSess:    cfg.ArtifactSessions,
		queue:           make(chan runners.Operation, orInt(cfg.QueueDepth, DefaultQueueDepth)),
		inflight:        map[string]context.CancelFunc{},
		cancelled:       map[string]bool{},
		callbacks:       map[string]callbackTarget{},
	}
	if svc.now == nil {
		svc.now = time.Now
	}
	svc.httpClient = cfg.HTTPClient
	if svc.httpClient == nil {
		svc.httpClient = &http.Client{Timeout: svc.callbackTimeout}
	}
	svc.rootCtx, svc.stop = context.WithCancel(context.Background())

	if err := svc.recoverInterrupted(); err != nil {
		svc.stop()
		return nil, err
	}

	for range orInt(cfg.Concurrency, DefaultConcurrency) {
		svc.workers.Add(1)
		go svc.work()
	}
	svc.workers.Add(1)
	go svc.sweeper()

	return svc, nil
}

// Durable reports whether this service's status store survives a restart, so
// a process can say so at startup rather than implying a promise it is not
// keeping.
func (s *Service) Durable() bool { return s.store.Durable() }

// StatusRetention is the retention every acceptance declares.
func (s *Service) StatusRetention() time.Duration { return s.retention }

// Close stops accepting work, cancels every operation still in flight, and
// waits for the pool to drain.
//
// Cancelling rather than waiting is the deliberate choice: an operation may
// have minutes of wall clock left, and a shutdown that waited them out is a
// shutdown an orchestrator will SIGKILL through — which is precisely the
// crash the durable store then has to recover from. Cancelling gives every
// in-flight operation an honest terminal status (`cancelled`, reported by the
// runner that actually stopped it) before the process exits.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		s.stop()
		s.workers.Wait()
		s.drainQueue()
		s.notices.Wait()
	})
}

// drainQueue gives a terminal status to every operation that was accepted but
// never started. Nothing ran, so the result claims nothing.
func (s *Service) drainQueue() {
	for {
		select {
		case op := <-s.queue:
			s.finishNeverRan(op.OperationID, runners.StateCancelled, runners.ErrorCancellation,
				"the runner service shut down before this operation started",
				"the operation was accepted and queued but never started, so nothing about it was observed")
		default:
			return
		}
	}
}

// ---------------------------------------------------------------- transport

// Handler returns the protocol's HTTP surface.
//
// Authentication wraps the router rather than each route, so a credential is
// checked before anything else is — including before a path is matched. That
// ordering is the property that stops an unauthenticated caller from telling
// "no such operation" apart from "not yours".
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runners.OperationsPath, s.handleExecute)
	mux.HandleFunc("GET "+runners.OperationsPath+"/{operation_id}", s.handleStatus)
	mux.HandleFunc("POST "+runners.OperationsPath+"/{operation_id}/cancel", s.handleCancel)
	return s.authenticated(mux)
}

func (s *Service) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.authenticate(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, runners.ErrorAuthOrPolicy,
				"a bearer credential is required on every runner-protocol request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleExecute is the dispatch path: refuse everything refusable here, then
// answer 202 and get out of the way.
//
// Every refusal this handler can decide is decided synchronously, because the
// 202 is a door that closes: after it, there is no HTTP error left to send
// and the only honest way to report a refusal is a terminal `rejected` status
// (see result.go). So the schema, the protocol version, the idempotency key,
// the payload size, and the queue's capacity are all checked before the
// acceptance is issued.
func (s *Service) handleExecute(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readOperationBody(w, r)
	if !ok {
		return
	}
	if declared := r.Header.Get(runners.ProtocolVersionHeader); !supportedProtocolVersion(declared) {
		writeError(w, http.StatusBadRequest, runners.ErrorRejectedInput,
			"this runner speaks runner protocol "+runners.ProtocolVersion+", not "+declared)
		return
	}
	if err := s.validator.ValidateJSON(runners.OperationSchemaPath, body); err != nil {
		writeError(w, http.StatusBadRequest, runners.ErrorRejectedInput,
			"the operation document is not valid against "+runners.OperationSchemaPath+": "+err.Error())
		return
	}

	var op runners.Operation
	if err := json.Unmarshal(body, &op); err != nil {
		writeError(w, http.StatusBadRequest, runners.ErrorRejectedInput, "the operation document could not be decoded")
		return
	}
	if key := r.Header.Get(runners.IdempotencyKeyHeader); key != "" && key != op.OperationID {
		writeError(w, http.StatusBadRequest, runners.ErrorRejectedInput,
			"the Idempotency-Key header names a different operation than the document does; the operation_id "+
				"IS the idempotency key, and two answers to \"which operation is this\" is none")
		return
	}

	digest, err := contracts.DigestValue(op)
	if err != nil {
		writeError(w, http.StatusBadRequest, runners.ErrorRejectedInput,
			"the operation document could not be canonicalised: "+err.Error())
		return
	}
	callback, hasCallback := callbackFromHeaders(r, s.allowCallback)

	acceptance, code, message := s.accept(op, digest, callback, hasCallback)
	if code != http.StatusAccepted {
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", fmt.Sprint(DefaultRetryAfterSeconds))
		}
		writeError(w, code, errorKindForStatus(code), message)
		return
	}
	writeJSON(w, http.StatusAccepted, acceptance)
}

// readOperationBody reads the request body under the transport limit,
// distinguishing "too large" (413, whose remedy is an artifact ref) from
// "unreadable" (400).
func (s *Service) readOperationBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBytes))
	if err == nil {
		return body, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, runners.ErrorRejectedInput,
			fmt.Sprintf("the operation document exceeds this runner's %d-byte transport limit; "+
				"pass large inputs as an artifact ref rather than truncating them", s.maxBytes))
		return nil, false
	}
	writeError(w, http.StatusBadRequest, runners.ErrorRejectedInput, "the request body could not be read")
	return nil, false
}

// accept is the idempotent admission decision, taken under one lock so two
// concurrent dispatches of the same operation cannot both start work.
func (s *Service) accept(op runners.Operation, digest string, callback callbackTarget, hasCallback bool) (
	runners.Acceptance, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return runners.Acceptance{}, http.StatusServiceUnavailable, "this runner service is shutting down"
	}

	existing, found, err := s.store.Get(op.OperationID)
	if err != nil {
		return runners.Acceptance{}, http.StatusInternalServerError, "the status store could not be read"
	}
	if found {
		if existing.DocumentDigest != digest {
			return runners.Acceptance{}, http.StatusConflict,
				"operation " + op.OperationID + " was already accepted with a different document; " +
					"an idempotency key names one piece of work, not one name for several"
		}
		// Re-sending a key returns the acceptance already issued, and starts
		// nothing: at-least-once delivery must not become at-least-twice
		// execution.
		return existing.Acceptance, http.StatusAccepted, ""
	}

	replay, err := replayOf(op)
	if err != nil {
		return runners.Acceptance{}, http.StatusBadRequest, "the operation's policy could not be digested: " + err.Error()
	}
	record := Record{
		OperationID:    op.OperationID,
		State:          runners.StateAccepted,
		DocumentDigest: digest,
		Acceptance:     s.acceptanceFor(op.OperationID),
		Replay:         replay,
		AcceptedAt:     s.now().UTC(),
	}
	if err := s.store.Put(record); err != nil {
		s.report(err)
		return runners.Acceptance{}, http.StatusInternalServerError, "the status record could not be written"
	}

	select {
	case s.queue <- op:
	default:
		// Nothing was started, so nothing is owed a status: remove the record
		// rather than leave an operation the caller will poll forever.
		if err := s.store.Delete(op.OperationID); err != nil {
			s.report(err)
		}
		return runners.Acceptance{}, http.StatusTooManyRequests,
			"this runner's work queue is full; retry after " + fmt.Sprint(DefaultRetryAfterSeconds) + "s"
	}
	if hasCallback {
		s.callbacks[op.OperationID] = callback
	}
	return record.Acceptance, http.StatusAccepted, ""
}

func (s *Service) acceptanceFor(operationID string) runners.Acceptance {
	return runners.Acceptance{
		OperationID:            operationID,
		PollAfterSeconds:       int(s.pollAfter.Seconds()),
		StatusRetentionSeconds: int(s.retention.Seconds()),
		SupportsCancellation:   true,
		SupportsCallback:       true,
	}
}

// handleStatus is the authoritative completion path.
func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operation_id")
	record, found, err := s.store.Get(operationID)
	if err != nil {
		s.report(err)
		writeError(w, http.StatusInternalServerError, runners.ErrorRunnerUnavailable, "the status store could not be read")
		return
	}
	if !found || s.expired(record) {
		writeError(w, http.StatusNotFound, runners.ErrorRunnerUnavailable,
			"this runner holds no status for operation "+operationID)
		return
	}
	writeJSON(w, http.StatusOK, runners.OperationStatus{
		OperationID: record.OperationID,
		State:       record.State,
		Result:      record.Result,
	})
}

// handleCancel is the optional, best-effort cancel path.
func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operation_id")
	record, found, err := s.store.Get(operationID)
	if err != nil {
		s.report(err)
		writeError(w, http.StatusInternalServerError, runners.ErrorRunnerUnavailable, "the status store could not be read")
		return
	}
	if !found || s.expired(record) {
		writeError(w, http.StatusNotFound, runners.ErrorRunnerUnavailable,
			"this runner holds no status for operation "+operationID)
		return
	}

	s.mu.Lock()
	// Recorded only while there is something to stop, and only for an
	// operation still queued or running — a worker that has not picked it up
	// yet then refuses to start it rather than racing the cancel. Recording
	// it for an already-terminal operation would leave an entry nothing ever
	// clears, since the commit that clears it has already happened.
	var cancel context.CancelFunc
	if !record.State.Terminal() {
		s.cancelled[operationID] = true
		cancel = s.inflight[operationID]
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	// 202 for a terminal operation too: the request was accepted, there was
	// simply nothing left to stop. Cancellation is durable in the control
	// plane before this call is ever made.
	w.WriteHeader(http.StatusAccepted)
}

// ------------------------------------------------------------------ workers

func (s *Service) work() {
	defer s.workers.Done()
	for {
		select {
		case <-s.rootCtx.Done():
			return
		case op := <-s.queue:
			s.execute(op)
		}
	}
}

// execute runs one operation and records its terminal status.
func (s *Service) execute(op runners.Operation) {
	operationID := op.OperationID

	ctx, cancel := context.WithCancel(s.rootCtx)
	defer cancel()

	s.mu.Lock()
	alreadyCancelled := s.cancelled[operationID]
	if !alreadyCancelled {
		s.inflight[operationID] = cancel
	}
	s.mu.Unlock()

	if alreadyCancelled {
		s.finishNeverRan(operationID, runners.StateCancelled, runners.ErrorCancellation,
			"the operation was cancelled before it started",
			"the cancel arrived while the operation was still queued, so nothing about it was observed")
		return
	}
	defer func() {
		s.mu.Lock()
		delete(s.inflight, operationID)
		s.mu.Unlock()
	}()

	started := s.now().UTC()
	s.markRunning(operationID, started)

	// The d1 artifact-session bracket: hand the wrapped runner this
	// operation's attempt-scoped upload credential for exactly the duration
	// of Execute. Reading s.callbacks without deleting — finish() still owns
	// the entry's lifecycle for the completion notification.
	if s.artifactSess != nil && op.Context != nil && op.Context.AttemptID != "" {
		s.mu.Lock()
		cb, hasCallback := s.callbacks[operationID]
		s.mu.Unlock()
		if hasCallback {
			if regErr := s.artifactSess.Register(op.Context.AttemptID, cb.url, cb.token); regErr != nil {
				s.report(regErr)
			} else {
				defer s.artifactSess.Release(op.Context.AttemptID)
			}
		}
	}

	result, err := s.runner.Execute(ctx, op)
	s.finish(operationID, result, err, started, s.now().UTC())
}

// markRunning moves an accepted operation to running. A failure to record it
// is a diagnostic, not a reason to refuse to run: the work is about to happen
// either way, and a status stuck at `accepted` is recoverable.
func (s *Service) markRunning(operationID string, started time.Time) {
	record, found, err := s.store.Get(operationID)
	if err != nil || !found {
		s.report(fmt.Errorf("runnerservice: no status record for %s at start of execution", operationID))
		return
	}
	record.State = runners.StateRunning
	record.StartedAt = &started
	if err := s.store.Put(record); err != nil {
		s.report(err)
	}
}

// finish records the terminal status for an operation the runner answered
// for, and fires the optional notification.
func (s *Service) finish(operationID string, result runners.Result, err error, started, finished time.Time) {
	record, found, storeErr := s.store.Get(operationID)
	if storeErr != nil || !found {
		s.report(fmt.Errorf("runnerservice: no status record for %s at end of execution", operationID))
		return
	}

	var state runners.State
	switch {
	case err != nil:
		state, result = refusalResult(record, err, started, finished)
	case !result.State.Terminal():
		// A Runner that returns a non-terminal state has broken its own
		// contract; the service records that it learned nothing rather than
		// serving a state the status envelope does not admit.
		state, result = runners.StateFailed, unmeasuredResult(record, runners.StateFailed,
			runners.ErrorContractFailure,
			"the runner returned the non-terminal state "+string(result.State),
			"the runner's result did not report a terminal state, so nothing it said about the execution could be recorded",
			started, finished)
	default:
		state = result.State
		if result.OperationID == "" {
			// An identity field, not a measurement: the envelope and the
			// result must name the same operation.
			result.OperationID = operationID
		}
	}

	s.commit(record, state, &result, finished)
}

// finishNeverRan records a terminal status for an operation that was accepted
// but never executed.
func (s *Service) finishNeverRan(operationID string, state runners.State, kind runners.ErrorKind, message, note string) {
	record, found, err := s.store.Get(operationID)
	if err != nil || !found {
		return
	}
	now := s.now().UTC()
	result := unmeasuredResult(record, state, kind, message, note, record.AcceptedAt, now)
	s.commit(record, state, &result, now)
}

// commit writes the terminal record, starts the retention clock, and hands
// the optional notification off to its own goroutine.
func (s *Service) commit(record Record, state runners.State, result *runners.Result, finished time.Time) {
	expires := finished.Add(s.retention)
	record.State = state
	record.Result = result
	record.FinishedAt = &finished
	record.ExpiresAt = &expires
	if err := s.store.Put(record); err != nil {
		s.report(err)
	}

	s.mu.Lock()
	target, hasCallback := s.callbacks[record.OperationID]
	delete(s.callbacks, record.OperationID)
	delete(s.cancelled, record.OperationID)
	s.mu.Unlock()

	if !hasCallback {
		return
	}
	s.notices.Add(1)
	go func() {
		defer s.notices.Done()
		s.notify(target, record.OperationID, state)
	}()
}

// ----------------------------------------------------------- recovery/sweep

// recoverInterrupted gives a terminal status to every operation a previous
// process was still holding.
//
// It runs before the pool starts, so a status read can never observe a
// `running` operation this process does not actually hold.
func (s *Service) recoverInterrupted() error {
	records, err := s.store.List()
	if err != nil {
		return fmt.Errorf("runnerservice: read the status store at startup: %w", err)
	}
	now := s.now().UTC()
	for _, record := range records {
		if record.State.Terminal() {
			continue
		}
		state, result := interruptedResult(record, now)
		expires := now.Add(s.retention)
		record.State = state
		record.Result = &result
		record.FinishedAt = &now
		record.ExpiresAt = &expires
		if err := s.store.Put(record); err != nil {
			return fmt.Errorf("runnerservice: record the interrupted operation %s: %w", record.OperationID, err)
		}
	}
	return nil
}

// sweeper removes records whose declared retention has elapsed.
func (s *Service) sweeper() {
	defer s.workers.Done()
	interval := s.retention / 8
	if interval > maxSweepInterval {
		interval = maxSweepInterval
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.rootCtx.Done():
			return
		case <-ticker.C:
			if _, err := s.sweep(s.now().UTC()); err != nil {
				s.report(err)
			}
		}
	}
}

// sweep removes every record whose retention has elapsed, and returns how
// many it removed. A record with no expiry is never swept: retention starts
// when an operation finishes, and one that has not finished has not started
// counting.
func (s *Service) sweep(now time.Time) (int, error) {
	records, err := s.store.List()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, record := range records {
		if record.ExpiresAt == nil || !now.After(*record.ExpiresAt) {
			continue
		}
		if err := s.store.Delete(record.OperationID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// expired reports whether a record's retention has elapsed but the sweep has
// not reached it yet, so a status read never serves a record the service has
// stopped promising.
func (s *Service) expired(record Record) bool {
	return record.ExpiresAt != nil && s.now().After(*record.ExpiresAt)
}

// -------------------------------------------------------------- small parts

func (s *Service) report(err error) {
	if err == nil {
		return
	}
	if s.onError != nil {
		s.onError(err)
	}
}

// supportedProtocolVersion compares major versions only: a minor bump is
// additive by construction, and a caller declaring nothing is taken at the
// current version rather than refused for silence.
func supportedProtocolVersion(declared string) bool {
	if declared == "" {
		return true
	}
	return majorVersion(declared) == majorVersion(runners.ProtocolVersion)
}

func majorVersion(version string) string {
	if index := strings.Index(version, "."); index >= 0 {
		return version[:index]
	}
	return version
}

// errorKindForStatus maps an HTTP refusal onto the result schema's error
// vocabulary, so a failure classified here and one reported inside a result
// speak one language.
func errorKindForStatus(code int) runners.ErrorKind {
	switch code {
	case http.StatusTooManyRequests:
		return runners.ErrorRateLimited
	case http.StatusConflict, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return runners.ErrorRejectedInput
	case http.StatusUnauthorized, http.StatusForbidden:
		return runners.ErrorAuthOrPolicy
	default:
		return runners.ErrorRunnerUnavailable
	}
}

// wireError is the body a refusal carries. The status code is authoritative —
// the runtime classifies on it — and this exists so a human reading a failed
// dispatch learns what was wrong. It never contains a credential.
type wireError struct {
	Error struct {
		Kind    runners.ErrorKind `json:"kind"`
		Message string            `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, code int, kind runners.ErrorKind, message string) {
	body := wireError{}
	body.Error.Kind = kind
	body.Error.Message = message
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"kind":"contract_failure","message":"the response could not be encoded"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

func orDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func orInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func orInt64(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}
