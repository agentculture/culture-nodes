package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// The engine executes the *normalized IR*, decoded from JSON, and never the
// compiler's in-memory document. That is not indirection for its own sake:
// after a process restart the only thing that exists is the IR bytes stored
// on the pinned workflow version, so making that the one input keeps the
// restart path and the fresh-compile path identical instead of leaving the
// restart path less exercised.
//
// The same reasoning covers CEL. compiler.CompiledWorkflow carries compiled
// guard Programs, but they are keyed by the JSON path of the *authored*
// document ("/spec/edges/3/when") while the runtime evaluates edges in
// normalized order, and after a restart there is no CompiledWorkflow at all.
// So the engine recompiles guards from the expression text the IR carries —
// which the compiler has already proven compiles and yields a boolean, so
// this is a rebuild, not a second validation pass.

// CEL variables available to an edge guard (PRD §11.2), matching the
// compiler's environment exactly: input is the run input, output is the
// deciding node's output for the outcome being routed, outcome is that
// outcome's name, and event is the delivered signal event an `onEvent` edge's
// guard filters on (issue #43). The two environments must stay identical —
// the compiler proves a guard compiles, this rebuilds it from the pinned IR,
// and a variable in one and not the other would make a published workflow
// fail at run time.
const (
	celVarInput   = "input"
	celVarOutput  = "output"
	celVarOutcome = "outcome"
	celVarEvent   = "event"
	celVarNode    = "node"
	celVarBudget  = "budget"
)

// Workflow is a normalized workflow IR prepared for execution: guards
// compiled, contracts compiled, edges in the order they will be evaluated.
// It is immutable after Load and safe for concurrent use.
type Workflow struct {
	// Digest is the content digest of the IR — what a run pins.
	Digest  string
	Name    string
	Version string

	Entry  string
	Limits Limits
	Budget Budget
	Ledger LedgerLimits

	Nodes map[string]*Node
	// Edges are in normalized order (by source node, outcome, target, guard),
	// which is the order guards are evaluated and therefore the order that
	// decides which edge wins.
	Edges []Edge

	// InputSchema and OutputSchema are the workflow-level contracts, nil when
	// the workflow declares none inline. A schemaRef is left unresolved:
	// resolving one needs a source root, which is the deployment level's job
	// (PRD §11.4), and the engine will not pretend an unresolvable reference
	// is a satisfied contract.
	InputSchema  *jsonschema.Schema
	OutputSchema *jsonschema.Schema

	// IR is the exact normalized JSON this workflow was loaded from.
	IR json.RawMessage
}

// Limits are the §9.7 loop bounds, already expanded by the compiler so every
// field is present.
type Limits struct {
	MaxDuration       time.Duration
	MaxTransitions    int
	MaxVisitsPerNode  int
	MaxParallelTokens int
}

// Budget is the workflow's declared ECONOMIC contract (PRD §9.7's "optional
// agent token or cost budget"; task t11, spec claim c6). Zero on a field
// means the author declared no bound on that axis — never "zero allowed",
// which the compiler refuses outright so the two readings can never collide
// here.
//
// It is separate from Limits because the two fail differently. Exceeding a
// limit is a bound the engine raises against the run; exceeding a budget is a
// refusal the AUTHOR routes (OutcomeBudgetExhausted), decided before anything
// is dispatched. The budget travels in the pinned IR with everything else, so
// the enforcement site reads what this run agreed to rather than a live
// setting that may have moved since it started.
type Budget struct {
	// MaxSessions bounds the NEW provider sessions one run may start —
	// cold starts only. A dispatch carrying a prior continuation ref (ADR
	// 0010) continues a session already paid for and charges nothing; see
	// internal/worker/budget.go for why counting warm turns would make the
	// budget fight the stickiness it exists beside.
	MaxSessions int
	// MaxUncachedInput bounds the input tokens one run may send that the
	// provider did not serve from cache, summed over the attempts that
	// reported usage. An attempt reporting no cached figure charges its
	// input in full — absent cache telemetry is not a demonstrated cache
	// hit.
	MaxUncachedInput int64
}

