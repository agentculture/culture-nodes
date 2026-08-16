package runners

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// This file is the worker seam. Task t12 is defining the actor-side
// RunnerDispatcher interface on a parallel branch; it has not landed, so
// rather than guess its shape this package defines the one thing the worker
// genuinely needs from a runner result and keeps it small enough to adapt:
// Result in, engine completion out.
//
// A later integration task wires BuildCompletion into the worker's
// CompleteAttempt call. Nothing here talks to the engine at runtime — it
// only produces the values engine.CompletionRequest wants — so that wiring is
// a constructor change, not a rewrite.

// NodeContract is the slice of a compiled node this seam reads. It is
// deliberately not engine.Node: the worker will hold a compiled node and can
// project one of these, and keeping the seam's input flat means a test can
// state a contract in four lines instead of building a workflow.
type NodeContract struct {
	// NodeID is the node whose attempt produced the result, for messages.
	NodeID string

	// SuccessOutcome is the domain outcome for an operation that ran and
	// exited zero. Required.
	SuccessOutcome string

	// FailureOutcome is the domain outcome for an operation that ran and
	// exited nonzero.
	//
	// This is PRD §3.4 in one field. A test suite that runs to completion
	// and reports failures produced a *domain* answer — the node ran fine,
	// the tests did not pass — and a workflow that declares an outcome for
	// it wants an edge followed, not a retry. Leaving this empty says the
	// opposite: for this node a nonzero exit is a technical failure with no
	// domain answer, and the engine's retry policy applies.
	FailureOutcome string

	// ExitCodeOutcomes maps individual exit codes onto declared domain
	// outcomes, and is consulted BEFORE the success/failure pair above.
	//
	// It exists because two ports are not always enough, and the merge gate
	// (task t16, issue #101) is the case that proves it. A gate's exit
	// status carries three domain answers — the gates passed, a threshold
	// was missed, or the measurement never happened — and the last two must
	// not share an edge: "we could not run the Go suite on this host" and
	// "the Go suite failed" call for entirely different next steps, and
	// routing the first as the second manufactures a defect nobody observed.
	//
	// A code NOT in the table falls through to the success/failure pair, so
	// a node that declares no table behaves exactly as it did before. A node
	// that declares one and exits outside it gets whatever FailureOutcome
	// says — normally nothing, i.e. a technical failure, which is the honest
	// answer for a tool that crashed: no trustworthy domain measurement was
	// produced, so there is no domain answer to route.
	ExitCodeOutcomes map[int]string

	// ActorID is the runner actor that produced the evidence. It is the
	// identity the ledger's producer matrix checks against the manifest.
	// Required for the evidence record; without it BuildCompletion emits no
	// ledger delta rather than an unattributed one.
	ActorID string

	// ActorRevision is the runner revision, recorded on the record's origin.
	ActorRevision string

	// RunID, NodeRunID, and AttemptID are optional. The engine stamps all
	// three unconditionally when it prepares a delta — a record cannot
	// attribute itself to a different attempt than the one that produced it
	// — so a caller normally leaves them empty. They exist for callers that
	// want a self-contained, schema-valid record before the engine sees it.
	RunID     string
	NodeRunID string
	AttemptID string

	// SubjectRef optionally points the evidence at the claim it is evidence
	// for.
	SubjectRef string
}

// Completion is what the worker needs to complete an attempt.
//
// The task brief sketched this as a bare (outcome, output, ledgerDelta)
// triple. It is a struct because the worker also needs the technical status —
// PRD §3.4's other half, which a bare outcome cannot express — and the
// manifest that authorizes the delta, which engine.CompletionRequest takes as
// a separate field. Returning them separately would force every caller to
// re-derive both from the Result, and "re-derive the authority rule at each
// call site" is how authority rules drift.
type Completion struct {
	// TechStatus is how the dispatch went (PRD §3.4).
	TechStatus engine.TechStatus
	// Outcome is the domain outcome, set only when TechStatus is succeeded.
	Outcome string
	// Output is the code node's output surface (see CodeNodeOutput).
	Output json.RawMessage
	// LedgerDelta is the observed evidence the runner earned the right to
	// write — and nothing else.
	LedgerDelta []ledger.Record
	// RunnerManifest declares exactly which fields of that delta the runner
	// directly measured. It is built alongside the record, field by field,
	// so the manifest cannot describe a payload the builder did not write.
	RunnerManifest *ledger.RunnerManifest
}

