package compiler

import "sort"

// Level separates a compilation-stopping verdict from an advisory one. There
// are deliberately only two: a diagnostic either blocks publication or it does
// not, and a third "info" tier would invite diagnostics nobody has to act on.
type Level string

const (
	// LevelError blocks compilation: no IR and no digest are produced.
	LevelError Level = "error"
	// LevelWarning is advisory: the workflow still compiles.
	LevelWarning Level = "warning"
)

// Diagnostic is one finding about a workflow document. Path is a JSON Pointer
// into the *submitted* document (its JSON form), so a caller can point at the
// exact value that caused the finding even when the source was YAML.
//
// Code is a stable machine identifier of the form "<level-name>.<what>", where
// the prefix names the §11.4 validation level that produced it. Callers match
// on Code; Message and Hint are prose and may be reworded.
type Diagnostic struct {
	Level   Level  `json:"level"`
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// Diagnostic codes, grouped by the validation level that emits them (PRD
// §11.4). The "deployment" level — are pinned components resolvable in the
// target environment — has no codes here: it needs a component registry, which
// does not exist yet.
const (
	// Syntax.
	CodeSyntaxParse       = "syntax.parse"
	CodeSyntaxNotAnObject = "syntax.not_an_object"

	// Structure.
	CodeStructureSchema = "structure.schema"
	CodeStructureDecode = "structure.decode"

	// Graph.
	CodeGraphEntryUnknown      = "graph.entry_unknown"
	CodeGraphEdgeSourceUnknown = "graph.edge_source_unknown"
	CodeGraphEdgeTargetUnknown = "graph.edge_target_unknown"
	CodeGraphEdgeFromEndNode   = "graph.edge_from_end_node"
	CodeGraphOutcomeUndeclared = "graph.outcome_undeclared"
	CodeGraphNodeUnreachable   = "graph.node_unreachable"
	CodeGraphNoEndReachable    = "graph.no_end_reachable"
	CodeGraphUnboundedCycle    = "graph.unbounded_cycle"

	// Contract.
	CodeContractOutcomesMissing    = "contract.outcomes_missing"
	CodeContractSelectMissing      = "contract.select_missing"
	CodeContractUntilMissing       = "contract.until_missing"
	CodeContractSchemaInvalid      = "contract.schema_invalid"
	CodeContractBindingMalformed   = "contract.binding_malformed"
	CodeContractBindingUnresolved  = "contract.binding_unresolved"
	CodeContractBindingNodeUnknown = "contract.binding_node_unknown"
	CodeContractCELInvalid         = "contract.cel_invalid"
	CodeContractCELNotBoolean      = "contract.cel_not_boolean"

	// Ledger.
	CodeLedgerProjectionUnknown   = "ledger.projection_unknown"
	CodeLedgerRecordTypeUnknown   = "ledger.record_type_unknown"
	CodeLedgerObserveNotPermitted = "ledger.observe_not_permitted"
	CodeLedgerAcceptanceUnknown   = "ledger.acceptance_kind_unknown"

	// Policy.
	CodePolicyDurationInvalid   = "policy.duration_invalid"
	CodePolicyRetryExcessive    = "policy.retry_excessive"
	CodePolicyTimeoutOverCap    = "policy.timeout_exceeds_runner_cap"
	CodePolicyPayloadOverCap    = "policy.payload_exceeds_runner_cap"
	CodePolicyComponentUnpinned = "policy.component_unpinned"
	CodePolicyImageUnpinned     = "policy.image_unpinned"
	CodePolicyShellRequested    = "policy.shell_requested"

	// Hooks (task t14, spec claim c37).
	CodeHookKindNotAgent      = "hook.kind_not_agent"
	CodeHookOutcomeUndeclared = "hook.on_failure_outcome_undeclared"

	// Owners.
	CodeOwnersUnresolved = "owners.unresolved"
)

// HasErrors reports whether any diagnostic blocks compilation.
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Level == LevelError {
			return true
		}
	}
	return false
}

// CountByLevel returns how many diagnostics are errors and how many warnings.
func CountByLevel(diags []Diagnostic) (errors, warnings int) {
	for _, d := range diags {
		if d.Level == LevelError {
			errors++
			continue
		}
		warnings++
	}
	return errors, warnings
}

// sortDiagnostics puts diagnostics in the documented deterministic order: by
// path, then by code, then by message. Two compilations of the same bytes must
// produce the same sequence, or "assert the diagnostics" is not a contract a
// test or an agent can rely on.
func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		if diags[i].Code != diags[j].Code {
			return diags[i].Code < diags[j].Code
		}
		return diags[i].Message < diags[j].Message
	})
}

// dedupeDiagnostics collapses byte-identical findings. Two levels legitimately
// reporting the same path with different codes are kept: they are different
// statements about the same value.
func dedupeDiagnostics(diags []Diagnostic) []Diagnostic {
	seen := make(map[Diagnostic]bool, len(diags))
	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