// Declared reports whether the workflow bounded anything economically.
func (b Budget) Declared() bool {
	return b.MaxSessions > 0 || b.MaxUncachedInput > 0
}

// OutcomeBudgetExhausted is the reserved name a dispatch refused for want of
// budget routes under (compiler.OutcomeBudgetExhausted, kept in step).
//
// It is not one of §3.4's technical statuses and no node contract declares
// it: no actor produces it, the control plane does, before invoking anything.
// The refused attempt's technical status is `policy_denied` — a declared
// policy denied the dispatch — and this is the name the edge is looked up
// under, so an author can distinguish "the budget refused this" from every
// other policy denial.
const OutcomeBudgetExhausted = "budget_exhausted"

// OutcomePreflightUnacknowledged is the second reserved refusal name
// (compiler.OutcomePreflightUnacknowledged, kept in step): a dispatch whose
// clarify-then-commit preflight was never acknowledged inside its window
// (task t14, issue #67).
//
// It is the same KIND of thing as OutcomeBudgetExhausted and is treated
// identically — no contract declares it, no actor produces it, the control
// plane produces it before invoking anything, and the refused attempt's
// technical status is `policy_denied`. It is a separate NAME for the reason
// that one is: an author who wants "nobody acknowledged the briefing" to
// reach a human, or a different actor, or a summarise-and-stop node needs to
// distinguish it from "we ran out of money" and from every other policy
// denial.
const OutcomePreflightUnacknowledged = "preflight_unacknowledged"

// LedgerLimits are the workflow-level ledger bounds (PRD §10.7).
type LedgerLimits struct {
	SchemaVersion     string
	MaxRecordsPerNode int
}

// Node is one node of the graph, prepared for execution.
type Node struct {
	ID       string
	Kind     string
	OwnerRef string

	// Outcomes is the resolved set of domain outcomes this node can produce,
	// as the compiler computed it. The engine reads it rather than
	// re-deriving the union of contract outcomes, decision ports, and
	// kind-implied ports.
	Outcomes []string
	// OutcomeSchemas holds the compiled contract for each outcome that
	// declares one inline. An outcome with no entry has no checkable
	// contract, not an empty one.
	OutcomeSchemas map[string]*jsonschema.Schema

	// OutputFrom is an end node's output binding (a JSON Pointer into the run
	// surface), which is how a workflow says what its result is.
	OutputFrom string

	// InputFrom and InputBindings are a node's own input binding (PRD §11.2),
	// carried for every kind even though only an approval node's dispatch
	// reads it today: it becomes that node's human task "context and
	// artifact references" (PRD §9.9) rather than an actor payload, because
	// the engine still does not resolve bindings into dispatch payloads (see
	// this package's doc comment) — it records the reference, not the value.
	// A binding that declares a LITERAL (issue #73) carries the declared value
	// itself, which is a reference in the only sense that matters here: it is
	// what the author wrote, unchanged.
	InputFrom     string
	InputBindings map[string]InputBinding

	Propose    []string
	Observe    []string
	MaxRecords int

	Timeout time.Duration
	Retry   RetryPolicy

	// DecisionSchemaRef, ApproverRef, and Deadline are an approval node's
	// PRD §9.9 fields: the schema the human decision must satisfy, the
	// requested approver role or group, and how long the pause may run
	// before it is overdue. Empty/zero for every other kind.
	DecisionSchemaRef string
	ApproverRef       string
	Deadline          time.Duration

	// JoinPolicy and JoinQuorum are a join node's barrier policy (issue
	// #43): all | any | quorum, with JoinQuorum meaningful only under
	// quorum. Empty/zero for every other kind.
	JoinPolicy string
	JoinQuorum int

	Continue *Continuation
}

type Continuation struct {
	While       []cel.Program
	Bounds      ContinuationBounds
	OnExhausted string
}

