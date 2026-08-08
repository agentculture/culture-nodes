package runners

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Result is the Go mirror of schemas/runner/result.schema.json — the
// structured result a runner returns for an Operation (PRD §13.7).
//
// The document's whole point is Observations: every result declares, per
// observation, whether the runner directly measured the fact and whether the
// measurement covered the whole declared scope. Text printed by the executed
// process is process-reported content and never an observation about that
// process's own behaviour.
type Result struct {
	OperationID string `json:"operation_id"`
	// State is the *technical* state of the operation. It is not a domain
	// outcome: mapping a result onto a node's declared outcome is the
	// engine's job (PRD §3.4), which BuildCompletion performs.
	State State `json:"state"`
	// Exit is the process exit, required when the operation actually ran.
	Exit *Exit `json:"exit,omitempty"`
	// Timing is when the operation ran and for how long.
	Timing Timing `json:"timing"`
	// Environment carries the digests that make the operation replayable.
	Environment Environment `json:"environment"`
	// Changes describes workspace changes. Complete is true only when the
	// runner controlled and compared the entire relevant workspace.
	Changes Changes `json:"changes"`
	// Artifacts references stored artifacts. Log references point at
	// complete stored logs even when the inline capture was bounded.
	Artifacts *Artifacts `json:"artifacts,omitempty"`
	// ResourceUsage holds measurements the runner directly observed. Its
	// completeness is declared in Observations, not here.
	ResourceUsage *ResourceUsage `json:"resource_usage,omitempty"`
	// Observations is the honesty block. The four named keys are required.
	Observations Observations `json:"observations"`
	// Error is set when State is not StateCompleted.
	Error *ResultError `json:"error,omitempty"`
}

// State is a runner result's technical state.
type State string

// The states the schema admits.
const (
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateTimedOut  State = "timed_out"
	StateCancelled State = "cancelled"
	StateRejected  State = "rejected"
)

// Exit is the process exit as observed — or as reported — for an operation.
// Both fields are always emitted, because null is an explicit answer here:
// a null Code says the process did not exit normally, never a stand-in zero.
type Exit struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
}

// ExitCode returns the exit code and whether one was reported at all.
func (r Result) ExitCode() (int, bool) {
	if r.Exit == nil || r.Exit.Code == nil {
		return 0, false
	}
	return *r.Exit.Code, true
}

// Timing is when the operation ran. BilledDurationMs is reported only by
// runners that bill execution.
type Timing struct {
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	DurationMs       int       `json:"duration_ms"`
	BilledDurationMs *int      `json:"billed_duration_ms,omitempty"`
}

// Environment holds the digests that make an operation replayable. Replay may
// still be nondeterministic when dependencies, clocks, randomness, or network
// were allowed; the ledger records those allowances rather than claiming
// hermetic behaviour.
type Environment struct {
	RunnerRevision string `json:"runner_revision"`
	ImageDigest    string `json:"image_digest"`
	// InputDigest is always emitted, as null when the operation had no
	// workspace, so "no workspace" is visible rather than inferred from a
	// missing key.
	InputDigest  *string `json:"input_digest"`
	PolicyDigest string  `json:"policy_digest"`
	// PlatformRequestID is the platform-assigned execution identity when the
	// runner's platform issues one (a Lambda invocation request id).
	PlatformRequestID string `json:"platform_request_id,omitempty"`
	MemoryMiB         *int   `json:"memory_mib,omitempty"`
}

// Changes describes workspace changes. A runner that cannot compare the
// workspace reports Complete false — it does not omit the field.
type Changes struct {
	Complete        bool     `json:"complete"`
	Paths           []string `json:"paths,omitempty"`
	SnapshotDigest  string   `json:"snapshot_digest,omitempty"`
	DiffArtifactRef string   `json:"diff_artifact_ref,omitempty"`
	Truncated       *bool    `json:"truncated,omitempty"`
}

