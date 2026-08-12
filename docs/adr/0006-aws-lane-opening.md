# ADR 0006: Opening the AWS lane — SQS's role, Lambda's place, and the awslive test lane

- Status: accepted
- Date: 2026-08-12
- Issues: #25 (AWS lane follow-up: access provisioned), closing the
  decisions carried over from #7
- Prior art this builds on: ADR 0003 (Lambda runner IAM), ADR 0004 (AWS
  package isolation + credential chain), `deploy/aws/README.md` (the
  `culture-nodes-dev` operator bootstrap, PR #24)

## Context

Issue #7 parked everything real-AWS behind one premise: no credentials
existed on any machine. That premise dissolved on 2026-08-12 — account
435593604218 carries the scoped `culture-nodes-dev` IAM user under
`deploy/aws/dev-operator-policy.json`, and agent sessions on spark operate
as `AWS_PROFILE=culture-nodes` in us-east-1. The code was already written
and fake-tested (SQS driver, Lambda adapter, IAM renderer); what remained
were the five decisions #7 enumerated. This ADR records them, because the
alternative is deciding them silently one commit at a time.

## Decisions

### 1. SQS is the optional cloud-profile signal driver, and stays optional

The Postgres queue driver serves the thor+orin production pair and remains
the default. SQS is neither removed nor promoted: it is the signal driver a
cloud-profile deployment *may* choose, per the PRD's "SQS is a disposable
work signal only" ground rule — never authoritative state. The driver's
fake and chaos suites stay in the default test run so the code path cannot
rot (h7 from the original frame), and the new `awslive` live suite
(`internal/queue/sqs/awslive_test.go`) proves the same driver against the
real service on demand.

### 2. Lambda stays a first-class in-worker adapter, for now

Phase 2 introduced the network runner-service contract with headspace as
the reference deployment. The Lambda adapter predates it and could be
refolded as one runner-service implementation behind that contract — but
doing so today would add a network hop and a service to operate in front
of a lane that has no production user yet. The adapter therefore stays
first-class and in-worker, and the refold decision is *re-armed, not
dropped*: the trigger to revisit is the first real cloud deployment target
(the same trigger as decision 4). Recorded here so it cannot drift into
"it was always going to stay an adapter".

### 3. The awslive lane is manual, like the codex smoke — CI never runs it

The live suites (`-tags awslive` in `internal/runners/lambda` and
`internal/queue/sqs`) cost real invocations and real requests against real
credentials. They are armed by hand, from a machine holding the
`culture-nodes` profile, and skip silently everywhere else. CI does not
gain an AWS identity for them: LocalStack's fidelity for digest-pinned
container-image functions and IAM was the original risk r1 and remains
unverified, and a periodically-red cloud lane in CI is worse than a
documented manual one. The arming recipe lives in `deploy/aws/README.md`.

The lane's standing resources, all inside the operator policy's fences:

| Resource | Name |
| --- | --- |
| SQS queue | `culture-nodes-awslive` |
| ECR repository | `culture-nodes/runner` |
| Lambda function | `culture-nodes-runner` (arm64, container image) |
| Function execution role | `culture-nodes-lambda-exec` |
| Worker role (rendered policy) | `culture-nodes-worker` |

The function runs `cmd/nodes-runner-lambda` (new in this cycle): the
minimal, honest function side of the runner contract — executes an
operation's argv, reports process-reported exit facts, and refuses
workspaces, environment refs, and shell requests it cannot honour instead
of silently dropping them. Built by `deploy/aws/lambda-runner.Dockerfile`.

### 4. ECS/Fargate stays deferred

Bare-metal thor+orin is production. The PRD §24 "AWS production topology
and threat model" checkbox stays open until a real cloud deployment target
appears; nothing in this cycle builds toward Fargate.

### 5. Credential chain: named profile for dev, IRSA for k8s, unchanged

Dev machines use the `culture-nodes` named profile through ADR 0004's
shared chain (`internal/awsauth`), which already resolves profiles without
new code. IRSA/OIDC remains the intended production path and remains
untouched. No long-lived keys are added anywhere beyond the one scoped
dev-operator key the bootstrap minted.

## Consequences

- The AWS surface is live but fenced: everything the dev operator can
  create or invoke matches `culture-nodes-*` (or `culture-nodes/*` for
  ECR), so a misfired command cannot reach the account's other tenants.
- Engine-side spend protections (#16/#19's dispatch budget and cancel
  reap, v0.10.0) bound the blast radius of a misconfigured cloud lane;
  Lambda invocations ride the same protections when dispatched through the
  runner path.
- Two build-tagged live suites now exist; anyone touching the SQS driver
  or the Lambda adapter can prove their change against the real service in
  minutes without waiting for a cloud deployment to exist.
- Decision 2 leaves two dispatch idioms (in-worker adapter vs runner
  service) alive simultaneously. That asymmetry is the accepted cost of
  not building a service nobody deploys; the revisit trigger is explicit.
