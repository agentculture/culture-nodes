package headspace

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// This file maps a parsed headspace-cli resultPackage onto a schema-valid
// runners.Result. See doc.go for the field-by-field evidence table this
// implements.

// buildResult assembles the Result for one run's resultPackage. The
// headspace-cli profile the operation ran under is not a parameter here: it
// is already reflected in pkg.Provenance.Profile and ImageDigest, both
// reported by headspace-cli itself, so a caller that needs it reads the
// result package rather than being handed a second, redundant copy.
func buildResult(op runners.Operation, pkg *resultPackage, started, finished time.Time, revision string) (runners.Result, error) {
	policyDigest, err := contracts.DigestValue(op.Policy)
	if err != nil {
		return runners.Result{}, fmt.Errorf("runners/headspace: digest policy: %w", err)
	}

	result := runners.Result{
		OperationID: op.OperationID,
		Timing:      buildTiming(pkg, started, finished),
		Environment: buildEnvironment(op, pkg, revision, policyDigest),
		// c12, restated for headspace: headspace-cli's result package has no
		// snapshot/diff section in any verb (create/put/run/export/destroy
		// all describe lifecycle and storage bytes, never a changed-path
		// list), so workspace changes are always incomplete -- same honesty
		// as the Lambda adapter's identical gap, reached for a different
		// reason (see buildObservations' ChangedPaths note).
		Changes: runners.Changes{Complete: false},
	}

	state, exit := interpretPackage(pkg)
	result.State = state
	result.Exit = exit

	if pkg.ResourceUsage.MaxMemoryBytes > 0 {
		mib := float64(pkg.ResourceUsage.MaxMemoryBytes) / (1024 * 1024)
		usage := &runners.ResourceUsage{MaxMemoryMiB: &mib}
		if pkg.ResourceUsage.CPUSeconds > 0 {
			cpu := pkg.ResourceUsage.CPUSeconds
			usage.CPUSeconds = &cpu
		}
		result.ResourceUsage = usage
	}

	result.Observations = buildObservations(pkg)
	return result, nil
}

// exitStatusPattern matches headspace-cli's fixed "exit status N" sentence
// (headspace/core/workspace.py's _job_findings), which is the only place in
// headspace-cli 0.11.0's result package the container command's own exit
// code appears -- there is no dedicated JSON field for it.
var exitStatusPattern = regexp.MustCompile(`exit status (\d+)`)

