package compiler

import "encoding/json"

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

	Presentation map[string]any `json:"presentation,omitempty"`

	// PreRun/PostRun are declared but unvalidated in the t2 schema; the
	// compiler carries them through the IR untouched so authoring can begin
	// before task t14 specifies their contract.
	PreRun  json.RawMessage `json:"pre_run,omitempty"`
	PostRun json.RawMessage `json:"post_run,omitempty"`

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

type ledgerDelta struct {
	Read               []string `json:"read,omitempty"`
	Propose            []string `json:"propose,omitempty"`
	Observe            []string `json:"observe,omitempty"`
	MaxRecords         *int     `json:"maxRecords,omitempty"`
	RequireHumanReview *bool    `json:"requireHumanReview,omitempty"`
}

type acceptance struct {
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