type ContinuationBounds struct {
	MaxContinuations int
	MaxWallClock     time.Duration
	MaxSessions      int
}

// ContinuationState contains only continuation accounting. A node deadline
// is intentionally not part of it: retry/deadline and continuation bounds
// are independent clocks.
type ContinuationState struct {
	// NodeState is the node run's own durable state, mapped onto the
	// `node.state` vocabulary by ContinuationNodeState — MEASURED by the
	// caller from the node_runs row, never a literal (issue #95: the
	// scheduler used to pass "incomplete" unconditionally, which made the
	// canonical `node.state == "incomplete"` true in every run for every
	// node).
	//
	// The empty string means "not measured", and it is not a value: a
	// condition that reads node.state under an empty NodeState is
	// undecidable (ErrContinuationUndecidable), not false. That is what
	// stops a future caller fabricating a state by omission the way the
	// old one did by assignment.
	NodeState         string
	RemainingSessions int
	Continuations     int
	Sessions          int
	WallClock         time.Duration
}

type ContinuationDecision struct {
	Continue      bool
	Outcome       string
	EngineFailure bool
}

// ErrContinuationUndecidable is returned by DecideContinuation when a declared
// `continue.while` condition could not be EVALUATED — the CEL program errored,
// or it produced something that is not a boolean. Neither is a domain
// decision, and dressing them as one is issue #105: before this, all three of
// "the condition was false", "the condition errored" and "the condition
// returned a non-boolean" returned the identical zero ContinuationDecision.
// The first is the node saying stop; the other two are nobody answering. A
// reader of the run could not tell them apart, and because the zero value
// carries no outcome, the author's declared `onExhausted` safety net was
// bypassed by exactly the kind of trouble it exists to catch.
var ErrContinuationUndecidable = errors.New("continue.while condition is undecidable")

// ContinuationNodeState maps a node run's durable lifecycle state onto the
// `node.state` vocabulary a `continue.while` condition is written against
// (issue #95). Terminal node runs are "complete"; every parked or in-flight
// one is "incomplete", which is what the canonical
// `node.state == "incomplete"` means and what its author expects to be able
// to observe as FALSE.
//
// An empty NodeRunState maps to the empty string on purpose: a row nobody
// read is not a state, and DecideContinuation turns that into an undecidable
// condition rather than a guess. Callers pass the status they queried; they
// never pass a literal.
func ContinuationNodeState(status NodeRunState) string {
	switch {
	case status == "":
		return ""
	case status.Terminal():
		return "complete"
	default:
		return "incomplete"
	}
}

// DecideContinuation evaluates the author-owned condition between turns.
// Exhaustion is a routable domain answer, never an engine failure.
//
// The error return is reserved for a condition that could not be decided at
// all (ErrContinuationUndecidable). A condition that decides "stop" is a
// nil-error zero decision, and bound exhaustion is a nil-error decision
// carrying OnExhausted: neither is a failure, and the difference between
// those two and an undecidable one is the whole point of the signature.
func (n *Node) DecideContinuation(state ContinuationState) (ContinuationDecision, error) {
	if n == nil || n.Continue == nil {
		return ContinuationDecision{}, nil
	}
	b := n.Continue.Bounds
	if (b.MaxContinuations > 0 && state.Continuations >= b.MaxContinuations) ||
		(b.MaxWallClock > 0 && state.WallClock >= b.MaxWallClock) ||
		(b.MaxSessions > 0 && state.Sessions >= b.MaxSessions) {
		return ContinuationDecision{Outcome: n.Continue.OnExhausted}, nil
	}
	// An unmeasured node state is OMITTED, not defaulted: `node.state` then
	// fails to resolve and the condition comes back undecidable, instead of
	// silently comparing against "" (issue #95).
	nodeVars := map[string]any{}
	if state.NodeState != "" {
		nodeVars["state"] = state.NodeState
	}
	activation := map[string]any{
		celVarNode:   nodeVars,
		celVarBudget: map[string]any{"remaining_sessions": state.RemainingSessions},
		celVarInput:  map[string]any{}, celVarOutput: map[string]any{},
		celVarOutcome: "", celVarEvent: map[string]any{},
	}
	for i, program := range n.Continue.While {
		value, _, err := program.Eval(activation)
		if err != nil {
			return ContinuationDecision{EngineFailure: true},
				fmt.Errorf("%w: continue.while[%d]: %w", ErrContinuationUndecidable, i, err)
		}
		ok, err := truthy(value)
		if err != nil {
			return ContinuationDecision{EngineFailure: true},
				fmt.Errorf("%w: continue.while[%d]: %w", ErrContinuationUndecidable, i, err)
		}
		if !ok {
			return ContinuationDecision{}, nil
		}
	}
	return ContinuationDecision{Continue: true}, nil
}