// parseExitStatus scans key_findings for the "exit status N" sentence.
func parseExitStatus(findings []string) (int, bool) {
	for _, line := range findings {
		if m := exitStatusPattern.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// interpretPackage maps a run's resultPackage status onto a runners.State
// and, when one was produced, an Exit. Only success/failure/timeout/
// cancelled ever reach here: resource_exhausted is intercepted earlier, in
// classifyVerbExit, and returned as a DispatchError before a Result is ever
// built (see that function's doc comment for why).
func interpretPackage(pkg *resultPackage) (runners.State, *runners.Exit) {
	switch pkg.Status {
	case statusSuccess, statusFailure:
		code, ok := parseExitStatus(pkg.KeyFindings)
		if !ok {
			// headspace-cli reported success/failure but this bridge could not
			// find the "exit status N" sentence it parses that from. Reporting
			// completed with no exit would silently drop information;
			// inventing a 0 would fabricate a value headspace-cli never gave
			// us. Neither is acceptable, so this is surfaced as failed with no
			// exit -- visibly incomplete, never silently wrong.
			return runners.StateFailed, nil
		}
		return runners.StateCompleted, &runners.Exit{Code: &code}
	case statusTimeout:
		return runners.StateTimedOut, nil
	case statusCancelled:
		return runners.StateCancelled, nil
	default:
		return runners.StateFailed, nil
	}
}

// buildTiming prefers headspace-cli's own reported timestamps and measured
// wall-clock duration -- it is the thing that actually watched the job run --
// and falls back to this bridge's own launch/reap clock only when
// headspace-cli's timestamps do not parse.
func buildTiming(pkg *resultPackage, fallbackStarted, fallbackFinished time.Time) runners.Timing {
	started, sOK := parseHeadspaceTime(pkg.Provenance.StartedAt)
	finished, fOK := parseHeadspaceTime(pkg.Provenance.FinishedAt)
	if !sOK {
		started = fallbackStarted
	}
	if !fOK {
		finished = fallbackFinished
	}

	durationMs := int(finished.Sub(started).Milliseconds())
	if pkg.ResourceUsage.WallTimeSeconds > 0 {
		durationMs = int(pkg.ResourceUsage.WallTimeSeconds * 1000)
	}

	return runners.Timing{StartedAt: started, FinishedAt: finished, DurationMs: durationMs}
}

func parseHeadspaceTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// buildEnvironment carries the digests that make this run replayable.
// image_digest and platform_request_id (job_id) are headspace-cli's own
// reported facts (task t18: "headspace's provenance ... measured with
// {measured:true}"); memory_mib is echoed only when the operation itself
// requested a ceiling, because that is a config value this bridge knows for
// certain rather than one it would have to parse out of headspace-cli's
// free-text policy_summary.
func buildEnvironment(op runners.Operation, pkg *resultPackage, revision, policyDigest string) runners.Environment {
	env := runners.Environment{
		RunnerRevision:    revision,
		ImageDigest:       pkg.Provenance.ImageDigest,
		PolicyDigest:      policyDigest,
		PlatformRequestID: pkg.Provenance.JobID,
	}
	if op.Workspace != nil {
		digest := op.Workspace.SourceDigest
		env.InputDigest = &digest
	}
	if op.Policy.MemoryMiB != nil {
		mib := *op.Policy.MemoryMiB
		env.MemoryMiB = &mib
	}
	return env
}

// runExcerpt returns the "captured output" evidence entry headspace-cli's run
// result carries, if any.
func runExcerpt(pkg *resultPackage) (excerpt string, truncated bool, ok bool) {
	for _, e := range pkg.Evidence {
		if e.Label == "captured output" {
			return e.Excerpt, e.Truncated, true
		}
	}
	return "", false, false
}

// buildObservations is the honesty block: per PRD c12 / c25 and this
// package's doc.go table, every observation states plainly whether this
// bridge -- not headspace-cli, this bridge -- directly measured the fact,
// and whether that measurement is complete. Where headspace-cli itself
// measured something and reported it in its own result package, this bridge
// relays that as measured: headspace-cli is the runner that actually
// watched the container; this bridge parses its report rather than watching
// the container itself (task t18: "note that headspace itself is the
// measuring runner and cite its result package as the source").
func buildObservations(pkg *resultPackage) runners.Observations {
	obs := runners.Observations{Additional: map[string]runners.Observation{}}

	if code, ok := parseExitStatus(pkg.KeyFindings); ok {
		obs.ExitStatus = runners.Observation{
			Measured: true,
			Complete: true,
			Method:   "headspace_run_key_findings",
			Scope: fmt.Sprintf(
				"The container command's exit status (%d), as headspace-cli's own provider observed it and reported in the run result's key_findings.",
				code),
			Note: "headspace-cli 0.11.0 has no dedicated exit-code field in its result package; this bridge extracts the " +
				"number from the fixed \"exit status N\" sentence headspace-cli always writes in key_findings[0]. " +
				"headspace-cli is the thing that actually watched the container end; this bridge parses its own report.",
		}
	} else {
		obs.ExitStatus = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "headspace_run_key_findings",
			Scope:    "None.",
			Note: fmt.Sprintf(
				"headspace-cli reported status %q with no \"exit status N\" sentence in key_findings, which is what it "+
					"does when a job was stopped or timed out before producing one -- there is no exit status to report.",
				pkg.Status),
		}
	}

	obs.ChangedPaths = runners.Observation{
		Measured: false,
		Complete: false,
		Method:   "unavailable_in_headspace_cli_0.11",
		Scope:    "None.",
		Note: "headspace-cli's result package has no snapshot/diff section in any verb (spec claim c12, restated for " +
			"headspace as it was for the Lambda adapter's changed_paths gap). A later deterministic validator could " +
			"derive a diff from two exported workspace artifacts; that record would carry `derived` authority and name " +
			"its parser, which is a different claim from this bridge having watched files change inside the workspace.",
	}

	if _, truncated, ok := runExcerpt(pkg); ok {
		obs.Logs = runners.Observation{
			Measured: true,
			Complete: !truncated,
			Method:   "headspace_run_evidence_excerpt",
			Scope: "The captured-output excerpt in the run result's evidence[0], bounded (and, if oversized, truncated) " +
				"under headspace-cli's own context-return byte budget.",
			Note: "This bridge destroys the workspace as its last step (see the workspace_cleanup observation), so " +
				"headspace-cli's own `inspect --logs` retrieval path for the untruncated capture no longer resolves once " +
				"Execute returns: this excerpt is the only capture that survives the call. The excerpt text itself is not " +
				"duplicated into this observation -- it is process-reported content headspace-cli already carries in " +
				"evidence[0], not a second copy this bridge would need to keep in sync.",
		}
	} else {
		obs.Logs = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "headspace_run_evidence_excerpt",
			Scope:    "None.",
			Note:     "The run result carried no captured-output evidence entry.",
		}
	}

	if pkg.ResourceUsage.MaxMemoryBytes > 0 || pkg.ResourceUsage.CPUSeconds > 0 || pkg.ResourceUsage.WallTimeSeconds > 0 {
		basis := pkg.ResourceUsage.MaxMemoryBasis
		if basis == "" {
			basis = "sampled"
		}
		obs.ResourceUsage = runners.Observation{
			Measured: true,
			Complete: false,
			Method:   "headspace_run_resource_usage",
			Scope:    "Wall time, CPU time, and " + basis + " memory, as headspace-cli's provider measured them for this job.",
			Note: "Peak memory is sampled, not accounted, so it is a lower bound on the true peak -- worst exactly when " +
				"a job is killed for exceeding its own memory ceiling, which is why headspace-cli itself relabels this " +
				"figure a \"sampled floor\" rather than a peak on a resource_exhausted result.",
		}
	} else {
		obs.ResourceUsage = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "headspace_run_resource_usage",
			Scope:    "None.",
			Note:     "The run result carried no nonzero resource_usage figures.",
		}
	}

	obs.Additional["workspace_id"] = measuredProvenance(pkg.Provenance.WorkspaceID,
		"The workspace id headspace-cli minted for this operation: "+pkg.Provenance.WorkspaceID+".")
	obs.Additional["job_id"] = measuredProvenance(pkg.Provenance.JobID,
		"The job id headspace-cli assigned to this run: "+pkg.Provenance.JobID+".")
	// The same measured fact under the canonical, runner-neutral key
	// internal/runners' evidence builder reads (BuildCompletion looks for
	// "platform_request_id" to decide whether Environment.PlatformRequestID
	// may enter an observed evidence record). buildEnvironment already sets
	// that field FROM the job id; declaring it only as "job_id" meant a fact
	// headspace genuinely measured was silently dropped from every evidence
	// record it produced. Both keys are kept: "job_id" is what headspace-cli
	// calls it, and a reader looking for headspace's own vocabulary should
	// still find it.
	obs.Additional["platform_request_id"] = measuredProvenance(pkg.Provenance.JobID,
		"The platform execution identity for this operation, which for headspace-cli is the job id: "+pkg.Provenance.JobID+".")
	obs.Additional["image_digest"] = measuredProvenance(pkg.Provenance.ImageDigest,
		"The digest-pinned image headspace-cli's profile registry resolved and ran: "+pkg.Provenance.ImageDigest+".")
	obs.Additional["policy_summary"] = measuredProvenance(pkg.Provenance.PolicySummary,
		"headspace-cli's own free-text summary of the effective policy it enforced for this workspace: \""+pkg.Provenance.PolicySummary+"\".")

	return obs
}

// measuredProvenance builds the Additional observation for one of
// headspace-cli's own directly-reported provenance fields.
func measuredProvenance(value, scope string) runners.Observation {
	if value == "" {
		return runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "headspace_provenance",
			Scope:    "None.",
			Note:     "headspace-cli's result package carried no value for this field.",
		}
	}
	return runners.Observation{Measured: true, Complete: true, Method: "headspace_provenance", Scope: scope}
}
