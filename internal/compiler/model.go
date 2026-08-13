package compiler

import (
	"encoding/json"
	"fmt"
)

// The types below mirror schemas/workflow/workflow.schema.json. They are the
// authoring shape *and*, after normalization, the IR shape: normalization
// fills in defaults and resolves owners in place rather than copying the
// document into a parallel set of structs that would drift from the schema.
//
// Every optional field is a pointer or carries omitempty, so a document that
// omits a field round-trips as omitted — which matters because the IR's bytes
// are what the content digest addresses.

type document struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       spec     `json:"spec"`
}

type metadata struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	OwnerRef    string            `json:"ownerRef,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type spec struct {
	Entry        string           `json:"entry"`
	Contract     workflowContract `json:"contract"`
	Limits       *limits          `json:"limits,omitempty"`
	Ledger       *ledgerLimits    `json:"ledger,omitempty"`
	Nodes        map[string]*node `json:"nodes"`
	Edges        []edge           `json:"edges"`
	Presentation map[string]any   `json:"presentation,omitempty"`
}

type workflowContract struct {
	Input  *schemaSource `json:"input,omitempty"`
	Output *schemaSource `json:"output,omitempty"`
}

type schemaSource struct {
	SchemaRef string         `json:"schemaRef,omitempty"`
	Schema    map[string]any `json:"schema,omitempty"`
	Digest    string         `json:"digest,omitempty"`
}

type limits struct {
	MaxDuration       string `json:"maxDuration,omitempty"`
	MaxTransitions    *int   `json:"maxTransitions,omitempty"`
	MaxVisitsPerNode  *int   `json:"maxVisitsPerNode,omitempty"`
	MaxParallelTokens *int   `json:"maxParallelTokens,omitempty"`
}

// bounded reports whether the author declared any loop bound at all. It is
// deliberately about the *authored* limits: normalization expands defaults, so
// asking the normalized document would always answer yes and the
// "no loop relies on an agent deciding when to stop" rule (PRD §9.7) would
// never be able to speak.
func (l *limits) bounded() bool {
	return l != nil && (l.MaxTransitions != nil || l.MaxVisitsPerNode != nil)
}

type ledgerLimits struct {
	SchemaVersion     string `json:"schemaVersion,omitempty"`
	MaxRecordsPerNode *int   `json:"maxRecordsPerNode,omitempty"`
	MaxPayloadBytes   *int   `json:"maxPayloadBytes,omitempty"`
	RequireProvenance *bool  `json:"requireProvenance,omitempty"`
}

type node struct {
	Kind     string `json:"kind"`
	OwnerRef string `json:"ownerRef,omitempty"`
	Uses     string `json:"uses,omitempty"`

	Contract  *nodeContract  `json:"contract,omitempty"`
	Input     *inputBinding  `json:"input,omitempty"`
	Output    *outputBinding `json:"output,omitempty"`
	Operation *codeOperation `json:"operation,omitempty"`
	Ledger    *ledgerDelta   `json:"ledger,omitempty"`

	Acceptance *acceptance `json:"acceptance,omitempty"`
	Policy     *nodePolicy `json:"policy,omitempty"`

	DecisionSchemaRef string       `json:"decisionSchemaRef,omitempty"`
	ApproverRef       string       `json:"approverRef,omitempty"`
	Deadline          string       `json:"deadline,omitempty"`
	Select            []selectPort `json:"select,omitempty"`
	Until             *until       `json:"until,omitempty"`
	Join              *joinConfig  `json:"join,omitempty"`

	Presentation map[string]any `json:"presentation,omitempty"`

	// PreRun/PostRun are the t14 code-hook contract (spec claim c37): a
	// pre-run operation runs through the runner boundary before an agent
	// node's actor is dispatched, and a post-run operation runs after the
	// actor returns. checkNodeHooks enforces that only agent nodes declare
	// either. The compiler carries them into the IR unchanged, exactly as it
	// does the node's own operation.
	PreRun  *preRunHook  `json:"pre_run,omitempty"`
	PostRun *postRunHook `json:"post_run,omitempty"`

	// Outcomes is IR-only: the resolved set of domain outcomes this node can
	// produce, sorted. The engine reads it instead of re-deriving the union of
	// contract outcomes, decision ports, and kind-implied ports.
	Outcomes []string `json:"outcomes,omitempty"`
}

type nodeContract struct {
	Input                 *schemaSource            `json:"input,omitempty"`
	Outcomes              map[string]*schemaSource `json:"outcomes,omitempty"`
	Error                 *schemaSource            `json:"error,omitempty"`
	MaxInlinePayloadBytes *int                     `json:"maxInlinePayloadBytes,omitempty"`
	ArtifactTypes         []string                 `json:"artifactTypes,omitempty"`
}

type inputBinding struct {
	From     string            `json:"from,omitempty"`
	Bindings map[string]string `json:"bindings,omitempty"`
}

type outputBinding struct {
	From string `json:"from"`
}

type codeOperation struct {
	WorkspaceRef       string   `json:"workspaceRef,omitempty"`
	Image              string   `json:"image"`
	Argv               []string `json:"argv"`
	WorkingDirectory   string   `json:"workingDirectory,omitempty"`
	EnvironmentRefs    []string `json:"environmentRefs,omitempty"`
	Network            string   `json:"network,omitempty"`
	AllowedOutputPaths []string `json:"allowedOutputPaths,omitempty"`
	RequiresShell      *bool    `json:"requiresShell,omitempty"`
}

// preRunHook mirrors schemas/workflow/workflow.schema.json's #/$defs/preRunHook.
type preRunHook struct {
	Operation codeOperation `json:"operation"`
}

// postRunHook mirrors #/$defs/postRunHook.
type postRunHook struct {
	Operation codeOperation `json:"operation"`
	OnFailure hookOnFailure `json:"on_failure"`
}

// hookOnFailureRejectAssurance is the sentinel string #/$defs/hookOnFailure's
// oneOf admits alongside an {"outcome": "..."} object.
const hookOnFailureRejectAssurance = "reject_assurance"

// hookOnFailure is a post-run hook's declared failure routing (PRD-adjacent
// honesty condition h32): either a domain outcome the node declares, so a
// workflow can steer a failed check down its own edge, or the
// reject_assurance sentinel, which records a derived assurance rejection
// while the agent's own domain outcome still stands. It mirrors the schema's
// oneOf(object|const) shape by hand — via custom (Un)MarshalJSON — so the IR
// carries exactly what was authored, never a third, inferred state.
type hookOnFailure struct {
	// Outcome is set when the author wrote {"outcome": "..."}.
	Outcome string `json:"-"`
	// RejectAssurance is true when the author wrote the "reject_assurance"
	// sentinel.
	RejectAssurance bool `json:"-"`
}

// MarshalJSON renders the sentinel string or the {"outcome": ...} object,
// matching whichever the author wrote.
func (h hookOnFailure) MarshalJSON() ([]byte, error) {
	if h.RejectAssurance {
		return json.Marshal(hookOnFailureRejectAssurance)
	}
	return json.Marshal(struct {
		Outcome string `json:"outcome"`
	}{Outcome: h.Outcome})
}

// UnmarshalJSON accepts exactly the two shapes the schema's oneOf admits. Any
// other string is a decode error rather than a silently-accepted third state
// — the schema level already reports the same document as invalid, with a
// pointer, so this is a second, independent no.
func (h *hookOnFailure) UnmarshalJSON(data []byte) error {
	var sentinel string
	if err := json.Unmarshal(data, &sentinel); err == nil {
		if sentinel != hookOnFailureRejectAssurance {
			return fmt.Errorf("on_failure %q is not the %q sentinel", sentinel, hookOnFailureRejectAssurance)
		}
		*h = hookOnFailure{RejectAssurance: true}
		return nil
	}
	var obj struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("on_failure is neither the %q sentinel nor an object with an outcome: %w", hookOnFailureRejectAssurance, err)
	}
	*h = hookOnFailure{Outcome: obj.Outcome}
	return nil
}

type ledgerDelta struct {
	Read               []string `json:"read,omitempty"`
	Propose            []string `json:"propose,omitempty"`
	Observe            []string `json:"observe,omitempty"`
	MaxRecords         *int     `json:"maxRecords,omitempty"`
	RequireHumanReview *bool    `json:"requireHumanReview,omitempty"`
}

type acceptance struct {
	// Enforce is the issue #37 policy for a failed check: "observe" (also the
	// meaning of an omitted field — the default is documented in the schema,
	// never materialized here, so it re-digests no published workflow),
	// "route_technical", or "route_outcome:<name>" where <name> is a domain
	// outcome the node itself declares.
	Enforce  string           `json:"enforce,omitempty"`
	Requires []map[string]any `json:"requires"`
}

type nodePolicy struct {
	Timeout            string       `json:"timeout,omitempty"`
	Retry              *retryPolicy `json:"retry,omitempty"`
	DataClassification string       `json:"dataClassification,omitempty"`
	PolicySet          string       `json:"policySet,omitempty"`
	EscalationContact  string       `json:"escalationContact,omitempty"`
}

type retryPolicy struct {
	MaxAttempts *int   `json:"maxAttempts,omitempty"`
	Backoff     string `json:"backoff,omitempty"`
}

type selectPort struct {
	Outcome string `json:"outcome"`
	When    string `json:"when"`
}

type until struct {
	Duration  string `json:"duration,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Signal    string `json:"signal,omitempty"`
}

