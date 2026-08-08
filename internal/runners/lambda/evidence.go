package lambda

import (
	"fmt"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// Runner-specific observation keys this adapter adds alongside the schema's
// required four. Each one exists because a Lambda fact does not line up with
// a required key, and folding it into one that nearly fits would overstate
// what was measured.
const (
	// ObsHandlerCompletion is what Lambda genuinely observes about the run:
	// whether the handler returned or raised. It is not the exit status of
	// anything the handler started.
	ObsHandlerCompletion = "handler_completion"
	// ObsPlatformRequestID is the invocation identity Lambda assigned.
	ObsPlatformRequestID = "platform_request_id"
	// ObsImageDigest is the deployed container image digest, and whether the
	// adapter can still vouch that it describes what actually ran.
	ObsImageDigest = "image_digest"
	// ObsDuration separates a platform-reported duration from the adapter's
	// own round-trip measurement.
	ObsDuration = "duration"
	// ObsWorkspaceSnapshot answers the operation's snapshot_before /
	// snapshot_after evidence requests, which this platform cannot honour.
	ObsWorkspaceSnapshot = "workspace_snapshot"
)

// observations builds the honesty block for one invocation.
//
// The rule applied throughout: measured is true only when *Lambda* determined
// the fact and this adapter read it from the platform's own response;
// complete is true only when that determination covers the whole of what the
// observation's name promises. Where neither holds, the observation says so
// and the note explains why, because an unexplained {false,false} invites
// someone to assume it is a gap in the code rather than a property of the
// platform.
func (a *Adapter) observations(
	op runners.Operation,
	state functionState,
	rep report,
	logTail string,
	executedVersion string,
	requestID string,
) runners.Observations {
	obs := runners.Observations{Additional: map[string]runners.Observation{}}

	// --- exit status -----------------------------------------------------
	//
	// Always unmeasured. Lambda watches the handler, not the processes the
	// handler starts, so the exit code in the result is the function's report
	// about itself. The headspace runner earns this observation from a wait
	// status; this adapter does not.
	obs.ExitStatus = runners.Observation{
		Measured: false,
		Complete: false,
		Method:   "function_reported_payload",
		Scope:    "None. The exit code and signal in this result were reported by the executed function.",
		Note: "Lambda observes whether the handler returned or raised (see the handler_completion observation), " +
			"not the exit status of a process the handler started. Reporting this as measured would attribute a " +
			"process-reported value to the platform.",
	}

	// --- workspace -------------------------------------------------------
	//
	// c25, stated twice on purpose: once as changed_paths (the schema's
	// required key) and once as workspace_snapshot (the operation's actual
	// request). Both are always false. Lambda has no view of a workspace.
	obs.ChangedPaths = runners.Observation{
		Measured: false,
		Complete: false,
		Method:   "unavailable_on_managed_function_platform",
		Scope:    "None.",
		Note: "Lambda cannot observe a workspace (spec claim c25). Input and output workspaces cross this " +
			"boundary as S3 artifact refs and the platform never compares them, so no path list here would be a " +
			"measurement. A later deterministic validator may derive a diff from two stored workspace artifacts; " +
			"that record carries derived authority and names its parser, which is a different claim from this " +
			"runner having watched files change.",
	}
	obs.Additional[ObsWorkspaceSnapshot] = workspaceSnapshotObservation(op)

	// --- logs ------------------------------------------------------------
	if logTail != "" {
		obs.Logs = runners.Observation{
			Measured: true,
			Complete: false,
			Method:   "lambda_invoke_log_tail",
			Scope:    "The last 4 KB of the execution log, as returned by Invoke with LogType=Tail.",
			Note: "Bounded by Lambda at 4 KB, so the capture is partial by construction. Capturing output is " +
				"also not a claim about the truth of anything printed: a line reading \"all tests passed\" " +
				"remains a statement by the process.",
		}
	} else {
		note := "No log tail was returned, so nothing about the process's output was captured."
		if !op.Evidence.CaptureLogs {
			note = "The operation did not request log capture, so no log tail was requested and none was captured."
		}
		obs.Logs = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "lambda_invoke_log_tail",
			Scope:    "None.",
			Note:     note,
		}
	}

	// --- resource usage --------------------------------------------------
	//
	// The REPORT line is the only place Lambda states billed duration and
	// peak memory, and it only appears when a log tail was requested and the
	// 4 KB window still contained it. No REPORT means no measurement — never
	// an inferred one from the configured memory size.
	if rep.found() {
		obs.ResourceUsage = runners.Observation{
			Measured: true,
			Complete: false,
			Method:   "lambda_report_line",
			Scope:    "Billed duration and peak memory used for this invocation, as Lambda reported them in the execution log's REPORT line.",
			Note: "Partial: Lambda reports peak memory and billed time, not CPU time, per-process usage, or I/O. " +
				"Peak memory is the platform's own figure for the whole execution environment, which can include a " +
				"warm start's residue from an earlier invocation.",
		}
	} else {
		note := "No REPORT line was available, so billed duration and peak memory were not measured."
		if !op.Evidence.CaptureLogs {
			note = "Lambda reports billed duration and peak memory only in the execution log's REPORT line, and this " +
				"operation did not request log capture. Nothing was measured; the configured memory size is a " +
				"deployment setting, not a measurement of this run."
		}
		obs.ResourceUsage = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "lambda_report_line",
			Scope:    "None.",
			Note:     note,
		}
	}

	// --- handler completion ----------------------------------------------
	obs.Additional[ObsHandlerCompletion] = runners.Observation{
		Measured: true,
		Complete: true,
		Method:   "lambda_invoke_function_error",
		Scope: "Whether the Lambda handler returned normally or raised, as reported by the invoke response's " +
			"FunctionError field. This is the one execution fact the platform itself determines.",
	}

	// --- platform request id ---------------------------------------------
	if requestID != "" {
		obs.Additional[ObsPlatformRequestID] = runners.Observation{
			Measured: true,
			Complete: true,
			Method:   "lambda_invoke_response_metadata",
			Scope:    "The invocation identity Lambda assigned to this request.",
		}
	} else {
		obs.Additional[ObsPlatformRequestID] = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "lambda_invoke_response_metadata",
			Scope:    "None.",
			Note:     "The invoke response carried no request id, so this invocation has no platform-assigned identity to record.",
		}
	}

	// --- image digest ----------------------------------------------------
	obs.Additional[ObsImageDigest] = imageDigestObservation(state, executedVersion)

	// --- duration --------------------------------------------------------
	if rep.DurationMs != nil {
		obs.Additional[ObsDuration] = runners.Observation{
			Measured: true,
			Complete: true,
			Method:   "lambda_report_line",
			Scope:    "Execution duration as Lambda measured it, excluding request transport.",
		}
	} else {
		obs.Additional[ObsDuration] = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "adapter_round_trip_clock",
			Scope:    "None of the function's execution time.",
			Note: "Without a REPORT line the duration in this result is the adapter's own wall-clock round trip, " +
				"which includes request transport, SDK time, and any cold start. It is recorded so the result is " +
				"not empty, and it is not a measurement of how long the function ran.",
		}
	}

	return obs
}

