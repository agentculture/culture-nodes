# ADR 0003: Lambda runner IAM — registry-pinned, enumerated invoke

- Status: accepted
- Date: 2026-08-09
- Task: t13 (Runner boundary and the Lambda adapter: typed operations,
  IAM-scoped registry-pinned dispatch, honest evidence mapping)
- Spec claims: c41 (code-node dispatch is IAM-scoped and registry-pinned),
  c25 (code nodes execute on AWS Lambda for now), c17 (AWS code stays in
  AWS-specific packages)
- Honesty condition: h36 (dispatch to an unregistered function identity is
  refused, and the reference deploy's worker IAM policy enumerates registered
  function ARNs only — no wildcard invoke)

## Context

Code nodes execute on AWS Lambda container-image functions
(`docs/initial-design/culture-nodes-prd-spec.md` §19.2 allows a non-Docker
adapter that implements the same typed operation and evidence contract;
§16.4 requires the runner to be a separate security boundary from the generic
worker). The worker process therefore holds credentials that can invoke
Lambda.

That is the whole risk. A worker that can invoke *any* function in the
account is a worker that can be turned into an arbitrary code-execution
service by whatever can influence a workflow definition — an agent proposing
a node, a compromised authoring surface, a copy-paste error in a graph. PRD
§16.2's policy constraints and §9.10's pinning exist to stop exactly that,
and "the control plane can never invoke arbitrary Lambdas" (c41) is the
property this ADR fixes.

There are two places that property can be enforced, and both are needed:

- **in the process**, so a dispatch to an unregistered identity never becomes
  a network call at all;
- **in IAM**, so a bug in the process is still not enough.

Enforcing it in only one place is the failure mode this ADR is written
against. In-process only means an SDK call bypasses it; IAM only means the
worker discovers the boundary as an opaque `AccessDeniedException` at the
worst possible moment, with no idea which node asked for what.

## Decision

### One list, two renderings

`internal/runners.FunctionRegistry` is the single source of the allowlist. It
maps a logical name (normally `runners.NodeKey(workflow, nodeID)`) to a
`FunctionIdentity{ARN, ImageDigest}`.

- `FunctionRegistry.Resolve` is the in-process enforcement point. An
  unregistered name yields a typed `*runners.DispatchError` wrapping
  `ErrUnregisteredFunction`, classified `auth_or_policy` and **not**
  retryable — asking the same forbidden question again cannot succeed. The
  Lambda adapter calls it before it constructs a request, so a refused
  dispatch costs zero AWS calls (asserted by a hit counter in
  `internal/runners/lambda/adapter_test.go`).
- `runners.RenderWorkerIAMPolicy` renders the worker role's IAM policy *from
  that same registry*. A function that is not registered cannot appear in the
  policy, and a function that is registered must.

The registry refuses a wildcard ARN at registration time, for the same
reason: it is the policy's source, and a wildcard there would become a
wildcard in IAM.

### The policy

`deploy/aws/worker-iam-policy.json` is the checked-in template, rendered from
placeholder ARNs in account `000000000000` so an operator who applies it
unchanged grants access to nothing. A test
(`TestCheckedInTemplateMatchesTheRenderer`) re-renders it and fails on any
drift, so the committed reference cannot fall behind the code.

Five statements:

| Sid | Effect | Action | Resource |
| --- | --- | --- | --- |
| `InvokeRegisteredCodeRunnerFunctions` | Allow | `lambda:InvokeFunction` | every registered ARN, enumerated |
| `ReadRegisteredFunctionPinning` | Allow | `lambda:GetFunction` | every registered ARN, enumerated |
| `DenyInvokeOutsideRegisteredFunctions` | Deny | `lambda:InvokeFunction`, `lambda:InvokeFunctionUrl` | `NotResource`: every registered ARN |
| `ArtifactBucketObjects` | Allow | `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` | `arn:aws:s3:::<bucket>/<prefix>/*` |
| `ArtifactBucketListing` | Allow | `s3:ListBucket` | `arn:aws:s3:::<bucket>`, conditioned on `s3:prefix` |

Four notes on the shape:

