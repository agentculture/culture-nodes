# deploy/aws

The AWS deployment profile (PRD §19.2).

## Start here: `PREREQUISITES.md` + `preflight.py`

Before provisioning anything, run:

```bash
./deploy/aws/preflight.py        # read-only; exits 0 when the account is ready
```

It checks every account-level prerequisite and, for each failure, prints why
the check exists, who is allowed to fix it, and the exact command that does.
`PREREQUISITES.md` is the same material written for a DevOps reader who has
never seen this repository — hand them that page and nothing else.

Two prerequisites need an **account admin**, not the scoped operator profile,
and both are wrapped by `bootstrap-operator.sh`:

```bash
./deploy/aws/bootstrap-operator.sh enable-region il-central-1   # opt-in region
./deploy/aws/bootstrap-operator.sh update-policy                # grant RDS
```

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

## Live test lane (`awslive`)

The manual live lane over the fake-tested AWS code paths (ADR 0006 records
the decisions; issue #25 opened it). Two build-tagged suites, both skipping
silently unless armed, both costing real requests when armed:

```bash
export AWS_PROFILE=culture-nodes AWS_REGION=us-east-1

# SQS driver against the real service:
NODES_TEST_SQS_QUEUE_URL=$(aws sqs get-queue-url --queue-name culture-nodes-awslive --output text) \
  go test -tags awslive ./internal/queue/sqs/ -run TestLive -v

# Lambda adapter against the real function:
NODES_TEST_LAMBDA_ARN=$(aws lambda get-function --function-name culture-nodes-runner \
    --query Configuration.FunctionArn --output text) \
NODES_TEST_LAMBDA_IMAGE_DIGEST=$(aws lambda get-function --function-name culture-nodes-runner \
    --query 'Code.ResolvedImageUri' --output text | sed 's/.*@//') \
  go test -tags awslive ./internal/runners/lambda/ -run TestLive -v
```

CI never runs these (ADR 0006 decision 3); they are the codex-smoke idiom
applied to AWS.

### Standing resources

| Resource | Name | Created by |
| --- | --- | --- |
| SQS queue | `culture-nodes-awslive` | `aws sqs create-queue` (below) |
| ECR repository | `culture-nodes/runner` | `aws ecr create-repository` (below) |
| Lambda function | `culture-nodes-runner` | `aws lambda create-function` (below) |
| Execution role | `culture-nodes-lambda-exec` | `aws iam create-role` (below) |
| Worker role | `culture-nodes-worker` | rendered policy, see below |

All of it sits inside the dev-operator policy's fences, so the scoped
profile can create, update, and tear all of it down.

### (Re)creating the lane

```bash
export AWS_PROFILE=culture-nodes AWS_REGION=us-east-1
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

# 1. The signal queue.
aws sqs create-queue --queue-name culture-nodes-awslive

# 2. The runner image (build from the repo root; arch follows the host).
aws ecr create-repository --repository-name culture-nodes/runner
aws ecr get-login-password | docker login --username AWS --password-stdin \
  "$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com"
docker build -f deploy/aws/lambda-runner.Dockerfile \
  -t "$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/culture-nodes/runner:latest" .
docker push "$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/culture-nodes/runner:latest"

# 3. Let the Lambda service pull the image. Without this repository policy
#    CreateFunction fails with "Lambda does not have permission to access
#    the ECR image" (found live 2026-08-12; ecr:SetRepositoryPolicy joined
#    the dev-operator policy for it).
aws ecr set-repository-policy --repository-name culture-nodes/runner --policy-text '{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "LambdaPull",
    "Effect": "Allow",
    "Principal": {"Service": "lambda.amazonaws.com"},
    "Action": ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"],
    "Condition": {"StringLike": {"aws:sourceArn": "arn:aws:lambda:us-east-1:'"$ACCOUNT"':function:culture-nodes-*"}}
  }]
}'

# 4. The function's execution role (logs only).
aws iam create-role --role-name culture-nodes-lambda-exec \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
aws iam attach-role-policy --role-name culture-nodes-lambda-exec \
  --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole

# 5. The function, pinned to the pushed digest, matching the build arch.
DIGEST=$(aws ecr describe-images --repository-name culture-nodes/runner \
  --image-ids imageTag=latest --query 'imageDetails[0].imageDigest' --output text)
aws lambda create-function --function-name culture-nodes-runner \
  --package-type Image \
  --code ImageUri="$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com/culture-nodes/runner@$DIGEST" \
  --role "arn:aws:iam::$ACCOUNT:role/culture-nodes-lambda-exec" \
  --architectures arm64 --timeout 120 --memory-size 512

# 6. The worker role, carrying the policy the registry renders (ADR 0003).
#    Render it with runners.RenderWorkerIAMPolicy for a registry holding the
#    function above, then:
aws iam create-role --role-name culture-nodes-worker \
  --assume-role-policy-document "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"arn:aws:iam::$ACCOUNT:root\"},\"Action\":\"sts:AssumeRole\"}]}"
aws iam put-role-policy --role-name culture-nodes-worker \
  --policy-name culture-nodes-worker-dispatch --policy-document file://rendered-worker-policy.json
```