// bindingLiteralKey is the wrapper the authoring schema uses to declare a
// literal binding value.
const bindingLiteralKey = "literal"

// InputBinding is one entry of a node's `bindings` map: a JSON Pointer into
// run, node, or ledger data, or a literal declared in the graph text (issue
// #73). It mirrors schemas/workflow/workflow.schema.json's
// #/$defs/bindingValue.
//
// Like parsePointer in this package, the decode is the engine's own rather than
// a type shared with internal/compiler and internal/worker: each layer reads
// the IR for itself, and tests on each side prove the three readings land in
// the same place. A shared type would make that agreement an assumption instead
// of a check.
type InputBinding struct {
	// Pointer is the RFC 6901 pointer the author wrote as a bare string.
	Pointer string
	// Literal is the declared value's JSON, non-nil exactly when the author
	// wrapped it in `{literal: ...}`. Raw bytes, so the value the human task
	// shows is byte-for-byte the value the workflow digest addresses.
	Literal json.RawMessage
}

// IsLiteral reports whether this binding declares a literal rather than a
// pointer. It asks about the literal, not about an empty pointer, because
// `{literal: ""}` and `{literal: null}` are both legitimate declared values.
func (b InputBinding) IsLiteral() bool { return b.Literal != nil }

func (b *InputBinding) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var pointer string
		if err := json.Unmarshal(data, &pointer); err != nil {
			return err
		}
		*b = InputBinding{Pointer: pointer}
		return nil
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return fmt.Errorf("a binding value is a JSON Pointer string or a {%s: ...} object", bindingLiteralKey)
	}
	literal, ok := members[bindingLiteralKey]
	if !ok || len(members) != 1 {
		return fmt.Errorf("a binding value object declares exactly one member, %q", bindingLiteralKey)
	}
	*b = InputBinding{Literal: literal}
	return nil
}

func (b InputBinding) MarshalJSON() ([]byte, error) {
	if b.IsLiteral() {
		return json.Marshal(map[string]json.RawMessage{bindingLiteralKey: b.Literal})
	}
	return json.Marshal(b.Pointer)
}

// joinThreshold is how many arrivals fire this join node's barrier for a
// group of the given cardinality. The second return is false when the
// barrier can never fire — a quorum larger than the realized cardinality,
// which guarded split edges make reachable at runtime even though the
// authored edge count satisfied it (design §4.2/§4.3: the compiler can only
// validate quorum >= 1 statically; an unsatisfiable barrier resolves loudly,
// never as a silent hang).
func (n *Node) joinThreshold(cardinality int) (int, bool) {
	switch n.JoinPolicy {
	case JoinPolicyAny:
		return 1, cardinality >= 1
	case JoinPolicyQuorum:
		return n.JoinQuorum, n.JoinQuorum >= 1 && n.JoinQuorum <= cardinality
	default: // "all", and hand-built IR that declared nothing
		return cardinality, cardinality >= 1
	}
}

// declaresOutcome reports whether name is an outcome this node can produce.
func (n *Node) declaresOutcome(name string) bool {
	for _, o := range n.Outcomes {
		if o == name {
			return true
		}
	}
	return false
}