// CodeNodeOutput is the output surface a code node produces: the stable shape
// a node's outcome contract binds against.
//
// It is small on purpose. Lambda's synchronous response is capped at 6 MB, so
// a function's real output travels as an artifact the store holds and this
// document references — the alternative is a contract that works until the
// first large test report and then silently truncates.
type CodeNodeOutput struct {
	OperationID string `json:"operation_id"`
	State       State  `json:"state"`
	// ExitCode is null when the process did not exit normally.
	ExitCode *int `json:"exit_code"`
	// Artifacts are the refs the runner reported. They are function-reported
	// references, not observations: the artifact store verifies content
	// against the digest it recorded at Put time, which is where that trust
	// actually comes from.
	Artifacts map[string]string `json:"artifacts,omitempty"`
	// PlatformRequestID correlates the output with the platform's own
	// execution record.
	PlatformRequestID string `json:"platform_request_id,omitempty"`
	DurationMs        int    `json:"duration_ms"`
}

// BuildCompletion maps a runner Result onto an engine completion.
//
// The mapping rules, in order:
//
//   - completed + an exit code contract.ExitCodeOutcomes names → succeeded,
//     that outcome (task t16: a gate's three domain answers);
//   - completed + exit 0 → succeeded, contract.SuccessOutcome;
//   - completed + exit nonzero → succeeded with contract.FailureOutcome when
//     the node declares one (a domain answer, PRD §3.4), otherwise failed;
//   - completed + null exit → failed: the runner has no honest exit to route
//     on, and inventing zero would be the fabrication this whole package
//     exists to prevent;
//   - timed_out → timed_out, cancelled → cancelled;
//   - failed → failed;
//   - rejected → policy_denied or contract_rejected, by error kind.
//
// The ledger delta is a single observed evidence record built from the
// Result's observations: a measurement lands in the payload only when its
// observation says measured, and the returned manifest declares exactly the
// pointers the builder wrote. That is why the delta passes
// ledger.CheckAuthority by construction rather than by hope — and why an
// unmeasured field cannot be smuggled in by editing one side.
func BuildCompletion(res Result, contract NodeContract) (Completion, error) {
	if contract.SuccessOutcome == "" {
		return Completion{}, fmt.Errorf(
			"runners: node %q declares no success outcome; a code node needs one before a result can be routed", contract.NodeID)
	}

	completion := Completion{}
	completion.TechStatus, completion.Outcome = mapStatus(res, contract)

	output, err := buildOutput(res)
	if err != nil {
		return Completion{}, err
	}
	completion.Output = output

	if contract.ActorID != "" {
		record, manifest, err := buildEvidence(res, contract)
		if err != nil {
			return Completion{}, err
		}
		completion.LedgerDelta = []ledger.Record{record}
		completion.RunnerManifest = &manifest
	}

	return completion, nil
}

// mapStatus applies the technical-status/domain-outcome split.
func mapStatus(res Result, contract NodeContract) (engine.TechStatus, string) {
	switch res.State {
	case StateCompleted:
		code, ok := res.ExitCode()
		switch {
		case !ok:
			return engine.StatusFailed, ""
		case contract.ExitCodeOutcomes[code] != "":
			return engine.StatusSucceeded, contract.ExitCodeOutcomes[code]
		case code == 0:
			return engine.StatusSucceeded, contract.SuccessOutcome
		case contract.FailureOutcome != "":
			return engine.StatusSucceeded, contract.FailureOutcome
		default:
			return engine.StatusFailed, ""
		}
	case StateTimedOut:
		return engine.StatusTimedOut, ""
	case StateCancelled:
		return engine.StatusCancelled, ""
	case StateRejected:
		if res.Error != nil && res.Error.Kind == ErrorRejectedInput {
			return engine.StatusContractRejected, ""
		}
		return engine.StatusPolicyDenied, ""
	default:
		return engine.StatusFailed, ""
	}
}

// buildOutput renders the code-node output surface.
func buildOutput(res Result) (json.RawMessage, error) {
	out := CodeNodeOutput{
		OperationID:       res.OperationID,
		State:             res.State,
		PlatformRequestID: res.Environment.PlatformRequestID,
		DurationMs:        res.Timing.DurationMs,
	}
	if res.Exit != nil {
		out.ExitCode = res.Exit.Code
	}
	if res.Artifacts != nil && !res.Artifacts.Empty() {
		out.Artifacts = artifactMap(*res.Artifacts)
	}

	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("runners: encode code node output: %w", err)
	}
	return data, nil
}

