package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/telemetry"
)

// Default retry pacing. The PRD fixes neither, so they are named constants a
// deployment can move rather than literals buried in the backoff arithmetic.
const (
	// DefaultRetryBaseDelay is the first backoff step.
	DefaultRetryBaseDelay = 5 * time.Second
	// DefaultRetryMaxDelay caps exponential growth, so a node with a long
	// retry budget cannot schedule its last attempt beyond any useful horizon.
	DefaultRetryMaxDelay = 5 * time.Minute
)

// Option configures an Engine.
type Option func(*Engine)

// WithClock replaces the engine's source of wall-clock time. It is what makes
// the §9.7 maxDuration bound testable without waiting for it.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithIDFactory replaces the identifier generator for runs, tokens, node
// runs, and attempts.
func WithIDFactory(newID func() string) Option {
	return func(e *Engine) {
		if newID != nil {
			e.newID = newID
		}
	}
}

// WithRetryDelays replaces the backoff base and cap.
func WithRetryDelays(base, max time.Duration) Option {
	return func(e *Engine) {
		if base >= 0 {
			e.retryBase = base
		}
		if max > 0 {
			e.retryMax = max
		}
	}
}

// WithValidator supplies an already-compiled schema validator, so a process
// holding several engines compiles the embedded schemas once.
func WithValidator(v *contracts.Validator) Option {
	return func(e *Engine) {
		if v != nil {
			e.validator = v
		}
	}
}

// WithTelemetry wires the §12.5 completion transaction (task t19,
// CompleteAttempt) through a telemetry.Provider. Omitting this option
// leaves e.telemetry at its zero value, a nil *telemetry.Provider — every
// telemetry.Provider method tolerates a nil receiver, so an Engine built
// without this option (every existing caller, every existing test) behaves
// exactly as it did before this option existed.
func WithTelemetry(p *telemetry.Provider) Option {
	return func(e *Engine) {
		e.telemetry = p
	}
}

// Engine is the workflow state machine: it creates runs and commits the
// §12.5 completion transaction. It is safe for concurrent use as long as its
// Store is.
type Engine struct {
	store     Store
	validator *contracts.Validator
	now       func() time.Time
	newID     func() string
	retryBase time.Duration
	retryMax  time.Duration
	telemetry *telemetry.Provider

	mu       sync.Mutex
	prepared map[string]*Workflow
}