func (n *Node) permits(list []string, recordType string) bool {
	for _, t := range list {
		if t == recordType {
			return true
		}
	}
	return false
}

// RetryPolicy is a node's technical-failure retry policy. MaxAttempts counts
// *total* attempts, so 1 means "one attempt, no retry" — the compiler's
// default, because retrying by default would silently re-dispatch work that
// may not be idempotent.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     string
}

// Edge is one eligible transition, with its guard compiled. Its source is
// either a node outcome (FromNode/FromOutcome, the ordinary case) or a named
// external event (OnEvent, issue #43's event routes — design D9). Exactly one
// of the two is set: the compiler's schema makes it a oneOf, and every
// consumer here reads OnEvent != "" as "this is an event edge".
//
// planTransition needs no special case for them: it filters on
// `edge.FromNode != in.NodeID`, and an event edge's FromNode is empty, so no
// node completion can ever select one.
type Edge struct {
	From        string
	FromNode    string
	FromOutcome string
	OnEvent     string
	To          string
	When        string
	Guard       cel.Program
}

// EventEdges are the workflow's `onEvent` edges, in normalized order — what
// CreateRun materializes as the run's durable event routes. Several edges may
// name one event: that is the pickup split (design D9), and it needs no extra
// machinery because one delivery simply matches every one of them.
func (w *Workflow) EventEdges() []Edge {
	var out []Edge
	for _, e := range w.Edges {
		if e.OnEvent != "" {
			out = append(out, e)
		}
	}
	return out
}

// LoadWorkflow decodes a normalized IR document and prepares it for
// execution. digest is the content digest that addresses those bytes; it is
// what a run pins and what the engine caches on.
func LoadWorkflow(digest string, ir []byte) (*Workflow, error) {
	fail := func(format string, args ...any) (*Workflow, error) {
		return nil, &WorkflowError{Digest: digest, Detail: fmt.Sprintf(format, args...)}
	}

	var doc irDocument
	if err := json.Unmarshal(ir, &doc); err != nil {
		return fail("normalized IR could not be decoded: %v", err)
	}
	if doc.Spec.Entry == "" {
		return fail("normalized IR declares no entry node")
	}

	limits, err := decodeLimits(doc.Spec.Limits)
	if err != nil {
		return fail("%v", err)
	}

	wf := &Workflow{
		Digest:  digest,
		Name:    doc.Metadata.Name,
		Version: doc.Metadata.Version,
		Entry:   doc.Spec.Entry,
		Limits:  limits,
		Budget:  decodeBudget(doc.Spec.Budget),
		Ledger: LedgerLimits{
			SchemaVersion:     doc.Spec.Ledger.SchemaVersion,
			MaxRecordsPerNode: valueOr(doc.Spec.Ledger.MaxRecordsPerNode, 0),
		},
		Nodes: make(map[string]*Node, len(doc.Spec.Nodes)),
		Edges: make([]Edge, 0, len(doc.Spec.Edges)),
		IR:    append(json.RawMessage(nil), ir...),
	}

	if wf.InputSchema, err = compileSource(doc.Spec.Contract.Input); err != nil {
		return fail("workflow input contract: %v", err)
	}
	if wf.OutputSchema, err = compileSource(doc.Spec.Contract.Output); err != nil {
		return fail("workflow output contract: %v", err)
	}

	for id, raw := range doc.Spec.Nodes {
		node, err := decodeNode(id, raw)
		if err != nil {
			return fail("node %q: %v", id, err)
		}
		wf.Nodes[id] = node
	}
	if _, ok := wf.Nodes[wf.Entry]; !ok {
		return fail("entry node %q is not declared in the IR", wf.Entry)
	}

	env, err := newCELEnv()
	if err != nil {
		return fail("%v", err)
	}
	for i, e := range doc.Spec.Edges {
		edge := Edge{
			From: e.From, FromNode: e.FromNode, FromOutcome: e.FromOutcome,
			OnEvent: e.OnEvent, To: e.To, When: e.When,
		}
		switch {
		case edge.OnEvent != "":
			// An event edge is sourced by a name, not by a node outcome. Its
			// target still has to exist — the run materializes a route at it.
			if _, ok := wf.Nodes[edge.To]; !ok {
				return fail("event edge %d (onEvent %q) targets node %q, which the IR does not declare", i, edge.OnEvent, edge.To)
			}
		case edge.FromNode == "" || edge.FromOutcome == "":
			return fail("edge %d (%q) is not decomposed into node and outcome", i, e.From)
		}
		if edge.When != "" {
			program, err := compileGuard(env, edge.When)
			if err != nil {
				return fail("edge %d (%q) guard: %v", i, e.From, err)
			}
			edge.Guard = program
		}
		wf.Edges = append(wf.Edges, edge)
	}

	return wf, nil
}

