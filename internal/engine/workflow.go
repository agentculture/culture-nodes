package engine

import (
	"bytes"
	"encoding/json"
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
// outcome's name.
const (
	celVarInput   = "input"
	celVarOutput  = "output"
	celVarOutcome = "outcome"
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
	InputFrom     string
	InputBindings map[string]string

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

// Edge is one eligible transition, with its guard compiled.
type Edge struct {
	From        string
	FromNode    string
	FromOutcome string
	To          string
	When        string
	Guard       cel.Program
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
		edge := Edge{From: e.From, FromNode: e.FromNode, FromOutcome: e.FromOutcome, To: e.To, When: e.When}
		if edge.FromNode == "" || edge.FromOutcome == "" {
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
		Ledger struct {
			SchemaVersion     string `json:"schemaVersion"`
			MaxRecordsPerNode *int   `json:"maxRecordsPerNode"`
		} `json:"ledger"`
		Nodes map[string]*irNode `json:"nodes"`
		Edges []struct {
			From        string `json:"from"`
			FromNode    string `json:"fromNode"`
			FromOutcome string `json:"fromOutcome"`
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
		From     string            `json:"from"`
		Bindings map[string]string `json:"bindings"`
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

	// DecisionSchemaRef, ApproverRef, and Deadline mirror the authoring
	// document's approval-node fields (schemas/workflow/workflow.schema.json,
	// PRD §9.9) straight into the IR — normalization resolves defaults
	// (e.g. the deadline) but does not rename or restructure them.
	DecisionSchemaRef string `json:"decisionSchemaRef"`
	ApproverRef       string `json:"approverRef"`
	Deadline          string `json:"deadline"`
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
			node.InputBindings = make(map[string]string, len(raw.Input.Bindings))
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