// New returns an engine over store.
func New(s Store, opts ...Option) (*Engine, error) {
	if s == nil {
		return nil, errors.New("engine: New requires a store")
	}
	e := &Engine{
		store:     s,
		now:       func() time.Time { return time.Now().UTC() },
		newID:     store.NewULID,
		retryBase: DefaultRetryBaseDelay,
		retryMax:  DefaultRetryMaxDelay,
		prepared:  make(map[string]*Workflow),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	if e.validator == nil {
		v, err := contracts.NewValidator()
		if err != nil {
			return nil, fmt.Errorf("engine: compile schemas: %w", err)
		}
		e.validator = v
	}
	return e, nil
}

// Store returns the store the engine writes through.
func (e *Engine) Store() Store { return e.store }

// Workflow returns the prepared workflow for a digest, loading and caching it
// from the given IR if it has not been seen. Guards and contracts are
// compiled once per digest; because a digest addresses immutable bytes, a
// cached entry can never be stale.
func (e *Engine) Workflow(digest string, ir []byte) (*Workflow, error) {
	e.mu.Lock()
	prepared, ok := e.prepared[digest]
	e.mu.Unlock()
	if ok {
		return prepared, nil
	}

	loaded, err := LoadWorkflow(digest, ir)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if existing, ok := e.prepared[digest]; ok {
		loaded = existing
	} else {
		e.prepared[digest] = loaded
	}
	e.mu.Unlock()
	return loaded, nil
}

// CreateRun starts one execution of a compiled workflow (PRD §12.5's opening
// move, and §20.1's "a run pins an immutable definition").
//
// Everything below commits in one transaction, or nothing does:
//
//   - the run input is validated against the workflow's input contract
//     *before* the transaction opens — a run whose input violates its
//     contract is never created, so there is no half-born run to clean up;
//   - the definition is published (or re-resolved) by content digest, and the
//     run pins that immutable version;
//   - the entry token and its node run are created together, because a run
//     with no token is a run nothing will ever move;
//   - the node run's work is enqueued, so the run is claimable the moment it
//     is visible;
//   - run.created and node-run.ready are appended to the event log and the
//     outbox, so the queue signal and the audit trail cannot disagree with
//     the state that produced them.
func (e *Engine) CreateRun(ctx context.Context, cw *compiler.CompiledWorkflow, input json.RawMessage, opts ...RunOption) (Run, error) {
	if cw == nil {
		return Run{}, errors.New("engine: CreateRun requires a compiled workflow")
	}
	wf, err := e.Workflow(cw.Digest, cw.Normalized)
	if err != nil {
		return Run{}, err
	}
	if err := validatePayload(wf.InputSchema, input); err != nil {
		return Run{}, &ContractError{What: "run input", Detail: err.Error()}
	}

	format := string(cw.Format)
	if format != string(compiler.FormatJSON) {
		format = string(compiler.FormatYAML)
	}

	now := e.now().UTC()
	run := Run{
		ID:             e.newID(),
		NamespaceID:    e.store.NamespaceID(),
		WorkflowDigest: cw.Digest,
		State:          RunRunning,
		Input:          jsonOrNull(input),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	for _, opt := range opts {
		opt(&run)
	}

	err = e.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		versionID, err := tx.EnsureWorkflowVersion(ctx, WorkflowVersionInput{
			WorkflowKey:   wf.Name,
			SourceFormat:  format,
			Source:        string(cw.Source),
			NormalizedIR:  cw.Normalized,
			ContentDigest: cw.Digest,
		})
		if err != nil {
			return err
		}
		run.WorkflowVersionID = versionID

		if err := tx.InsertRun(ctx, run); err != nil {
			return err
		}
		// The same advisory lock every writer of this run's ledger takes.
		// Taking it here means a completion cannot begin before the run's
		// entry state is fully committed.
		if err := tx.Lock(ctx, ledger.RunLockKey(run.ID)); err != nil {
			return err
		}

		token := Token{
			ID:          e.newID(),
			NamespaceID: run.NamespaceID,
			RunID:       run.ID,
			NodeID:      wf.Entry,
			State:       TokenActive,
			CreatedAt:   now,
		}
		if err := tx.InsertToken(ctx, token); err != nil {
			return err
		}

		entryNode := wf.Nodes[wf.Entry]
		nodeRun := NodeRun{
			ID:          e.newID(),
			NamespaceID: run.NamespaceID,
			RunID:       run.ID,
			TokenID:     token.ID,
			NodeID:      wf.Entry,
			State:       dispatchState(entryNode.Kind),
			VisitCount:  1,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := tx.InsertNodeRun(ctx, nodeRun); err != nil {
			return err
		}

		// The entry node has no producing edge — dispatchNode's
		// edgeFromNode/edgeFromOutcome are empty, matching what a human task
		// created here has to say about what put the run there.
		workID, humanTaskID, err := e.dispatchNode(ctx, tx, entryNode, run, nodeRun, "", "", now)
		if err != nil {
			return err
		}

		if _, err := tx.AppendEvent(ctx, run.ID, event(TypeRunCreated, map[string]any{
			"run_id":              run.ID,
			"workflow_version_id": versionID,
			"workflow_digest":     cw.Digest,
			"workflow_key":        wf.Name,
			"entry":               wf.Entry,
		})); err != nil {
			return err
		}

		// See complete.go's advance for why an approval node's entry emits
		// human-task.created instead of node-run.ready: there is no
		// claimable work item for a consumer of that event to find.
		if humanTaskID != "" {
			_, err = tx.AppendEvent(ctx, run.ID, event(TypeHumanTaskCreated, map[string]any{
				"run_id":        run.ID,
				"node_run_id":   nodeRun.ID,
				"node_id":       nodeRun.NodeID,
				"token_id":      token.ID,
				"human_task_id": humanTaskID,
				"visit":         nodeRun.VisitCount,
			}))
			return err
		}
		_, err = tx.AppendEvent(ctx, run.ID, event(TypeNodeRunReady, map[string]any{
			"run_id":      run.ID,
			"node_run_id": nodeRun.ID,
			"node_id":     nodeRun.NodeID,
			"token_id":    token.ID,
			"work_id":     workID,
			"visit":       nodeRun.VisitCount,
		}))
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

func jsonOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