func artifactMap(a Artifacts) map[string]string {
	out := make(map[string]string, len(a.Additional)+4)
	for k, v := range a.Additional {
		out[k] = v
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
	return out
}

// evidenceArtifactRefs collects every stored-content reference this result
// names that the evidence record should point at: the runner's own Artifacts
// block, plus the workspace diff artifact ref when (and only when) the
// changed_paths observation says the workspace comparison it came from was
// actually measured — an unmeasured diff artifact ref would assert a
// comparison the runner never made. Deduplicated and sorted so the payload
// is deterministic for the same Result.
func evidenceArtifactRefs(res Result) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	if res.Artifacts != nil {
		add(res.Artifacts.StdoutRef)
		add(res.Artifacts.StderrRef)
		add(res.Artifacts.OutputWorkspaceRef)
		add(res.Artifacts.ResultPayloadRef)
		for _, v := range res.Artifacts.Additional {
			add(v)
		}
	}
	if res.Observations.ChangedPaths.Measured {
		add(res.Changes.DiffArtifactRef)
	}
	sort.Strings(refs)
	return refs
}

// evidenceBuilder accumulates an evidence payload and, in lockstep, the
// manifest pointers that declare it. Every write goes through set, so there
// is no way to add a field without declaring it — the two lists are the same
// list.
type evidenceBuilder struct {
	data     map[string]any
	pointers []string
}

func newEvidenceBuilder() *evidenceBuilder {
	return &evidenceBuilder{data: map[string]any{}}
}

// set records a top-level field and declares its pointer.
func (b *evidenceBuilder) set(key string, value any) {
	b.data[key] = value
	b.pointers = append(b.pointers, "/"+key)
}

// measure records a field under `measurements` and declares that one
// pointer — never the whole `measurements` object, because declaring the
// parent would silently license every future sibling.
func (b *evidenceBuilder) measure(key string, value any) {
	measurements, ok := b.data["measurements"].(map[string]any)
	if !ok {
		measurements = map[string]any{}
		b.data["measurements"] = measurements
	}
	measurements[key] = value
	b.pointers = append(b.pointers, "/measurements/"+key)
}