// joinConfig mirrors schemas/workflow/workflow.schema.json's #/$defs/joinConfig
// (issue #43): a join node's barrier policy. Quorum is a pointer so an omitted
// value round-trips as omitted — checkParallelJoin enforces "required iff
// policy == quorum" as value semantics the schema cannot state.
type joinConfig struct {
	Policy string `json:"policy"`
	Quorum *int   `json:"quorum,omitempty"`
}

// Join policies the schema's enum admits.
const (
	JoinPolicyAll    = "all"
	JoinPolicyAny    = "any"
	JoinPolicyQuorum = "quorum"
)

type edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}

// IR is the normalized representation the runtime executes (PRD §11.3).
// Presentation metadata is lifted out of spec into its own block: presentation
// never changes runtime semantics (PRD §9.1), and a reader of the executable
// spec should not have to skip past layout hints to find it.
type IR struct {
	APIVersion   string        `json:"apiVersion"`
	Kind         string        `json:"kind"`
	Metadata     metadata      `json:"metadata"`
	Spec         irSpec        `json:"spec"`
	Presentation *presentation `json:"presentation,omitempty"`
}

type irSpec struct {
	Entry    string           `json:"entry"`
	Contract workflowContract `json:"contract"`
	Limits   limits           `json:"limits"`
	Ledger   ledgerLimits     `json:"ledger"`
	Nodes    map[string]*node `json:"nodes"`
	Edges    []irEdge         `json:"edges"`
}

// irEdge keeps the authored `from` string and adds its decomposition, so the
// engine never re-parses "<node>.<outcome>" at dispatch time.
type irEdge struct {
	From        string `json:"from"`
	FromNode    string `json:"fromNode"`
	FromOutcome string `json:"fromOutcome"`
	To          string `json:"to"`
	When        string `json:"when,omitempty"`
}

type presentation struct {
	Workflow map[string]any            `json:"workflow,omitempty"`
	Nodes    map[string]map[string]any `json:"nodes,omitempty"`
}
