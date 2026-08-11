# deploy/aws

The AWS deployment profile (PRD §19.2). One file lives here so far.

## `worker-iam-policy.json`

The reference IAM policy for the **worker** role — the role that dispatches
code nodes to Lambda and reads/writes artifacts in S3. It is the deployment
half of ADR 0003 (`docs/adr/0003-lambda-runner-iam.md`); read that first for
why the policy is shaped the way it is.

It is a **template**, not a policy to apply as-is. The two function ARNs and
the bucket name are placeholders in account `000000000000`, so applying it
unchanged grants access to nothing.

### Regenerating it

The file is rendered by `runners.RenderWorkerIAMPolicy` from a
`runners.FunctionRegistry`, and
`internal/runners/iam_test.go:TestCheckedInTemplateMatchesTheRenderer`
re-renders it on every test run and fails on drift. Do not hand-edit it —
change the renderer, or render your own deployment's policy from your own
registry:

```go
policy, err := runners.RenderWorkerIAMPolicy(registry, runners.IAMOptions{
    ArtifactBucket: "your-artifact-bucket",
    ArtifactPrefix: "artifacts",
})
data, err := policy.MarshalIndent()
```

### The property it exists to hold

The worker may invoke **only** the function ARNs its registry holds,
enumerated one by one — no wildcard invoke, plus an explicit `Deny` on
everything else so another attached policy cannot re-open it. `GetFunction`
is granted on the same enumerated ARNs because the adapter verifies each
pinned ECR image digest against the deployed function at load time. S3 access
is scoped to one bucket and one key prefix.

### Attaching it

The worker assumes this role through IRSA / OIDC workload identity — no
long-lived access keys. The Lambda runner functions carry their own,
separate execution role: the runner is a distinct security boundary from the
worker (PRD §16.4), and nothing in this policy grants the worker anything the
functions themselves hold.

## Operator bootstrap (`bootstrap-operator.sh` + `dev-operator-policy.json`)

The one-time, **human-run** setup of the scoped identity every agent and
script operates as. With admin (first time: root) credentials active:

```bash
./deploy/aws/bootstrap-operator.sh          # profile name defaults to culture-nodes
```

It creates the `culture-nodes-dev` IAM user, attaches
`dev-operator-policy.json` (SQS/S3/ECR/Lambda fenced to `culture-nodes-*`
names; IAM only on `culture-nodes-*` roles with PassRole conditioned to
Lambda), mints one access key, and writes it straight into the named CLI
profile via `aws configure set` — the secret is never echoed. Idempotent:
existing user/policy are kept, and a second key is refused without
`FORCE=1`.

Conventions the rest of the tooling assumes:

- profile **`culture-nodes`**, region **us-east-1** (override per machine);
- every AWS resource this project creates is named `culture-nodes-*`;
- agents operate **only** on the scoped profile — the bootstrap credential
  is a human's, used for this script and then set aside; agents never run
  this script or handle key material (they will refuse — by policy, not
  by accident);
- rotation: `FORCE=1 ./bootstrap-operator.sh` mints a fresh key into the
  profile (delete the old one in IAM afterwards; users cap at two keys).

### Updating the policy

The committed `dev-operator-policy.json` is the source of truth; the live
policy is just its latest applied version. After any change to the JSON,
a human with admin credentials re-applies it with:

```bash
./deploy/aws/bootstrap-operator.sh update-policy
```

(idempotent; prunes the oldest non-default version when IAM's 5-version
cap is hit — git history is the rollback store, not IAM versions).