// Artifacts references stored artifacts by ref.
type Artifacts struct {
	StdoutRef          string `json:"stdout_ref,omitempty"`
	StderrRef          string `json:"stderr_ref,omitempty"`
	OutputWorkspaceRef string `json:"output_workspace_ref,omitempty"`
	// ResultPayloadRef is set when the result payload exceeded the
	// transport limit and was stored out of band.
	ResultPayloadRef string `json:"result_payload_ref,omitempty"`
	// Additional carries runner-specific artifact refs. The schema permits
	// them (additionalProperties: string), so the mirror keeps them rather
	// than dropping refs a caller may need to fetch.
	Additional map[string]string `json:"-"`
}

// artifactFields are the schema's named artifact keys, in declaration order.
var artifactFields = []string{"stdout_ref", "stderr_ref", "output_workspace_ref", "result_payload_ref"}

// MarshalJSON flattens Additional alongside the named refs.
func (a Artifacts) MarshalJSON() ([]byte, error) {
	out := make(map[string]string, len(a.Additional)+len(artifactFields))
	for k, v := range a.Additional {
		if v != "" {
			out[k] = v
		}
	}
	for key, value := range map[string]string{
		"stdout_ref":           a.StdoutRef,
		"stderr_ref":           a.StderrRef,
		"output_workspace_ref": a.OutputWorkspaceRef,
		"result_payload_ref":   a.ResultPayloadRef,
	} {
		if value != "" {
			out[key] = value
		}
	}
	return json.Marshal(out)
}

// UnmarshalJSON splits the named refs out and keeps the rest in Additional.
func (a *Artifacts) UnmarshalJSON(data []byte) error {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("runners: decode artifacts: %w", err)
	}
	*a = Artifacts{
		StdoutRef:          raw["stdout_ref"],
		StderrRef:          raw["stderr_ref"],
		OutputWorkspaceRef: raw["output_workspace_ref"],
		ResultPayloadRef:   raw["result_payload_ref"],
	}
	for _, named := range artifactFields {
		delete(raw, named)
	}
	if len(raw) > 0 {
		a.Additional = raw
	}
	return nil
}

// Empty reports whether the artifact block references nothing at all.
func (a Artifacts) Empty() bool {
	return a.StdoutRef == "" && a.StderrRef == "" &&
		a.OutputWorkspaceRef == "" && a.ResultPayloadRef == "" && len(a.Additional) == 0
}

// ResourceUsage holds directly observed resource measurements. Report only
// what was measured; completeness is declared in Observations.
type ResourceUsage struct {
	MaxMemoryMiB *float64 `json:"max_memory_mib,omitempty"`
	CPUSeconds   *float64 `json:"cpu_seconds,omitempty"`
	// Additional carries runner-specific measurements (the schema sets
	// additionalProperties: true here).
	Additional map[string]any `json:"-"`
}

// MarshalJSON flattens Additional alongside the named measurements.
func (u ResourceUsage) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(u.Additional)+2)
	for k, v := range u.Additional {
		out[k] = v
	}
	if u.MaxMemoryMiB != nil {
		out["max_memory_mib"] = *u.MaxMemoryMiB
	}
	if u.CPUSeconds != nil {
		out["cpu_seconds"] = *u.CPUSeconds
	}
	return json.Marshal(out)
}

// UnmarshalJSON splits the named measurements out and keeps the rest.
func (u *ResourceUsage) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("runners: decode resource usage: %w", err)
	}
	*u = ResourceUsage{}
	if v, ok := raw["max_memory_mib"].(float64); ok {
		u.MaxMemoryMiB = &v
	}
	if v, ok := raw["cpu_seconds"].(float64); ok {
		u.CPUSeconds = &v
	}
	delete(raw, "max_memory_mib")
	delete(raw, "cpu_seconds")
	if len(raw) > 0 {
		u.Additional = raw
	}
	return nil
}

