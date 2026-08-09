package worker

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// The worker's view of the normalized IR.
//
// internal/engine already decodes the IR, but into the fields *it* needs:
// outcomes, contracts, limits, edges. The worker needs a different set —
// input bindings, the actor reference, decision ports, the node timeout — and
// none of those appear in engine.Node. Rather than widen a merged type that
// two packages would then both have to agree about, each package decodes the
// subset it uses. The IR bytes are the shared contract; the structs are not.
//
// The same reasoning the engine states applies here: the runtime executes the
// normalized IR and never the compiler's in-memory document, because after a
// restart the IR bytes are the only thing that exists.

// workflowSpec is one pinned definition, decoded once per digest.
type workflowSpec struct {
	Digest  string
	Name    string
	Version string
	Nodes   map[string]*nodeSpec
}

// nodeSpec is the worker's view of one node.
type nodeSpec struct {
	ID   string
	Kind string
	// Uses is the declared component reference, e.g. an actor or runner ref.
	// The worker passes it to a Registry; it never parses a provider out of
	// it, because there is no provider in it to parse (§9.5).
	Uses string
	// ContractDigest is the content digest of the node's contract block —
	// §13.1's `node.contract_digest`. It is computed from the IR rather than
	// carried by it, because the compiler does not currently emit a per-node
	// contract digest, and a digest an actor can verify has to be over
	// something the actor also sees.
	ContractDigest string
	// Outcomes are the domain outcomes the node's contract declares, sorted.
	// The engine keeps its own resolved set (engine.Node.Outcomes, which also
	// folds in decision ports and kind-implied ports); this is the narrower
	// authored list, and it is here because a code node's exit status has to
	// be mapped onto one of them before dispatch (see code.go).
	Outcomes []string
	// Input is the node's declared input binding (§11.2), nil when it
	// declares none.
	Input *inputBinding
	// Select are a decision node's guarded output ports.
	Select []selectPort
	// Timeout is the node's policy timeout, zero when it declares none.
	Timeout time.Duration
	// Deadline is an approval node's declared deadline, zero when none.
	Deadline time.Duration
	// Operation is a code node's typed operation, carried verbatim for the
	// runner seam to interpret (§13.7).
	Operation json.RawMessage
	// ApproverRef is an approval node's requested approver.
	ApproverRef string
	// Until is a wait node's resume condition, carried verbatim.
	Until json.RawMessage
	// PreRun/PostRun are the node's declared code hooks (task t14, spec claim
	// c37), nil when the node declares neither. The compiler's checkNodeHooks
	// already refused any node but an agent from declaring one, so the worker
	// does not re-check kind here — it only interprets what compiled.
	PreRun  *hookSpec
	PostRun *postRunHookSpec
}

// hookSpec is the worker's decoded view of a pre_run hook.
type hookSpec struct {
	Operation codeOperationSpec
}

// postRunHookSpec is the worker's decoded view of a post_run hook.
type postRunHookSpec struct {
	Operation codeOperationSpec
	OnFailure hookOnFailureSpec
}

// hookOnFailureSpec is a post-run hook's declared failure routing: either a
// domain outcome the node declares, or the reject_assurance sentinel. See
// internal/compiler's identically-shaped hookOnFailure for why this is a
// hand-written (Un)MarshalJSON pair rather than a plain struct: the schema's
// oneOf(object|const) shape has to survive the decode without a third,
// inferred state.
type hookOnFailureSpec struct {
	Outcome         string
	RejectAssurance bool
}

const hookOnFailureRejectAssurance = "reject_assurance"

// UnmarshalJSON accepts exactly the two shapes the schema's oneOf admits.
func (h *hookOnFailureSpec) UnmarshalJSON(data []byte) error {
	var sentinel string
	if err := json.Unmarshal(data, &sentinel); err == nil {
		if sentinel != hookOnFailureRejectAssurance {
			return fmt.Errorf("on_failure %q is not the %q sentinel", sentinel, hookOnFailureRejectAssurance)
		}
		*h = hookOnFailureSpec{RejectAssurance: true}
		return nil
	}
	var obj struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("on_failure is neither the %q sentinel nor an object with an outcome: %w", hookOnFailureRejectAssurance, err)
	}
	*h = hookOnFailureSpec{Outcome: obj.Outcome}
	return nil
}

// codeOperationSpec is the worker's decoded view of a code operation — a code
// node's own, or a pre_run/post_run hook's (they share one authoring shape,
// schemas/workflow/workflow.schema.json's #/$defs/codeOperation). It carries
// exactly what buildHookOperation needs to construct a runners.Operation.
type codeOperationSpec struct {
	WorkspaceRef       string   `json:"workspaceRef,omitempty"`
	Image              string   `json:"image"`
	Argv               []string `json:"argv"`
	WorkingDirectory   string   `json:"workingDirectory,omitempty"`
	EnvironmentRefs    []string `json:"environmentRefs,omitempty"`
	Network            string   `json:"network,omitempty"`
	AllowedOutputPaths []string `json:"allowedOutputPaths,omitempty"`
	RequiresShell      bool     `json:"requiresShell,omitempty"`
}