- **`GetFunction` is granted, deliberately.** The adapter reads each
  function's resolved ECR image digest at load time and verifies it against
  the registry's pin. A pin the worker cannot read is a pin it cannot verify,
  so the read permission is part of what makes the pin real. It is scoped to
  exactly the same enumerated ARNs.
- **The explicit `Deny` is not decoration.** IAM evaluates an explicit Deny
  above any Allow, so a broad platform role or a future managed policy
  attached to the same worker cannot re-open invoke. `NotResource` here is
  the correct construct precisely because it inverts a small enumerated set.
- **The one permitted wildcard is an S3 object-key suffix** under a named
  bucket and a named prefix. Object-level S3 permissions cannot be expressed
  without it. Every other wildcard is refused: no `Resource: "*"`, no
  wildcard account, region, bucket, or function name. `ListBucket` is a
  bucket-level action, so its Resource is the bare bucket ARN and the prefix
  condition does the scoping. `TestCheckedInTemplateHasNoWildcardResource`
  asserts all of this against the committed file, parsed on its own terms.
- **No `lambda:UpdateFunctionCode`, `CreateFunction`, `AddPermission`, or
  `PutFunctionConcurrency`.** The worker invokes and reads. It does not
  deploy, and it cannot grant itself anything.

### Deployment posture

- The worker assumes this role through IRSA / OIDC workload identity (PRD
  §19.2). No long-lived keys (honesty condition h14). Task t17 implements the
  shared credential resolver; this adapter takes an
  `aws.CredentialsProvider` override only so tests can point at a fake.
- The runner functions are a separate security boundary from the worker (PRD
  §16.4): they carry their own execution role, and nothing in this policy
  grants the worker anything the function itself holds.
- A scoped-network policy (`network: none` or `egress-allowlist`) requires
  the function to be VPC-attached. The adapter verifies attachment and
  refuses without it — and does not claim to have *observed* the network
  posture, because what a subnet routes to is outside its view. Enforcing the
  posture is the VPC's job; saying so honestly is this adapter's.

## Consequences

- Adding a code node to a workflow is a two-part change: register the
  function identity (code/config) *and* re-render the IAM policy. That is
  friction on purpose — it is the same friction that makes the allowlist
  meaningful, and `RenderWorkerIAMPolicy` exists so the second half is
  mechanical rather than hand-edited.
- The registry refuses to render a policy from an empty registry rather than
  emitting a statement with an empty `Resource` list, which is invalid IAM
  and would tempt whoever applies it to "fix" it with a `*`.
- Rotating a function's image digest requires a registry update, because the
  adapter refuses to load when the deployed image and the pinned digest
  disagree. A redeploy that lands *between* load and invoke does not fail the
  invocation; it downgrades the `image_digest` observation to
  `{measured:false, complete:false}` with a note, because the digest is then
  still what the registry pins but no longer an observation about what ran.
- Multi-tenant deployments will eventually want per-namespace registries and
  per-namespace roles. This ADR covers one worker role over one registry;
  splitting it is additive and does not change the property.

## Alternatives considered

- **Wildcard invoke on a naming convention**
  (`arn:aws:lambda:*:*:function:culture-nodes-*`). Rejected: it makes the
  boundary a naming convention, and anyone who can create a function with a
  matching name has the worker's invoke permission. It also cannot be
  verified from the control plane, so nothing would fail until it was
  exploited.
- **IAM only, no in-process registry.** Rejected: the refusal arrives as an
  opaque `AccessDeniedException` with no operation, node, or identity
  attached, it costs a network round trip and an audit-log entry per
  attempt, and it puts the security boundary somewhere the test suite cannot
  reach.
- **In-process registry only, broad IAM.** Rejected: a single code path that
  forgets to call `Resolve` — a new adapter, a debug tool, a future batch
  invoker — reopens the whole account.
- **Resource tags instead of ARN enumeration** (`aws:ResourceTag/culture-nodes
  = true` condition). Rejected for now: it moves the allowlist into mutable
  AWS state that the control plane does not own and cannot render, and tag
  changes are not visible to the registry. Worth revisiting if the enumerated
  list becomes operationally unwieldy — the enumeration is what a policy-size
  limit would eventually push against.