// irDocument mirrors the subset of compiler.IR's JSON shape the runtime
// needs. It is a separate declaration on purpose: the compiler's structs are
// the authoring shape, and the engine should break loudly if a field it
// depends on stops being emitted rather than silently inherit a change.
type irDocument struct {
	Metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"metadata"`
	Spec struct {
		Entry    string `json:"entry"`
		Contract struct {
			Input  *irSchemaSource `json:"input"`
			Output *irSchemaSource `json:"output"`
		} `json:"contract"`
		Limits struct {
			MaxDuration       string `json:"maxDuration"`
			MaxTransitions    *int   `json:"maxTransitions"`
			MaxVisitsPerNode  *int   `json:"maxVisitsPerNode"`
			MaxParallelTokens *int   `json:"maxParallelTokens"`
		} `json:"limits"`
		// A pointer, unlike Limits: the compiler expands no defaults here,
		// so an absent block is the IR saying "unbudgeted" rather than an
		// omission to fill in.
		Budget *struct {
			MaxSessions      *int   `json:"maxSessions"`
			MaxUncachedInput *int64 `json:"maxUncachedInput"`
		} `json:"budget"`
		Ledger struct {
			SchemaVersion     string `json:"schemaVersion"`
			MaxRecordsPerNode *int   `json:"maxRecordsPerNode"`
		} `json:"ledger"`
		Nodes map[string]*irNode `json:"nodes"`
		Edges []struct {
			From        string `json:"from"`
			FromNode    string `json:"fromNode"`
			FromOutcome string `json:"fromOutcome"`
			OnEvent     string `json:"onEvent"`
			To          string `json:"to"`
			When        string `json:"when"`
		} `json:"edges"`
	} `json:"spec"`
}

type irSchemaSource struct {
	SchemaRef string         `json:"schemaRef"`
	Schema    map[string]any `json:"schema"`
	Digest    string         `json:"digest"`
}

type irNode struct {
	Kind     string   `json:"kind"`
	OwnerRef string   `json:"ownerRef"`
	Outcomes []string `json:"outcomes"`
	Contract *struct {
		Outcomes map[string]*irSchemaSource `json:"outcomes"`
	} `json:"contract"`
	Input *struct {
		From     string                  `json:"from"`
		Bindings map[string]InputBinding `json:"bindings"`
	} `json:"input"`
	Output *struct {
		From string `json:"from"`
	} `json:"output"`
	Ledger *struct {
		Propose    []string `json:"propose"`
		Observe    []string `json:"observe"`
		MaxRecords *int     `json:"maxRecords"`
	} `json:"ledger"`
	Policy *struct {
		Timeout string `json:"timeout"`
		Retry   *struct {
			MaxAttempts *int   `json:"maxAttempts"`
			Backoff     string `json:"backoff"`
		} `json:"retry"`
	} `json:"policy"`
	Continue *struct {
		While  []string `json:"while"`
		Bounds struct {
			MaxContinuations *int   `json:"maxContinuations"`
			MaxWallClock     string `json:"maxWallClock"`
			MaxSessions      *int   `json:"maxSessions"`
		} `json:"bounds"`
		OnExhausted string `json:"onExhausted"`
	} `json:"continue"`

	// DecisionSchemaRef, ApproverRef, and Deadline mirror the authoring
	// document's approval-node fields (schemas/workflow/workflow.schema.json,
	// PRD §9.9) straight into the IR — normalization resolves defaults
	// (e.g. the deadline) but does not rename or restructure them.
	DecisionSchemaRef string `json:"decisionSchemaRef"`
	ApproverRef       string `json:"approverRef"`
	Deadline          string `json:"deadline"`

	// Join mirrors a join node's barrier policy block (#/$defs/joinConfig),
	// carried into the IR verbatim.
	Join *struct {
		Policy string `json:"policy"`
		Quorum *int   `json:"quorum"`
	} `json:"join"`
}

func decodeNode(id string, raw *irNode) (*Node, error) {
	if raw == nil {
		return nil, fmt.Errorf("node body is null")
	}
	node := &Node{
		ID:                id,
		Kind:              raw.Kind,
		OwnerRef:          raw.OwnerRef,
		Outcomes:          append([]string(nil), raw.Outcomes...),
		DecisionSchemaRef: raw.DecisionSchemaRef,
		ApproverRef:       raw.ApproverRef,
		// A node with no retry policy gets one attempt. The compiler expands
		// the policy for every kind that dispatches work, so this default
		// only ever applies to kinds that do not.
		Retry: RetryPolicy{MaxAttempts: 1, Backoff: "none"},
	}
	if raw.Input != nil {
		node.InputFrom = raw.Input.From
		if len(raw.Input.Bindings) > 0 {
			node.InputBindings = make(map[string]InputBinding, len(raw.Input.Bindings))
			for k, v := range raw.Input.Bindings {
				node.InputBindings[k] = v
			}
		}
	}
	if raw.Output != nil {
		node.OutputFrom = raw.Output.From
	}
	if raw.Deadline != "" {
		deadline, err := time.ParseDuration(raw.Deadline)
		if err != nil {
			return nil, fmt.Errorf("deadline %q is not a duration: %w", raw.Deadline, err)
		}
		node.Deadline = deadline
	}
	if raw.Join != nil {
		node.JoinPolicy = raw.Join.Policy
		node.JoinQuorum = valueOr(raw.Join.Quorum, 0)
	}
	if raw.Continue != nil {
		cont := &Continuation{OnExhausted: raw.Continue.OnExhausted}
		cont.Bounds.MaxContinuations = valueOr(raw.Continue.Bounds.MaxContinuations, 0)
		cont.Bounds.MaxSessions = valueOr(raw.Continue.Bounds.MaxSessions, 0)
		if raw.Continue.Bounds.MaxWallClock != "" {
			d, err := time.ParseDuration(raw.Continue.Bounds.MaxWallClock)
			if err != nil {
				return nil, fmt.Errorf("continue.bounds.maxWallClock: %w", err)
			}
			cont.Bounds.MaxWallClock = d
		}
		env, err := newCELEnv()
		if err != nil {
			return nil, err
		}
		for i, expression := range raw.Continue.While {
			program, err := compileGuard(env, expression)
			if err != nil {
				return nil, fmt.Errorf("continue.while[%d]: %w", i, err)
			}
			cont.While = append(cont.While, program)
		}
		node.Continue = cont
	}
	if raw.Ledger != nil {
		node.Propose = append([]string(nil), raw.Ledger.Propose...)
		node.Observe = append([]string(nil), raw.Ledger.Observe...)
		node.MaxRecords = valueOr(raw.Ledger.MaxRecords, 0)
	}
	if raw.Policy != nil {
		if raw.Policy.Timeout != "" {
			timeout, err := time.ParseDuration(raw.Policy.Timeout)
			if err != nil {
				return nil, fmt.Errorf("policy.timeout %q is not a duration: %w", raw.Policy.Timeout, err)
			}
			node.Timeout = timeout
		}
		if r := raw.Policy.Retry; r != nil {
			if r.MaxAttempts != nil && *r.MaxAttempts > 0 {
				node.Retry.MaxAttempts = *r.MaxAttempts
			}
			if r.Backoff != "" {
				node.Retry.Backoff = r.Backoff
			}
		}
	}
	if raw.Contract != nil && len(raw.Contract.Outcomes) > 0 {
		node.OutcomeSchemas = make(map[string]*jsonschema.Schema, len(raw.Contract.Outcomes))
		for outcome, source := range raw.Contract.Outcomes {
			compiled, err := compileSource(source)
			if err != nil {
				return nil, fmt.Errorf("contract.outcomes.%s: %w", outcome, err)
			}
			if compiled != nil {
				node.OutcomeSchemas[outcome] = compiled
			}
		}
	}
	return node, nil
}

func decodeLimits(raw struct {
	MaxDuration       string `json:"maxDuration"`
	MaxTransitions    *int   `json:"maxTransitions"`
	MaxVisitsPerNode  *int   `json:"maxVisitsPerNode"`
	MaxParallelTokens *int   `json:"maxParallelTokens"`
}) (Limits, error) {
	limits := Limits{
		MaxTransitions:    valueOr(raw.MaxTransitions, 0),
		MaxVisitsPerNode:  valueOr(raw.MaxVisitsPerNode, 0),
		MaxParallelTokens: valueOr(raw.MaxParallelTokens, 1),
	}
	if raw.MaxDuration != "" {
		d, err := time.ParseDuration(raw.MaxDuration)
		if err != nil {
			return Limits{}, fmt.Errorf("limits.maxDuration %q is not a duration: %w", raw.MaxDuration, err)
		}
		limits.MaxDuration = d
	}
	return limits, nil
}

// decodeBudget reads the optional economic block. An absent block, and an
// absent field within one, are both "no bound on this axis" — zero, which
// every enforcement site tests with `> 0`. The compiler has already refused
// an authored 0, so a zero here can only ever mean absent.
func decodeBudget(raw *struct {
	MaxSessions      *int   `json:"maxSessions"`
	MaxUncachedInput *int64 `json:"maxUncachedInput"`
}) Budget {
	if raw == nil {
		return Budget{}
	}
	budget := Budget{}
	if raw.MaxSessions != nil {
		budget.MaxSessions = *raw.MaxSessions
	}
	if raw.MaxUncachedInput != nil {
		budget.MaxUncachedInput = *raw.MaxUncachedInput
	}
	return budget
}

func valueOr(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// compileSource compiles an inline schema. A nil source, or one that only
// carries a schemaRef, yields a nil schema — an unresolved reference is not a
// contract this process can check, and treating it as one would be a lie
// about what was verified.
func compileSource(source *irSchemaSource) (*jsonschema.Schema, error) {
	if source == nil || source.Schema == nil {
		return nil, nil
	}
	canonical, err := contracts.CanonicalJSON(source.Schema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	const inlineURI = "https://nodes.culture.dev/inline/schema.json"
	if err := compiler.AddResource(inlineURI, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(inlineURI)
}

// validatePayload validates raw JSON against a compiled schema. A nil schema
// admits anything, which is the honest reading of "no checkable contract".
func validatePayload(schema *jsonschema.Schema, payload json.RawMessage) error {
	if schema == nil {
		return nil
	}
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	return schema.Validate(doc)
}

func newCELEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable(celVarInput, cel.DynType),
		cel.Variable(celVarOutput, cel.DynType),
		cel.Variable(celVarOutcome, cel.DynType),
		cel.Variable(celVarEvent, cel.DynType),
		cel.Variable(celVarNode, cel.DynType),
		cel.Variable(celVarBudget, cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("build CEL environment: %w", err)
	}
	return env, nil
}

func compileGuard(env *cel.Env, expression string) (cel.Program, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	return env.Program(ast)
}
