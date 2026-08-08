# ADR 0004: AWS package isolation lint and the shared IRSA-ready credential chain

- Status: accepted
- Date: 2026-08-09
- Task: t17 (AWS package isolation lint + IRSA-ready credential chain)
- Spec claims: c17 (AWS code stays in AWS-specific packages)
- Honesty condition: h14 (no long-lived keys; IRSA/OIDC is the intended
  production credential path)
- Models cited (READ, not imported): open-bedrock-server's
  `get_aws_session` (`src/open_bedrock_server/utils/config_loader.py` lines
  285-360) for the resolution order; ec2-cli's `build_client`/`aws_call`
  split (`ec2/aws/client.py`) for the error-mapping shape

## Context

Three packages already talk to AWS directly: `internal/queue/sqs` (SQS,
`aws-sdk-go-v2`), `internal/runners/lambda` (Lambda, `aws-sdk-go-v2`, task
t13 — see ADR 0003), and `internal/artifacts/s3` (S3-compatible storage via
`minio-go`, not the AWS SDK, but still a boundary package by design). Each of
the two SDK-using packages had grown its own inline
`awsconfig.LoadDefaultConfig` option list, and nothing stopped a fourth
package — the engine, the API server, a future adapter — from importing the
AWS SDK directly and quietly becoming a second, unaudited place credentials
could leak into. c17 says AWS code stays in AWS-specific packages; this ADR
is what makes that an enforced property instead of a convention.

Two problems, one task:

1. **Nothing enumerated or enforced the isolation boundary.** A grep-based
   check (the pattern `internal/actors/neutrality_test.go` already uses for
   provider names) would work, but an import is a structured thing — grep
   would also fire on the string appearing in a comment or doc example, and
   would miss nothing but also prove nothing about *why* a match is real.
2. **Credential resolution was duplicated and had no IRSA story.** Both
   `sqs.New` and `lambda.New` built their own `config.LoadOptions` slice by
   hand, and neither reported *which* credential source actually resolved —
   an IRSA pod whose service-account role assumption started silently
   falling back to an EC2 instance role would be invisible until something
   downstream failed on the wrong permissions.

## Decision

### The isolation lint is a Go test, not a separate tool

`tests/lint/awsisolation_test.go` (package `testslint`) walks every
non-test `.go` file in the repo with `go/parser` (`parser.ImportsOnly`) and
fails if a file outside a sanctioned set imports
`github.com/aws/aws-sdk-go-v2` or anything under it. This mirrors
`internal/actors/neutrality_test.go`'s own precedent for "lint as a Go
test": it runs wherever `go test ./...` already runs (local, CI, no new
tool to install or wire into a separate lint stage), and it is precise
because it parses import declarations rather than matching text — a vendor
name in a comment or a doc string does not trip it, and neither does a
renamed or dot-imported SDK package escape it, since the check is on the
import path string itself.

The sanctioned list is a package-level `var` with its own doc comment citing
c17:

| Package | Why it is sanctioned |
| --- | --- |
| `internal/queue/sqs` | The SQS `queue.Queue` driver. |
| `internal/artifacts/s3` | The S3-compatible `artifacts.Store` driver. It uses `minio-go`, not the AWS SDK, today — sanctioned anyway as a standing boundary, per its own doc comment, for the day it needs the SDK directly. |
| `internal/runners/lambda` | The Lambda `runners.Runner` adapter (ADR 0003). |
| `internal/awsauth` | This task's shared credential resolver (below). |

`web/` (the React/Vite UI) and `tests/lint` itself are excluded from the
walk. `TestSanctionedPackagesActuallyExist` is a second, small test proving
every directory the allowlist names still exists as a real package — so a
rename that forgets to update the list fails loudly instead of the
allowlist silently protecting nothing.

**Proof the lint actually fires**, done and reverted as part of this task:
a throwaway `_ "github.com/aws/aws-sdk-go-v2/aws"` import was added to
`internal/engine/engine.go`, `go test ./tests/lint/...` was run and failed
naming exactly that file and import, and the import was then removed and
`go test` re-run clean. That round trip is the actual acceptance evidence
for "the lint enforces the boundary," not merely "the lint's logic looks
right by inspection."

### `internal/awsauth`: one resolver, five reported sources

`internal/awsauth.LoadConfig(ctx, Options) (aws.Config, Source, error)`
replaces the "hand-build a `config.LoadOptions` slice" pattern with a single
resolver both `sqs` and `lambda` can route through, following
`get_aws_session`'s own priority order:

1. **`SourceAssumeRole`** — an explicit `RoleARN` (`Options.RoleARN` or env
   `AWS_ROLE_ARN`), no web-identity token file: plain STS `AssumeRole`,
   using the ambient chain's own credentials as the base identity that asks
   to assume the role.
