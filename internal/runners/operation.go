package runners

import "encoding/json"

// Operation is the Go mirror of schemas/runner/operation.schema.json — a
// typed code-execution operation (PRD §13.7), kept runner-neutral so the
// headspace Docker runner and the Lambda container-image adapter map onto
// the same document.
//
// The JSON tags are the contract. Optional fields carry omitempty because
// the schema sets additionalProperties:false and gives several fields a
// minLength or enum an empty string would violate — an absent field is
// absent, never an empty stand-in.
type Operation struct {
	// OperationID is the caller-assigned identity and the idempotency key
	// for dispatch.
	OperationID string `json:"operation_id"`
	// Runner is the logical runner name ("headspace", "lambda"). The engine
	// does not branch on it; adapters register under it.
	Runner string `json:"runner"`
	// RunnerRevision pins the runner revision the caller expects, and is
	// part of the replay manifest.
	RunnerRevision string `json:"runner_revision"`
	// Workspace is the immutable input workspace, nil when the operation
	// genuinely has none. Absence is explicit.
	Workspace *Workspace `json:"workspace,omitempty"`
	// Execution is the pinned execution environment.
	Execution Execution `json:"execution"`
	// Command is what to run inside the pinned image.
	Command Command `json:"command"`
	// Policy is the limit set dispatch must enforce.
	Policy Policy `json:"policy"`
	// Evidence is what the caller asks the runner to observe. A runner that
	// cannot honour a request says so in the Result's Observations rather
	// than fabricating the observation.
	Evidence EvidenceRequest `json:"evidence"`
	// Context correlates back to the graph. The runner treats it as opaque.
	Context *Context `json:"context,omitempty"`
}

// WriteMode is how a runner may write to the input workspace.
type WriteMode string

// The write modes the schema admits. CopyOnWrite is the safe default; inputs
// stay immutable either way.
const (
	WriteModeReadOnly    WriteMode = "read-only"
	WriteModeCopyOnWrite WriteMode = "copy-on-write"
)

// Workspace is the immutable input workspace an operation runs against.
type Workspace struct {
	SourceRef    string    `json:"source_ref"`
	SourceDigest string    `json:"source_digest"`
	WriteMode    WriteMode `json:"write_mode"`
}

// ExecutionKind distinguishes an OCI image run by an isolating runner from a
// managed function backed by a container image. Both are pinned by image
// digest — an unpinned tag is not an execution environment.
type ExecutionKind string

// The execution kinds the schema admits.
const (
	ExecutionContainer ExecutionKind = "container"
	ExecutionFunction  ExecutionKind = "function"
)

// Execution is the pinned execution environment.
type Execution struct {
	Kind ExecutionKind `json:"kind"`
	// ImageRef is a registry reference or a registered function identity.
	// Dispatch to an identity absent from the runner's registry is refused
	// by the adapter (see FunctionRegistry.Resolve), not by the schema.
	ImageRef    string `json:"image_ref,omitempty"`
	ImageDigest string `json:"image_digest"`
	// User is the non-root execution identity when the runner can set one.
	User string `json:"user,omitempty"`
	// ReadOnlyRoot is a pointer because false and absent are different
	// statements: false declares a writable root, absent declares nothing.
	ReadOnlyRoot *bool `json:"read_only_root,omitempty"`
}

// Command is what to execute inside the pinned image. Argv is an argument
// array, never a shell string.
type Command struct {
	Argv             []string `json:"argv"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	// EnvironmentRefs names individually granted environment values. No
	// implicit environment is inherited and secrets are absent unless
	// granted.
	EnvironmentRefs []string `json:"environment_refs,omitempty"`
	// RequiresShell is declared explicitly when a shell is genuinely
	// required, so policy can reject the operation instead of discovering
	// the shell at runtime.
	RequiresShell *bool `json:"requires_shell,omitempty"`
}

// commandWire preserves the difference between an absent environment_refs and
// an explicitly empty one. It matters: an empty list is the operation stating
// that no environment value was granted, which is the safe-default posture
// worth saying out loud, while an absent list states nothing at all.
type commandWire struct {
	Argv             []string  `json:"argv"`
	WorkingDirectory string    `json:"working_directory,omitempty"`
	EnvironmentRefs  *[]string `json:"environment_refs,omitempty"`
	RequiresShell    *bool     `json:"requires_shell,omitempty"`
}

// MarshalJSON emits an explicitly empty environment_refs as [] and an unset
// one not at all.
func (c Command) MarshalJSON() ([]byte, error) {
	w := commandWire{
		Argv:             c.Argv,
		WorkingDirectory: c.WorkingDirectory,
		RequiresShell:    c.RequiresShell,
	}
	if c.EnvironmentRefs != nil {
		w.EnvironmentRefs = &c.EnvironmentRefs
	}
	return json.Marshal(w)
}

// NetworkMode is the operation's network posture. NetworkNone is the safe
// default.
type NetworkMode string

// The network modes the schema admits.
const (
	NetworkNone            NetworkMode = "none"
	NetworkEgressAllowlist NetworkMode = "egress-allowlist"
	NetworkFull            NetworkMode = "full"
)

// Policy is the limit set enforced inside dispatch. Fields a given runner
// cannot enforce must be rejected by that adapter rather than silently
// ignored.
type Policy struct {
	// TimeoutSeconds is the wall-clock bound. An adapter whose platform caps
	// duration below this value rejects the operation at validate time,
	// naming the cap (see lambda.Adapter.Execute).
	TimeoutSeconds int `json:"timeout_seconds"`
	// CPU is omitted when the runner derives CPU from memory rather than
	// accepting it, which is exactly the Lambda case.
	CPU             *float64    `json:"cpu,omitempty"`
	MemoryMiB       *int        `json:"memory_mib,omitempty"`
	PIDs            *int        `json:"pids,omitempty"`
	DiskMiB         *int        `json:"disk_mib,omitempty"`
	Network         NetworkMode `json:"network"`
	EgressAllowlist []string    `json:"egress_allowlist,omitempty"`
	// AllowedOutputPaths is always emitted, including as an empty array:
	// "no writable output is permitted" is a statement the document should
	// make out loud rather than by omission.
	AllowedOutputPaths []string `json:"allowed_output_paths"`
}

// MarshalJSON emits AllowedOutputPaths as [] rather than null when unset, so
// the document always states the output policy explicitly. The alias type
// drops the method set, which is what stops this from recursing.
func (p Policy) MarshalJSON() ([]byte, error) {
	type alias Policy
	w := alias(p)
	if w.AllowedOutputPaths == nil {
		w.AllowedOutputPaths = []string{}
	}
	return json.Marshal(w)
}

// EvidenceRequest is what the caller asks the runner to observe. The four
// required flags are always emitted: asking for nothing is a statement too.
type EvidenceRequest struct {
	SnapshotBefore bool `json:"snapshot_before"`
	SnapshotAfter  bool `json:"snapshot_after"`
	CaptureExit    bool `json:"capture_exit"`
	CaptureLogs    bool `json:"capture_logs"`
	// CaptureResourceUsage is optional in the schema, so absent (nil) and
	// "explicitly not requested" (false) stay distinguishable.
	CaptureResourceUsage *bool `json:"capture_resource_usage,omitempty"`
}

// Context correlates an operation back to the run, node run, and attempt
// that caused it.
type Context struct {
	RunID     string `json:"run_id,omitempty"`
	NodeRunID string `json:"node_run_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}
