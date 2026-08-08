// Package headspace is the local dev-profile runner.Runner (task t18): it
// drives the real headspace-cli binary by subprocess (os/exec, never a
// shell) and maps its nine-section context-return result package onto a
// schema-valid runners.Result.
//
// PRD §13.7 names headspace-cli as *the* reference runner pattern, and
// internal/runners/lambda's own package doc records that its own delivery
// (task t13) came first as a deliberate, recorded deviation from that
// ordering -- this package is the one §13.7 actually describes. Facts below
// were verified live against headspace-cli 0.11.0 with the real Docker
// provider during this task's implementation, not read off documentation
// alone.
//
// # Subprocess is the only integration
//
// headspace-cli has no daemon and no HTTP surface: every call is a fresh
// process, one verb per process, reading nothing and writing nothing except
// argv, envp, stdout, stderr, its exit code, and $HEADSPACE_HOME's on-disk
// store. This bridge never imports a headspace-cli package -- there is none
// to import from Go -- and treats headspace-cli entirely as an external
// binary.
//
// # Verb flow
//
// Execute drives, at most, this sequence -- always ending in a destroy, even
// on a refusal or a timeout:
//
//	validate (no subprocess)
//	  -> create                     (workspace + policy budget)
//	  -> put        [op.Workspace != nil]   (stage source_ref into the workspace)
//	  -> run                        (the command; see "Cancellation" below)
//	  -> export     [per declared output path]  (pull declared artifacts out)
//	  -> destroy    (always; --force fallback on refusal -- see verbs.go)
//
// create, put, and export map straight onto Operation fields:
//
//	execution.image_digest  -> BridgeConfig.Profile[digest] -> create --profile
//	policy.{timeout,memory,cpu,pids,disk,network}  -> create budget flags
//	workspace.source_ref    -> put WORKSPACE HOST_PATH DESTINATION
//	command.argv             -> run WORKSPACE -- ARGV...
//	command.environment_refs -> run --env NAME (value in envp, never argv)
//	policy.allowed_output_paths -> run --declare NAME=PURPOSE, then export NAME
//
// # Exit-band -> outcome mapping
//
// headspace-cli's own exit codes are a frozen, additive band (its own
// cli/_errors.py: "extend downward-compatibly, never renumber an existing
// code"). What each one becomes here, verified live against the real CLI for
// every one of the nine codes:
//
//	code  category                stdout/stderr shape (verified live)      this bridge
//	----  ----------------------  ----------------------------------------  --------------------------------------
//	0     success                 resultPackage JSON on stdout              Result{State: completed, Exit.Code=0}
//	1     user_error              cliError JSON on stderr                   *DispatchError{Kind: ErrorRejectedInput}
//	2     environment_error       cliError JSON on stderr (same CliError    *DispatchError{Kind: ErrorRunnerUnavailable}
//	                              path as 1/3/7 in headspace-cli's source)
//	3     policy_denied           cliError JSON on stderr                   *DispatchError{Kind: ErrorAuthOrPolicy}
//	4     timeout                 resultPackage JSON on stdout,             Result{State: timed_out, Exit: nil}
//	                              status="timeout"
//	5     cancelled               resultPackage JSON on stdout,             Result{State: cancelled, Exit: nil}
//	                              status="cancelled"
//	6     computation_failed      resultPackage JSON on stdout,             Result{State: completed, Exit.Code=N}
//	                              status="failure"                          (a domain-mappable exit, not an engine
//	                                                                         failure -- task t18's own framing)
//	7     infrastructure_failure  cliError JSON on stderr (same path)       *DispatchError{Kind: ErrorRunnerUnavailable}
//	8     resource_exhausted      resultPackage JSON on stdout,             *DispatchError{Kind: ErrorExecutionFailure}
//	                              status="resource_exhausted" (verified     (see below: NOT a Result, despite headspace-
//	                              live: a real OOM kill produces a full     cli itself producing a full result package)
//	                              package, not a bare error)
//
// Codes 1, 2, 3, 7, and 8 are all typed DispatchError classes (task t18's own
// grouping); 0, 4, 5, and 6 become a Result. 8 is the one surprise: live
// testing shows headspace-cli DOES write a complete, well-formed result
// package for a resource_exhausted job (status, key findings naming the
// enforced ceiling, a sampled-floor memory reading, remediation in
// attention) -- exactly the same shape as 0/4/5/6. This bridge still returns
// it as a DispatchError rather than a Result, because runners.State has no
// ResourceExhausted member and collapsing it into StateFailed would erase
// the one distinction (killed for exceeding a declared ceiling, vs an
// ordinary nonzero exit) that made parsing this package worth doing in the
// first place. classifyVerbExit (process.go) still parses the package in
// this case, purely so the DispatchError's Detail can quote headspace-cli's
// own diagnosis instead of a bare "exit 8".
//
// # Evidence honesty (c12 / c25, headspace edition)
//
// Result field                Source                              Observation           honesty
// ---------------------------  ----------------------------------  --------------------  --------------------------
// environment.image_digest     provenance.image_digest              image_digest          measured, complete
// environment.platform_request_id  provenance.job_id                job_id                measured, complete
// (Additional observation)     provenance.workspace_id              workspace_id          measured, complete
// (Additional observation)     provenance.policy_summary            policy_summary        measured, complete
// timing.*                     provenance.started_at/finished_at,   duration              measured (headspace-cli's
//
//	resource_usage.wall_time_seconds                           own clock, not this
//	                                                           bridge's round trip)
//
// resource_usage.max_memory_mib  resource_usage.max_memory_bytes    resource_usage        measured, partial (sampled
//
//	peak/floor -- see below)
//
// exit.code                    parsed from key_findings[0]'s        exit_status           measured, complete --
//
//	"exit status N" sentence (the ONLY                        headspace-cli measured it;
//	place this exists in 0.11.0's JSON)                       this bridge parses its own
//	                                                           report of that measurement
//
// changes.*                    nothing                              changed_paths         NOT measured (c12: no
//
//	snapshot/diff verb exists)
//
// (captured output)            evidence[] "captured output" excerpt logs                   measured, partial (bounded
//
//	by headspace-cli's own byte
//	budget, and unrecoverable
//	after this bridge's own
//	destroy step -- see below)
//
// (declared artifacts)         export's artifacts[].reference       artifacts_export      measured, complete/partial
//
//	per export failure
//
// (cleanup)                    this bridge's own destroy call(s)    workspace_cleanup     measured, complete
//
// Three of those deserve saying out loud, the same way lambda/doc.go says
// its three:
//
//   - exit_status is reported measured:true even though headspace-cli 0.11.0
//     has no dedicated JSON field for it -- the number lives only in a fixed
//     English sentence in key_findings[0] ("the command completed with exit
//     status N"), which this bridge regex-matches. That is still an honest
//     measured:true: headspace-cli itself is the runner that watched the
//     container's wait status (confirmed against its own source,
//     core/workspace.py's _job_findings); this bridge is relaying headspace-
//     cli's own report of a fact IT measured, not fabricating one of its
//     own. What is NOT claimed is that this bridge watched the container
//     itself -- it did not; it read a subprocess's stdout.
//   - changed_paths is always {measured:false, complete:false}. Every
//     headspace-cli verb this bridge calls (create, put, run, export,
//     destroy, stop) was exercised live during this task, and none of them
//     report a path list or a workspace digest anywhere in their result
//     package -- the gap is not an oversight in this bridge, it is genuinely
//     absent from headspace-cli 0.11.0's surface, the same honesty the
//     Lambda adapter already applies for a different underlying reason (c25:
//     Lambda has no workspace at all; headspace-cli has one but no verb that
//     reports what changed inside it).
//   - logs is measured but never complete, for two compounding reasons: the
//     excerpt is bounded (and possibly truncated) by headspace-cli's own
//     8 KiB context-return budget, AND this bridge always destroys the
//     workspace as its last step (see "Cleanup" below), which means
//     headspace-cli's own retrieval path for the untruncated capture
//     (`headspace inspect <job_id> --logs`) no longer resolves once Execute
//     returns. The excerpt this bridge captured is the only capture that
//     survives the call, and the Logs observation's note says so.
//
// # Cancellation
//
// `run` blocks for the whole job and holds the workspace's flock the entire
// time (verified live: a second `run` against the same workspace would
// block on it). Killing that process on ctx cancellation would abandon the
// flock and the running container with nothing left to observe the outcome,
// so this bridge never does that. Instead, Execute launches `run` with plain
// exec.Command (deliberately not exec.CommandContext) and a separate
// goroutine watches ctx.Done(); if it fires while run is still blocked, the
// goroutine launches `headspace stop <ws> --apply` as its own process,
// sharing the same $HEADSPACE_HOME. Verified live end to end: the blocked
// run invocation observes the stop signal on its own and exits 5 with
// status "cancelled" in its own result package; the separate `stop --apply`
// process itself also exits 5. This bridge reaps run's exit through the same
// classifyVerbExit path as every other outcome -- cancellation is not a
// special case in the exit-band table, only in how the signal reaches run.
//
// # Secrets
//
// runners.Operation carries only environment_ref *names* -- there is no
// secrets-provider abstraction wired into runners.Runner today, and the
// Operation document is part of the replay manifest, so a value can never
// live in it. This bridge resolves each named ref from its OWN process
// environment (os.LookupEnv) and forwards ONLY the resolved value into the
// headspace run child's envp (see process.go's buildEnv) -- exactly
// mirroring how a real deployment injects secrets into a worker process
// (a Kubernetes Secret mounted as env vars) and this boundary forwards the
// named ones onward. The value is never placed in argv: `--env NAME` is the
// only flag this bridge ever emits for a secret, and headspace-cli itself
// reads the value back out of its own envp, which is this bridge's
// buildEnv output, not a command-line argument. Verified live: grepping the
// full run result JSON (provenance.inputs included) for an injected secret
// value finds nothing; the command that was handed the value in its own
// container environment can still read and print it, which is the point.
//
// # Cleanup
//
// Execute always attempts to destroy the workspace it created, in a deferred
// step that runs regardless of how run went -- success, a nonzero exit, a
// timeout, or a cancellation. It never uses the caller's ctx directly for
// this (ctx may be exactly why Execute is returning), instead deriving a
// bounded, cancellation-detached context (BridgeConfig.StopTimeout, default
// 30s). It tries a plain `destroy` first; headspace-cli refuses (exit 1)
// only when a declared artifact was never exported, which this bridge's own
// flow avoids by exporting every declared path before reaching here. If it
// is refused anyway, this bridge falls back to `destroy --force`, discarding
// whatever was never exported rather than leaking the workspace -- the
// export failure that caused the refusal is already recorded in the
// artifacts_export observation, so the forced destroy does not silently
// lose information on top of it. Either outcome is recorded on the returned
// Result as the workspace_cleanup observation.
//
// # Local artifact staging (a recorded t18 scope boundary)
//
// internal/artifacts defines a pod-agnostic "artifact://<namespace>/<id>"
// ref, resolved only through a Store (internal/artifacts/doc.go). This
// bridge does not hold or accept a Store: BridgeConfig has no such field,
// matching this task's own specified configuration surface. Workspace.
// SourceRef is instead treated as a path already present on the local
// filesystem this bridge's process runs on, staged directly with
// headspace-cli's `put`; exported declared artifacts come back as local
// filesystem paths under a per-Execute temp directory, recorded verbatim on
// Result.Artifacts.Additional -- not as artifact:// refs. A caller that hands
// this bridge an artifact:// ref for source_ref gets a clear, typed refusal
// (os.Stat fails on it) rather than a silent misinterpretation. Wiring an
// artifacts.Store resolution step in front of put, and a Store.Put call
// after export, is follow-up work for whichever task connects this bridge
// to the worker.
//
// # Testing
//
// bridge_test.go exercises the full exit-band table (including the
// cancellation goroutine/process-separation mechanism itself, exercised
// without Docker by having the fake run verb block until a fake stop verb
// signals it) against testdata/fakeheadspace, a canned-response shell script
// standing in for headspace-cli -- no Docker involved.
//
// live_test.go drives the real headspace-cli binary against the real Docker
// provider: happy path, a nonzero exit (band 6), a timeout (band 4), a
// cancellation (ctx cancel mid-run -> stop --apply -> band 5), a secret
// forwarded by name and never found in argv/provenance, and the destroy-
// refusal -> --force fallback. Each test skips (t.Skip) when `headspace
// doctor --json` fails or Docker is unavailable, but is not gated behind a
// build tag: on a machine with both present, `go test ./...` runs it for
// real.
package headspace