2. **`SourceWebIdentity`** — `RoleARN` *plus* a web-identity token file
   (`Options.WebIdentityTokenFile` or env `AWS_WEB_IDENTITY_TOKEN_FILE`):
   STS `AssumeRoleWithWebIdentity`. This is the EKS/IRSA path.
3. **`SourceProfile`** — a named shared-config profile
   (`Options.Profile`/`AWS_PROFILE`), no role.
4. **`SourceStaticKeys`** — explicit `AWS_ACCESS_KEY_ID`/
   `AWS_SECRET_ACCESS_KEY` (env only, matching `get_aws_session`), no role or
   profile.
5. **`SourceAmbient`** — none of the above: the SDK's own default chain
   (EC2/ECS/EKS instance role, or whatever `aws configure` last wrote).

Every link reports which one fired via `Options.Logf` (nil defaults to
`log.Printf`, the same "never silently drop the diagnostic" convention
`internal/queue/sqs.Config.Logf` already uses). An `Options` field always
takes precedence over its matching environment variable, so a caller with
an explicit value is never silently overridden by the process environment.

#### Why IRSA gets its own reported source instead of falling through to ambient

This is the one place this ADR deviates from "just copy `get_aws_session`
literally." aws-sdk-go-v2's *own* default chain already understands
`AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE` — the two env vars EKS's
pod-identity webhook injects into every IRSA pod — and would resolve them
correctly with zero help from this package, via nothing more than
`config.LoadDefaultConfig(ctx, WithRegion(region))`. That is precisely the
problem: if `LoadConfig` did nothing special for that pair, every
IRSA-authenticated deployment would report `Source = "ambient"`, which is
true but useless — a caller logging `Source` at startup could not
distinguish "IRSA is working as designed" from "something fell through to
an EC2 instance role I didn't expect." `LoadConfig` instead detects the pair
itself (whether supplied via `Options` or via the environment) and builds
the `stscreds.WebIdentityRoleProvider` explicitly, so IRSA — the path h14
calls out as the intended production credential source — is a named,
provably-taken branch, not an accident of how thorough the ambient chain
happens to be this SDK version.

#### `LoadConfig` never makes a real AWS call

For the `AssumeRole` and `WebIdentity` links, `LoadConfig` constructs (but
never invokes) an `stscreds.AssumeRoleProvider` /
`stscreds.WebIdentityRoleProvider` and wraps it in `aws.NewCredentialsCache`
— the actual STS call happens lazily, the first time something calls
`Credentials.Retrieve` on the returned `aws.Config`, which is always outside
this package. This was verified empirically (not assumed): a throwaway
probe against the vendored `config.LoadDefaultConfig` confirmed it performs
only local environment/file reads and returns synchronously, with no network
access, for every link this package's tests exercise — including the
profile link's `SharedConfigProfileNotExistError`, which the SDK itself
raises synchronously for a profile missing from the shared config/
credentials files. That is also why "bad profile" ends up a `*ConfigError`
constructed at `LoadConfig` time rather than something only discovered on
first use.

### Error mapping: ec2-cli's split, in Go

ec2-cli's `client.py` separates a configuration problem it can name and fix
before any request exists (`build_client`, code 2, a remediation) from a
runtime failure surfaced by an actual call (`aws_call` /
`map_aws_error`). `internal/awsauth` keeps the same two-sided shape:

- **`ConfigError{Message, Remediation}`** — the type both sides produce.
  It mirrors `internal/clifmt.CliError`'s own `{Message, Remediation}`
  shape (not a coincidence: this repo already had a convention for "an
  error a human or an agent reads should say what to do about it," and this
  package reuses it rather than inventing a third shape). `LoadConfig`
  constructs one directly and synchronously for: a `RoleARN` resolved with
  no `Options.Region` (STS needs a region to build its endpoint, and unlike
  a profile there is nothing else to fall back to — a hard error, not a
  warning); a web-identity token file with no resolvable role anywhere (STS
  `AssumeRoleWithWebIdentity` has no role to assume); and — via
  `MapAWSError` — a profile absent from the shared config/credentials
  files.
- **`MapAWSError(err) *ConfigError`** — the runtime-call-error translator,
  used both internally (wrapping `awsconfig.LoadDefaultConfig` failures) and
  exported for a caller's own later AWS calls made with the resolved
  `aws.Config`. Detection order mirrors `map_aws_error`: a typed check via
  `errors.As` wherever the SDK exposes a distinguishable type
  (`smithy.APIError` for service error codes such as `AccessDenied`;
  `aws.MissingRegionError`; `config.SharedConfigProfileNotExistError`), then
  a substring match on `err.Error()` as a fallback for the one case
  aws-sdk-go-v2 has no dedicated type for: "no credential provider in the
  chain produced anything," which botocore's v1 SDK named
  `NoCredentialsError` and aws-sdk-go-v2 does not name at all. ec2-cli's own
  `_is_no_credentials` needed no botocore import to make its name-based
  check either — the same trade-off, ported to Go: matching text is more
  fragile than matching a type, but the alternative is vendoring
  botocore-equivalent internals this package has no reason to depend on.