type inputBinding struct {
	From     string            `json:"from,omitempty"`
	Bindings map[string]string `json:"bindings,omitempty"`
}

// declared reports whether the node declares any input binding at all.
func (b *inputBinding) declared() bool {
	return b != nil && (b.From != "" || len(b.Bindings) > 0)
}

type selectPort struct {
	Outcome string `json:"outcome"`
	When    string `json:"when"`
}

// irDocument mirrors the subset of the IR the worker reads.
type irDocument struct {
	Metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"metadata"`
	Spec struct {
		Nodes map[string]*irNode `json:"nodes"`
	} `json:"spec"`
}

type irNode struct {
	Kind        string          `json:"kind"`
	Uses        string          `json:"uses"`
	Contract    map[string]any  `json:"contract"`
	Input       *inputBinding   `json:"input"`
	Select      []selectPort    `json:"select"`
	Operation   json.RawMessage `json:"operation"`
	ApproverRef string          `json:"approverRef"`
	Deadline    string          `json:"deadline"`
	Until       json.RawMessage `json:"until"`
	Policy      *struct {
		Timeout string `json:"timeout"`
	} `json:"policy"`
	PreRun *struct {
		Operation codeOperationSpec `json:"operation"`
	} `json:"pre_run"`
	PostRun *struct {
		Operation codeOperationSpec `json:"operation"`
		OnFailure hookOnFailureSpec `json:"on_failure"`
	} `json:"post_run"`
}

// loadWorkflowSpec decodes a normalized IR document into the worker's view.
func loadWorkflowSpec(digest string, ir []byte) (*workflowSpec, error) {
	var doc irDocument
	if err := json.Unmarshal(ir, &doc); err != nil {
		return nil, fmt.Errorf("worker: workflow %s: normalized IR could not be decoded: %w", digest, err)
	}
	spec := &workflowSpec{
		Digest:  digest,
		Name:    doc.Metadata.Name,
		Version: doc.Metadata.Version,
		Nodes:   make(map[string]*nodeSpec, len(doc.Spec.Nodes)),
	}
	for id, raw := range doc.Spec.Nodes {
		node, err := decodeNode(id, raw)
		if err != nil {
			return nil, fmt.Errorf("worker: workflow %s: node %q: %w", digest, id, err)
		}
		spec.Nodes[id] = node
	}
	return spec, nil
}

func decodeNode(id string, raw *irNode) (*nodeSpec, error) {
	if raw == nil {
		return nil, fmt.Errorf("node body is null")
	}
	node := &nodeSpec{
		ID:          id,
		Kind:        raw.Kind,
		Uses:        raw.Uses,
		Input:       raw.Input,
		Select:      append([]selectPort(nil), raw.Select...),
		Operation:   raw.Operation,
		ApproverRef: raw.ApproverRef,
		Until:       raw.Until,
	}
	if raw.PreRun != nil {
		node.PreRun = &hookSpec{Operation: raw.PreRun.Operation}
	}
	if raw.PostRun != nil {
		node.PostRun = &postRunHookSpec{Operation: raw.PostRun.Operation, OnFailure: raw.PostRun.OnFailure}
	}
	if raw.Policy != nil && raw.Policy.Timeout != "" {
		timeout, err := time.ParseDuration(raw.Policy.Timeout)
		if err != nil {
			return nil, fmt.Errorf("policy.timeout %q is not a duration: %w", raw.Policy.Timeout, err)
		}
		node.Timeout = timeout
	}
	if raw.Deadline != "" {
		deadline, err := time.ParseDuration(raw.Deadline)
		if err != nil {
			return nil, fmt.Errorf("deadline %q is not a duration: %w", raw.Deadline, err)
		}
		node.Deadline = deadline
	}
	if len(raw.Contract) > 0 {
		// Canonical JSON then SHA-256: the same content-addressing every
		// other digest in this system uses, so an actor that computes the
		// digest of the contract it holds gets the same answer.
		digest, err := contracts.DigestValue(raw.Contract)
		if err != nil {
			return nil, fmt.Errorf("contract digest: %w", err)
		}
		node.ContractDigest = digest
		node.Outcomes = declaredOutcomes(raw.Contract)
	}
	return node, nil
}

// declaredOutcomes reads the outcome names out of a node's contract block,
// sorted so a diagnostic that lists them reads the same way every time.
func declaredOutcomes(contract map[string]any) []string {
	outcomes, ok := contract["outcomes"].(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// specCache memoizes decoded definitions by content digest. A digest
// addresses immutable bytes, so a cached entry can never be stale.
type specCache struct {
	mu     sync.Mutex
	byHash map[string]*workflowSpec
}

func newSpecCache() *specCache {
	return &specCache{byHash: make(map[string]*workflowSpec)}
}

func (c *specCache) get(digest string, ir []byte) (*workflowSpec, error) {
	c.mu.Lock()
	cached, ok := c.byHash[digest]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	loaded, err := loadWorkflowSpec(digest, ir)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if existing, ok := c.byHash[digest]; ok {
		loaded = existing
	} else {
		c.byHash[digest] = loaded
	}
	c.mu.Unlock()
	return loaded, nil
}
