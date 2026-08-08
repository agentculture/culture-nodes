// Package lambda dispatches runner operations to AWS Lambda container-image
// functions, pinned by ECR digest and enumerated in a function registry.
//
// It is the first code-runner adapter (spec claim c25). PRD §13.7 names
// headspace-cli as the reference runner pattern and §19.2 explicitly allows a
// non-Docker adapter that implements the same typed operation and evidence
// contract — this is that adapter, and the deviation from §13.7's
// headspace-first ordering is recorded, not silent. headspace-cli remains the
// reference pattern and a later local adapter.
//
// This package and internal/queue/sqs and internal/artifacts/s3 are the only
// places an AWS SDK may be imported (spec claim c17, enforced by task t17's
// depguard rule). Nothing above this boundary knows Lambda exists.
//
// # Trust boundary
//
// Lambda provides isolation and a small set of platform facts. It does not
// provide truth, and it observes considerably less than a Docker runner with
// a controlled workspace does. This adapter's central job is therefore not
// invocation — that is fifteen lines — but refusing to claim more than Lambda
// actually told it.
//
// What Lambda observes, and what this adapter therefore claims as measured:
//
//	Result field                          Source                              Observation
//	------------------------------------  ----------------------------------  --------------------------------------
//	environment.platform_request_id       Invoke response metadata            platform_request_id  measured, complete
//	environment.image_digest              GetFunction ResolvedImageUri,       image_digest         measured, complete
//	                                      at registry-load time, verified                          — only when the
//	                                      against the invoke's                                     executed version
//	                                      ExecutedVersion                                          matches
//	environment.memory_mib                GetFunction MemorySize              resource_usage       measured, partial
//	timing.duration_ms                    REPORT line, else adapter clock     duration             measured, partial
//	timing.billed_duration_ms             REPORT line                         resource_usage       measured, partial
//	resource_usage.max_memory_mib         REPORT line                         resource_usage       measured, partial
//	state / error.kind                    FunctionError + payload             handler_completion   measured, complete
//	artifacts.*                           function payload                    (not an observation — function-reported)
//	exit.code, exit.signal                function payload                    exit_status          NOT measured
//	changes.*                             nothing                             changed_paths        NOT measured
//	                                                                          workspace_snapshot   NOT measured
//	(log tail)                            Invoke LogResult, last 4 KB         logs                 measured, partial
//
// Three of those deserve saying out loud:
//
//   - exit_status is {measured:false, complete:false}. Lambda observes
//     whether the *handler* returned or raised — that is the
//     handler_completion observation — not the exit status of any process the
//     handler started. The exit.code the result carries is what the function
//     reported about itself, which is process-reported content. The headspace
//     runner earns exit_status from a wait status; this adapter does not, and
//     saying otherwise would be the exact fabrication PRD §10.5 forbids.
//   - changed_paths and workspace_snapshot are always {measured:false,
//     complete:false}, and changes.complete is always false (spec claim c25:
//     "Lambda's unobservables (workspace snapshot/diff completeness) are
//     declared honestly, never fabricated"). Lambda has no view of a
//     workspace: the workspace arrives and leaves as S3 artifact refs, and
//     the platform never compares them. A future validator may derive a diff
//     from two stored workspace artifacts — that record would carry `derived`
//     authority and name its parser, which is a different claim from this
//     runner having watched the files change.
//   - logs is measured but never complete: LogResult is the last 4 KB. And
//     capturing a log says nothing about the truth of what was printed; a
//     line reading "all tests passed" remains a claim by the process.
//
// # Registry-pinned dispatch
//
// Execute resolves the operation's execution.image_ref against the
// runners.FunctionRegistry *before* building a request, and refuses an
// unregistered name with a typed error and zero network calls (spec claim
// c41). It then checks the operation's pinned digest against the registered
// one, and the registered one against what GetFunction reported for the
// deployed function at load time. The same registry renders the worker's IAM
// policy (runners.RenderWorkerIAMPolicy, ADR 0003), so code refusal and IAM
// refusal enumerate one list.
//
// # Policy the adapter can and cannot enforce
//
// The operation schema is explicit that a policy field a runner cannot
// enforce must be *rejected*, not silently ignored. On Lambda most limits are
// properties of the deployed function rather than per-invocation knobs, so
// "enforced" here means the deployed configuration is already at least as
// tight as the policy asks — verified at load time against GetFunction.
//
//   - timeout_seconds: enforced. Refused above 900s and above the function's
//     own configured timeout.
//   - memory_mib, disk_mib: enforced by comparison. Refused when the deployed
//     function is configured larger than the policy allows.
//   - cpu, pids: refused outright. Lambda derives CPU from memory and exposes
//     no per-invocation process limit.
//   - network: `none` and `egress-allowlist` require the function to be
//     VPC-attached, which the adapter verifies. Attachment is necessary and
//     not sufficient — what a subnet routes to is outside this adapter's
//     view, so it refuses without attachment and never claims to have
//     observed the posture.
//   - allowed_output_paths: must be empty. Lambda gives the adapter no
//     controlled workspace whose paths it could scope; outputs come back as
//     artifact refs.
//   - command.requires_shell: refused. That field exists so policy can reject
//     the operation, and this policy does.
//
// execution.user and execution.read_only_root are not refused and not
// enforced here: both are properties of the container image and the function
// deployment, and the schema admits them "when the runner can set one". This
// adapter cannot set either per invocation, and it claims no observation
// about them.
//
// # Platform limits
//
// The 15-minute maximum duration and the 6 MB synchronous payload limit are
// validation-time rules, not runtime surprises. The compiler already rejects
// an over-cap timeout; Execute checks it again against both the 900-second
// platform cap and the function's own configured timeout, because an adapter
// that cannot enforce a policy limit must refuse the operation rather than
// quietly run it under a different one. Oversize requests are refused with a
// remediation (pass S3 artifact refs); oversize responses are prevented by
// the function contract below rather than detected after the fact.
//
// # Function contract
//
// The payload sent to the function is the runner Operation document verbatim.
// The function is expected to return a small JSON object:
//
//	{
//	  "exit_code": 0,                  // required; null if the process did not exit normally
//	  "signal": null,                  // optional
//	  "artifacts": {                   // optional; artifact refs, never inline content
//	    "stdout_ref": "artifact://...",
//	    "stderr_ref": "artifact://...",
//	    "output_workspace_ref": "artifact://...",
//	    "result_payload_ref": "artifact://..."
//	  },
//	  "message": "..."                 // optional, diagnostic only
//	}
//
// Refs, not content. A function that inlines a large test report will hit
// Lambda's 6 MB response limit and fail the whole invocation; the contract is
// that anything of unbounded size is written to the artifact store and named
// here. Nothing in this payload is treated as an observation.
//
// # Testing
//
// The unit tests run against an in-process fake implementing Lambda's
// Invoke and GetFunction REST-JSON endpoints, with injectable responses
// (success with a REPORT log tail, FunctionError, throttle, timeout). No test
// in the default lane touches AWS.
//
// A real-AWS integration test lives in awslive_test.go behind the `awslive`
// build tag and is skipped unless NODES_TEST_LAMBDA_ARN and
// NODES_TEST_LAMBDA_IMAGE_DIGEST are set:
//
//	go test -tags awslive ./internal/runners/lambda/ \
//	    -run TestLive \
//	    NODES_TEST_LAMBDA_ARN=arn:aws:lambda:us-east-1:123456789012:function:nodes-runner \
//	    NODES_TEST_LAMBDA_IMAGE_DIGEST=sha256:...
//
// Which CI lane runs it is undecided (plan risk r1: LocalStack's fidelity for
// digest-pinned container functions and IAM is unverified, and a dedicated
// AWS test account has not been provisioned). The tag exists so the test can
// be written and run by hand today without pretending a lane exists.
package lambda