// buildEvidence turns a Result into one observed evidence record plus the
// manifest that authorizes it.
//
// What goes in: the runner's self-description, the honesty declarations, and
// every measurement whose observation says measured — including, when the
// runner's own changed_paths observation says it directly compared the
// workspace (task t12, spec claim c15, honesty condition h10), the changed
// files, a diff digest, and artifact refs from that comparison. What stays
// out: anything the observations mark unmeasured — workspace changes on a
// platform that cannot see a workspace, a subprocess exit code the platform
// never watched, artifact refs the executed function asserted. Those are not
// omitted quietly: covered_scope names what the observation actually covers
// and completeness says `partial` when it is partial.
func buildEvidence(res Result, contract NodeContract) (ledger.Record, ledger.RunnerManifest, error) {
	b := newEvidenceBuilder()

	b.set("producer_id", contract.ActorID)
	if contract.ActorRevision != "" {
		b.set("producer_revision", contract.ActorRevision)
	}
	b.set("collection_method", collectionMethod(res))
	b.set("observed_at", res.Timing.FinishedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	b.set("covered_scope", coveredScope(res))
	b.set("completeness", completeness(res))
	b.set("operation_digest", res.Environment.PolicyDigest)

	// The image digest is evidence only when the adapter verified that the
	// version that executed is the version whose digest it read.
	if obs, ok := res.Observations.Get("image_digest"); ok && obs.Measured {
		b.set("environment_digest", res.Environment.ImageDigest)
	}
	if obs, ok := res.Observations.Get("platform_request_id"); ok && obs.Measured && res.Environment.PlatformRequestID != "" {
		b.measure("platform_request_id", res.Environment.PlatformRequestID)
	}
	if obs, ok := res.Observations.Get("handler_completion"); ok && obs.Measured {
		b.measure("handler_error", res.Error != nil)
	}
	if obs, ok := res.Observations.Get("duration"); ok && obs.Measured {
		b.measure("duration_ms", res.Timing.DurationMs)
	}
	if res.Observations.ResourceUsage.Measured {
		if res.Timing.BilledDurationMs != nil {
			b.measure("billed_duration_ms", *res.Timing.BilledDurationMs)
		}
		if res.ResourceUsage != nil && res.ResourceUsage.MaxMemoryMiB != nil {
			b.measure("max_memory_mib", *res.ResourceUsage.MaxMemoryMiB)
		}
		if res.Environment.MemoryMiB != nil {
			b.measure("configured_memory_mib", *res.Environment.MemoryMiB)
		}
	}
	// The exit code rides along only when something actually measured it.
	// On a managed-function platform nothing does; on the headspace runner
	// the wait status does.
	if res.Observations.ExitStatus.Measured {
		if code, ok := res.ExitCode(); ok {
			b.measure("exit_code", code)
		}
	}
	// Workspace-snapshot facts (task t12, spec claim c15, honesty condition
	// h10): changed_paths and snapshot_digest ride the same measured gate as
	// every other observation above — they appear only when the runner's own
	// changed_paths observation says it directly compared the workspace.
	// Neither headspace-cli 0.11.0 nor the Lambda adapter can honour that
	// comparison today (see their own package docs), so this is dormant on
	// both until a runner that can measure it exists; a hook operation is the
	// one place today's worker always requests it (buildHookOperation), so a
	// capable runner starts producing this evidence through this same seam
	// without any worker change.
	if res.Observations.ChangedPaths.Measured {
		if len(res.Changes.Paths) > 0 {
			b.set("changed_paths", res.Changes.Paths)
		}
		if res.Changes.SnapshotDigest != "" {
			b.set("snapshot_digest", res.Changes.SnapshotDigest)
		}
	}
	// artifact_refs is the evidence schema's own named field
	// (schemas/ledger/evidence.schema.json) for stored content this record
	// points at. It is not gated on changed_paths being measured the way the
	// diff artifact ref folded into it is: an artifact ref's trust comes from
	// the store's own content-addressing at Put time (buildOutput's own
	// comment makes the identical point about CodeNodeOutput.Artifacts), not
	// from a runner's honesty about a workspace comparison it may never have
	// made.
	if refs := evidenceArtifactRefs(res); len(refs) > 0 {
		b.set("artifact_refs", refs)
	}

	payload, err := json.Marshal(b.data)
	if err != nil {
		return ledger.Record{}, ledger.RunnerManifest{}, fmt.Errorf("runners: encode evidence payload: %w", err)
	}

	pointers := append([]string(nil), b.pointers...)
	sort.Strings(pointers)

	record := ledger.Record{
		RecordType: ledger.RecordEvidence,
		RunID:      contract.RunID,
		NodeRunID:  ledger.NullableID(contract.NodeRunID),
		AttemptID:  ledger.NullableID(contract.AttemptID),
		Origin: ledger.Origin{
			Kind:          ledger.OriginRunner,
			ActorID:       contract.ActorID,
			ActorRevision: contract.ActorRevision,
		},
		Authority:      ledger.AuthorityObserved,
		SubjectRef:     ledger.NullableID(contract.SubjectRef),
		Data:           payload,
		ProvenanceRefs: []string{},
	}

	manifest := ledger.RunnerManifest{ActorID: contract.ActorID, ObservableFields: pointers}
	return record, manifest, nil
}

// collectionMethod names how the observation was made, taken from the
// exit-status observation's method when the adapter set one.
func collectionMethod(res Result) string {
	if obs, ok := res.Observations.Get("handler_completion"); ok && obs.Method != "" {
		return obs.Method
	}
	if res.Observations.ExitStatus.Method != "" {
		return res.Observations.ExitStatus.Method
	}
	return "runner_result"
}

// coveredScope states what the evidence actually covers, naming the
// unmeasured observations rather than leaving their absence to be noticed.
func coveredScope(res Result) string {
	var unmeasured []string
	for _, name := range res.Observations.Names() {
		if obs, ok := res.Observations.Get(name); ok && !obs.Measured {
			unmeasured = append(unmeasured, name)
		}
	}
	if len(unmeasured) == 0 {
		return "Every observation this runner declared was directly measured."
	}
	return "Directly measured platform facts only. Not measured by this runner: " +
		joinAnd(unmeasured) + " — those values, where present in the result, are reported by the executed process, not observed."
}

// completeness collapses the per-observation flags into the evidence
// schema's three-valued field. `partial` and `unknown` are first-class
// answers; omitting the field is not a way to imply `complete`.
func completeness(res Result) string {
	measuredAny, completeAll := false, true
	for _, name := range res.Observations.Names() {
		obs, ok := res.Observations.Get(name)
		if !ok {
			continue
		}
		if obs.Measured {
			measuredAny = true
		}
		if !obs.Measured || !obs.Complete {
			completeAll = false
		}
	}
	switch {
	case !measuredAny:
		return "unknown"
	case completeAll:
		return "complete"
	default:
		return "partial"
	}
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	out := ""
	for i, item := range items {
		switch {
		case i == 0:
			out = item
		case i == len(items)-1:
			out += " and " + item
		default:
			out += ", " + item
		}
	}
	return out
}