A profile without a region is a **warning** (via `Logf`), not a
`ConfigError` — the same open-bedrock-server "warn when RoleARN/Profile set
without region" idea, but split by how bad the ambiguity actually is: a
shared-config profile can carry its own region in `~/.aws/config`, so an
empty `Options.Region` there is not necessarily wrong, while a bare role
assumption genuinely has nothing else to resolve a region from.

### One consumer migrated as proof, a second migrated because it was trivial

The task asked for one proof migration and left the second optional. Both
turned out to be a small, uniform refactor — extract the "already have an
`aws.Config`, now build the driver/adapter" tail of the existing constructor
into a private helper, then add a second exported constructor that resolves
`aws.Config` via `awsauth.LoadConfig` instead of an inline
`awsconfig.LoadDefaultConfig` call and feeds the same helper:

- `internal/queue/sqs.NewDriverFromAuth(ctx, awsauth.Options, Config)` —
  helper `newDriver`.
- `internal/runners/lambda.NewFromAuth(ctx, awsauth.Options, Config)` —
  helper `newAdapter`, which also now performs the registry/revision
  validation (previously inline at the top of `New`) after `aws.Config` is
  resolved rather than before; this reorders two purely local checks (no
  network call moved on either side of the new call) and both constructors
  still refuse the same misconfigurations with the same messages, unchanged
  by existing tests.

Neither existing constructor (`sqs.New`, `lambda.New`) changed behavior or
signature — this is additive. Nothing outside these two packages calls
either constructor yet (`cmd/nodes` does not wire the workers up), so there
was no call site to update, and no cross-package migration risk. Wiring
`NewDriverFromAuth`/`NewFromAuth` into `cmd/nodes`'s actual startup path is
left to whichever task next builds that wiring.

## Consequences

- A future AWS-backed driver or adapter has an explicit checklist: add its
  package directory to `sanctionedAWSPackageDirs`, and prefer routing
  credential resolution through `internal/awsauth.LoadConfig` rather than
  reinventing another inline option list. Forgetting the first half fails
  CI (`go test ./tests/lint/...`) the moment the SDK import lands; forgetting
  the second half is not mechanically enforced, only encouraged by having a
  shared resolver be the path of least resistance.
- `internal/awsauth`'s own `Options.Region` deliberately does **not** fall
  back to `AWS_REGION`/`AWS_DEFAULT_REGION` the way `RoleARN`/
  `WebIdentityTokenFile`/`Profile` fall back to their env vars — a caller
  that wants role assumption to succeed must pass `Region` explicitly. This
  is a narrower contract than `get_aws_session`'s (boto3's `Session()`
  itself reads `AWS_REGION`), chosen so the "role needs a region" check is a
  property of what the caller declared, not of what else happened to be in
  the ambient environment at the moment `LoadConfig` ran.
- `go.mod`'s `github.com/aws/aws-sdk-go-v2/service/sts` moved from an
  indirect to a direct dependency (it was already present transitively via
  `aws-sdk-go-v2/config`) — `internal/awsauth` is the first package to build
  an `sts.Client` itself.

## Alternatives considered

- **A separate `golangci-lint` custom rule (depguard) instead of a Go
  test.** Rejected for now: it would need a new tool wired into CI and a
  config file kept in sync with the sanctioned list in two places instead
  of one; a Go test runs everywhere `go test ./...` already does, and
  `internal/actors/neutrality_test.go` already established the "lint as Go
  test" pattern this repo uses. Worth revisiting if the isolation rule grows
  beyond a single import-path check `golangci-lint`'s `depguard` handles
  natively.
- **Grep-based detection**, like the neutrality guard's provider-name check.
  Rejected specifically for this rule (unlike the neutrality guard, where
  grep is the deliberate choice): an import path is a structured piece of
  syntax, not free text, and `go/parser` costs nothing extra here while
  eliminating both false positives (a match inside a comment or string) and
  a class of false negatives (a dot import, though rare, would still need
  its own regex).
- **Let the default credential chain handle IRSA silently and report
  `Source = "ambient"` for it.** Rejected: it is technically correct (the
  SDK does resolve IRSA credentials that way on its own) but throws away
  exactly the operational signal h14 makes this task care about — see "Why
  IRSA gets its own reported source" above.
- **Thread `RoleSessionName`/`STSEndpoint` through environment variables
  only, no `Options` fields**, to stay closer to `get_aws_session`'s
  boto3-session-only surface. Rejected: this package's callers are Go
  constructors, not a CLI entry point, and an explicit `Options` field a
  driver's own `Config` can set (as `NewDriverFromAuth`/`NewFromAuth` do for
  `Region`) is more discoverable than an implicit env-var-only contract for
  the two fields that exist purely so tests can point the STS client at a
  fake without touching process-wide environment state.
