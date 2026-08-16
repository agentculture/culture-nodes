package runners

// Run identity inside the operation (task t16, issue #101).
//
// A code node has, until now, had no way to learn WHICH RUN it is executing
// for. `operation.argv` is pinned by the workflow digest, so it cannot carry a
// per-run value; `environment_refs` names values a deployment grants to the
// worker process, so those are per-worker, not per-run; and `workspace_ref`
// reaches the boundary opaque. The operation document has carried
// Operation.Context — the run, node run and attempt ids — since it was
// defined, but only as correlation metadata that never crossed into the
// executed process.
//
// The merge gate is the case where that gap stops being theoretical. A gate is
// a deterministic validator whose ENTIRE OUTPUT is a set of derived ledger
// records about a run (PRD §10.4), and a validator that cannot name its
// subject records nothing. So the boundary forwards the operation's own
// context as environment values, under reserved names.
//
// Three properties are deliberate:
//
//  1. The values come from the OPERATION the control plane composed, never
//     from anything the executed process said about itself. A container that
//     could choose its own run id would be able to write derived records
//     against someone else's run.
//  2. They are environment values, not argv. Same rule as every other value
//     that crosses this boundary (see headspace's doc.go): argv is the pinned,
//     digest-addressed half of an operation and must stay identical across
//     runs of the same published workflow.
//  3. They are omitted, not blanked, when the control plane did not set them.
//     A gate program reading an absent NODES_RUN_ID refuses; one reading an
//     empty string could mistake it for a run.

// Reserved environment names carrying an operation's own run identity into the
// executed process. They are reserved in the sense that a deployment must not
// grant values under these names through `environment_refs`: the boundary
// overwrites them with the operation's own context, which is the point.
const (
	EnvRunID       = "NODES_RUN_ID"
	EnvNodeRunID   = "NODES_NODE_RUN_ID"
	EnvAttemptID   = "NODES_ATTEMPT_ID"
	EnvOperationID = "NODES_OPERATION_ID"
)

// ContextEnvironment returns the environment values an adapter forwards into
// the executed process so it can name the run it is working for.
//
// Empty fields are omitted rather than set to "": absence is a fact a program
// can refuse on, where an empty string is a value that has to be re-checked at
// every use.
func ContextEnvironment(op Operation) map[string]string {
	out := map[string]string{}
	if op.OperationID != "" {
		out[EnvOperationID] = op.OperationID
	}
	if op.Context == nil {
		return out
	}
	for name, value := range map[string]string{
		EnvRunID:     op.Context.RunID,
		EnvNodeRunID: op.Context.NodeRunID,
		EnvAttemptID: op.Context.AttemptID,
	} {
		if value != "" {
			out[name] = value
		}
	}
	return out
}