// workspaceSnapshotObservation answers the operation's snapshot requests.
// The schema is explicit that a runner which cannot honour an evidence
// request says so here rather than fabricating the observation.
func workspaceSnapshotObservation(op runners.Operation) runners.Observation {
	requested := op.Evidence.SnapshotBefore || op.Evidence.SnapshotAfter
	note := "No workspace snapshot was requested, and this adapter could not have produced one: Lambda gives it no " +
		"controlled workspace to snapshot."
	if requested {
		note = fmt.Sprintf(
			"The operation requested snapshot_before=%t and snapshot_after=%t. This adapter honoured neither and did "+
				"not approximate them: Lambda gives it no controlled workspace to snapshot, so the request is "+
				"declined out loud rather than answered with an invented digest (spec claim c25).",
			op.Evidence.SnapshotBefore, op.Evidence.SnapshotAfter)
	}
	return runners.Observation{
		Measured: false,
		Complete: false,
		Method:   "unavailable_on_managed_function_platform",
		Scope:    "None.",
		Note:     note,
	}
}

// imageDigestObservation states whether the adapter can still vouch that the
// digest it recorded describes what actually ran.
//
// The digest is read once, from GetFunction at load time, and pinned. The
// invoke response names the version that executed. When those agree, the
// digest describes this execution. When they do not, the function was
// redeployed between load and invoke: the digest is still what the registry
// pins, but it is no longer an observation about this run, and the
// observation says exactly that instead of quietly keeping its credibility.
func imageDigestObservation(state functionState, executedVersion string) runners.Observation {
	switch {
	case state.version == "" || executedVersion == "":
		return runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "lambda_get_function_resolved_image",
			Scope:    "None.",
			Note: "The function version could not be compared between load time and this invocation, so the " +
				"recorded image digest cannot be attributed to this execution.",
		}
	case state.version != executedVersion:
		return runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "lambda_get_function_resolved_image",
			Scope:    "None.",
			Note: fmt.Sprintf(
				"The registry's digest was read from function version %s, but version %s executed: the function was "+
					"redeployed after this adapter loaded it. The digest in this result is what the registry pins, "+
					"not an observation about what ran. Reload the adapter.",
				state.version, executedVersion),
		}
	default:
		return runners.Observation{
			Measured: true,
			Complete: true,
			Method:   "lambda_get_function_resolved_image",
			Scope: fmt.Sprintf(
				"The ECR image digest Lambda resolved for function version %s, which is the version this invocation executed.",
				executedVersion),
		}
	}
}