// Observation is one attributed observation. Measured says the runner itself
// determined the fact; Complete says the determination covers the whole
// declared scope. Both false is a legitimate, honest answer — and for several
// facts on a managed-function platform it is the only honest answer.
type Observation struct {
	Measured bool   `json:"measured"`
	Complete bool   `json:"complete"`
	Method   string `json:"method,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Note     string `json:"note,omitempty"`
}

// The four observation keys the schema requires of every result.
const (
	ObsExitStatus    = "exit_status"
	ObsChangedPaths  = "changed_paths"
	ObsLogs          = "logs"
	ObsResourceUsage = "resource_usage"
)

// requiredObservations are the keys every result must carry.
var requiredObservations = []string{ObsExitStatus, ObsChangedPaths, ObsLogs, ObsResourceUsage}

// Observations is the per-observation honesty block. The four named fields
// are required by the schema; Additional carries runner-specific
// observations, which the schema explicitly permits.
type Observations struct {
	ExitStatus    Observation            `json:"exit_status"`
	ChangedPaths  Observation            `json:"changed_paths"`
	Logs          Observation            `json:"logs"`
	ResourceUsage Observation            `json:"resource_usage"`
	Additional    map[string]Observation `json:"-"`
}

// MarshalJSON flattens Additional alongside the four required observations.
// A runner-specific key that collides with a required one loses: the required
// four are the schema's, not an adapter's to redefine.
func (o Observations) MarshalJSON() ([]byte, error) {
	out := make(map[string]Observation, len(o.Additional)+len(requiredObservations))
	for k, v := range o.Additional {
		out[k] = v
	}
	out[ObsExitStatus] = o.ExitStatus
	out[ObsChangedPaths] = o.ChangedPaths
	out[ObsLogs] = o.Logs
	out[ObsResourceUsage] = o.ResourceUsage
	return json.Marshal(out)
}

// UnmarshalJSON splits the required four out and keeps the rest.
func (o *Observations) UnmarshalJSON(data []byte) error {
	var raw map[string]Observation
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("runners: decode observations: %w", err)
	}
	*o = Observations{
		ExitStatus:    raw[ObsExitStatus],
		ChangedPaths:  raw[ObsChangedPaths],
		Logs:          raw[ObsLogs],
		ResourceUsage: raw[ObsResourceUsage],
	}
	for _, name := range requiredObservations {
		delete(raw, name)
	}
	if len(raw) > 0 {
		o.Additional = raw
	}
	return nil
}

// Get returns the observation stored under name, and whether it exists.
func (o Observations) Get(name string) (Observation, bool) {
	switch name {
	case ObsExitStatus:
		return o.ExitStatus, true
	case ObsChangedPaths:
		return o.ChangedPaths, true
	case ObsLogs:
		return o.Logs, true
	case ObsResourceUsage:
		return o.ResourceUsage, true
	}
	obs, ok := o.Additional[name]
	return obs, ok
}

// Names returns every observation key present, sorted, so a caller iterating
// them (an evidence builder, a report) gets a stable order.
func (o Observations) Names() []string {
	names := append([]string(nil), requiredObservations...)
	for name := range o.Additional {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ErrorKind classifies why a result is not completed. PRD §13.5: only
// explicitly retryable categories are retried automatically.
type ErrorKind string

// The error kinds the schema admits.
const (
	ErrorRetryableTransport ErrorKind = "retryable_transport"
	ErrorRateLimited        ErrorKind = "rate_limited"
	ErrorRunnerUnavailable  ErrorKind = "runner_unavailable"
	ErrorRejectedInput      ErrorKind = "rejected_input"
	ErrorAuthOrPolicy       ErrorKind = "auth_or_policy"
	ErrorContractFailure    ErrorKind = "contract_failure"
	ErrorExecutionFailure   ErrorKind = "execution_failure"
	ErrorTimeout            ErrorKind = "timeout"
	ErrorCancellation       ErrorKind = "cancellation"
)

// Retryable reports whether a kind is one the runtime may retry on its own.
// It is deliberately a small closed set: an unknown kind is not retryable,
// because "we do not know what went wrong" is not a reason to do it again.
func (k ErrorKind) Retryable() bool {
	switch k {
	case ErrorRetryableTransport, ErrorRateLimited, ErrorRunnerUnavailable:
		return true
	default:
		return false
	}
}

// ResultError explains a non-completed result. Retryable is the adapter's
// declaration, carried on the record rather than re-derived by every reader.
type ResultError struct {
	Kind      ErrorKind `json:"kind"`
	Retryable bool      `json:"retryable"`
	Message   string    `json:"message,omitempty"`
}
