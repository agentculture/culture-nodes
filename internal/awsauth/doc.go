// Package awsauth resolves AWS credentials through the standard chain --
// AssumeRole, then IRSA web-identity, then a named profile, then explicit
// static keys, then the SDK's ambient default chain -- and reports which
// link resolved them, so callers can log that fact instead of discovering it
// only when something fails.
//
// This is one of the four sanctioned AWS SDK import sites the isolation lint
// in tests/lint/awsisolation_test.go enforces (spec claim c17; alongside
// internal/queue/sqs, internal/artifacts/s3, and internal/runners/lambda).
// It exists so those driver/adapter packages -- and any added later -- share
// one resolver instead of each hand-rolling awsconfig.LoadDefaultConfig
// option lists, and so a caller outside the sanctioned set (cmd/nodes, a
// future control-plane wiring) can request credentials by passing this
// package's plain-string Options, never an aws.Config, keeping the AWS SDK
// import itself confined to the sanctioned packages.
//
// # Models
//
// The resolution order is copied from open-bedrock-server's get_aws_session
// (READ src/open_bedrock_server/utils/config_loader.py lines 285-360): try
// role assumption, then a web-identity token file, then a named profile,
// then static keys, then whatever the SDK's own default chain finds. This
// package's LoadConfig follows the same order, expressed against
// aws-sdk-go-v2's config.LoadOptions and the stscreds credential providers
// rather than boto3 Sessions.
//
// The error-mapping split is copied from ec2-cli's build_client/aws_call
// split (READ ec2/aws/client.py): a synchronous, locally-detectable
// configuration problem (no region configured for a role that needs one to
// address STS, a profile name that does not resolve) is a typed
// *ConfigError with an actionable Remediation, built before any request
// exists; a failure surfacing from an actual AWS call later (through
// whatever aws.Config LoadConfig returned) is translated by MapAWSError,
// name-based the same way ec2-cli's map_aws_error is -- aws-sdk-go-v2 has no
// single "NoCredentialProviders" type the way botocore did for v1, so that
// case (and a few others) are detected by matching on the returned error's
// text, exactly as ec2-cli's _is_no_credentials matches on
// type(exc).__name__ without importing botocore.
//
// # Why IRSA gets its own reported Source
//
// A pod running under EKS IRSA has AWS_ROLE_ARN and
// AWS_WEB_IDENTITY_TOKEN_FILE set in its environment by the pod-identity
// webhook. aws-sdk-go-v2's own default credential chain already understands
// both env vars and would quietly do the right thing if LoadConfig did
// nothing special and simply called config.LoadDefaultConfig with no
// options -- but then every IRSA-authenticated deployment would show up as
// Source "ambient" in a caller's startup log, indistinguishable from "found
// something, who knows what." LoadConfig instead detects the RoleARN +
// WebIdentityTokenFile pair itself and builds the
// stscreds.WebIdentityRoleProvider explicitly, so IRSA is a named,
// intentionally reported resolution path (SourceWebIdentity), not a silent
// fallthrough -- the honesty condition h14 (no long-lived keys; IRSA/OIDC is
// the intended production path per docs/adr/0003-lambda-runner-iam.md)
// deserves to be visible in whatever observes Source, not merely true by
// accident of the default chain's own behavior.
//
// # No real AWS calls from LoadConfig
//
// LoadConfig never issues a network request. For the role-assumption and
// web-identity paths it constructs (but does not invoke) an
// stscreds.AssumeRoleProvider / stscreds.WebIdentityRoleProvider and wraps
// it in aws.NewCredentialsCache -- the STS call happens lazily, the first
// time something calls Credentials.Retrieve on the returned aws.Config,
// which is always outside this package. Loading a profile or the ambient
// chain only reads local environment variables and (for a profile) local
// config/credentials files -- confirmed empirically against this module's
// vendored config.LoadDefaultConfig, which returns
// config.SharedConfigProfileNotExistError synchronously for a profile
// missing from those files, with no network access. That is also why a bad
// profile is a *ConfigError alongside "no region with role" rather than
// something only MapAWSError ever sees.
package awsauth
