package worker

import (
	"encoding/json"
	"fmt"
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
	}
	return node, nil
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
