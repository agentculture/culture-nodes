package awsauth

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Source names which link in the chain resolved an aws.Config's credentials.
// Callers are expected to log it -- open-bedrock-server's
// test_aws_configuration reports the same fact under its "auth_method" key,
// for the same reason: which link fired is operationally significant (an
// IRSA pod that unexpectedly falls back to ambient EC2-instance-role
// credentials is a fact worth a log line, not silence).
type Source string

const (
	// SourceAssumeRole is a plain STS AssumeRole: RoleARN was set (directly
	// or via AWS_ROLE_ARN) with no web-identity token file alongside it.
	SourceAssumeRole Source = "assume_role"
	// SourceWebIdentity is STS AssumeRoleWithWebIdentity -- the EKS/IRSA
	// path (RoleARN plus a web-identity token file, either set directly or
	// via AWS_ROLE_ARN / AWS_WEB_IDENTITY_TOKEN_FILE). See the package doc's
	// "Why IRSA gets its own reported Source" section for why this is a
	// distinct, explicitly detected link rather than a fallthrough to
	// SourceAmbient.
	SourceWebIdentity Source = "web_identity"
	// SourceProfile is a named shared-config profile (Options.Profile or
	// AWS_PROFILE), with no RoleARN in play.
	SourceProfile Source = "profile"
	// SourceStaticKeys is explicit long-lived credentials from
	// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (and optionally
	// AWS_SESSION_TOKEN). Reported deliberately loudly: h14 treats static
	// keys as the path to avoid in production, and a caller that logs
	// Source at startup should be able to notice one is in use.
	SourceStaticKeys Source = "static_keys"
	// SourceAmbient is aws-sdk-go-v2's own default credential chain with no
	// resolver-level option this package added -- EC2/ECS/EKS instance
	// roles, ambient environment credentials the SDK itself understands, or
	// (in a dev shell) whatever `aws configure` last wrote.
	SourceAmbient Source = "ambient"
)

// defaultRoleSessionName is used for STS AssumeRole / AssumeRoleWithWebIdentity
// when Options.RoleSessionName is empty, mirroring open-bedrock-server's own
// default of a fixed, recognizable session name (its AWS_ROLE_SESSION_NAME
// default is "bedrock-server-session") rather than leaving it to the SDK's
// own generated value, so a session assumed by this binary is identifiable
// in CloudTrail.
const defaultRoleSessionName = "culture-nodes-session"

// Options configures LoadConfig. Every field mirrors an environment variable
// the standard AWS CLI/SDK chain already recognizes (named in each field's
// doc comment); a non-empty field here always takes precedence over that
// variable, so a caller with an explicit value never has it silently
// overridden by the process environment.
type Options struct {
	// Region is the AWS region to resolve credentials and build clients
	// for. Unlike RoleARN/WebIdentityTokenFile/Profile, LoadConfig never
	// falls back to an environment variable for Region on its own --
	// AWS_REGION/AWS_DEFAULT_REGION are still honored, but only by the
	// underlying SDK chain for the profile and ambient links, never to
	// satisfy the "role assumption needs a region" check below. A caller
	// that wants role assumption to succeed must pass Region explicitly.
	Region string
	// RoleARN is the IAM role to assume via STS. Falls back to env
	// AWS_ROLE_ARN.
	RoleARN string
	// WebIdentityTokenFile is the path to an OIDC/IRSA web-identity token
	// (the file EKS's pod-identity webhook projects into every IRSA pod).
	// Falls back to env AWS_WEB_IDENTITY_TOKEN_FILE. Only meaningful
	// alongside a resolved RoleARN -- STS AssumeRoleWithWebIdentity has no
	// role to assume otherwise.
	WebIdentityTokenFile string
	// Profile is a named profile from the shared AWS config/credentials
	// files. Falls back to env AWS_PROFILE. Ignored when a RoleARN
	// resolves, matching get_aws_session's priority order.
	Profile string
	// RoleSessionName overrides defaultRoleSessionName for the AssumeRole /
	// AssumeRoleWithWebIdentity session name. Falls back to env
	// AWS_ROLE_SESSION_NAME, matching open-bedrock-server.
	RoleSessionName string
	// HTTPClient overrides the SDK's HTTP client on every aws.Config
	// LoadConfig returns. Tests use it to keep the whole resolution chain
	// off the real network; production leaves it nil.
	HTTPClient *http.Client
	// STSEndpoint overrides where the STS client built for the
	// role-assumption and web-identity links sends requests. It has no
	// effect on the credentials returned by LoadConfig itself (which never
	// calls STS -- see the package doc), only on where the *provider it
	// constructs* would call if something later invoked it. Tests point
	// this at an httptest.Server to exercise the constructed provider
	// end-to-end; production leaves it empty.
	STSEndpoint string
	// Logf receives a formatted diagnostic naming the Source LoadConfig
	// resolved, and any non-fatal warning along the way (e.g. a profile set
	// without a region). Nil means log.Printf, matching this repo's other
	// AWS driver Configs (internal/queue/sqs.Config.Logf,
	// internal/runners/lambda's use of Config.Clock as the equivalent
	// "nil means a sensible default" convention) -- never silently dropped.
	Logf func(format string, args ...any)
}

func (o Options) logf() func(string, ...any) {
	if o.Logf != nil {
		return o.Logf
	}
	return log.Printf
}

// firstNonEmpty returns the first non-empty string among vals, or "" if all
// are empty. Used to apply "explicit Options field, else the matching
// environment variable" precedence throughout LoadConfig.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// LoadConfig resolves AWS credentials in priority order -- explicit role
// assumption (plain or IRSA web-identity), a named profile, static keys,
// then the SDK's ambient default chain -- and reports which link resolved
// them. See the package doc for the models this order and the error split
// are copied from, and for why LoadConfig never makes a real AWS call.
func LoadConfig(ctx context.Context, opts Options) (aws.Config, Source, error) {
	logf := opts.logf()

	roleARN := firstNonEmpty(opts.RoleARN, os.Getenv("AWS_ROLE_ARN"))
	tokenFile := firstNonEmpty(opts.WebIdentityTokenFile, os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"))
	profile := firstNonEmpty(opts.Profile, os.Getenv("AWS_PROFILE"))
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	switch {
	case roleARN != "":
		// c17/h14: role assumption (plain or IRSA) needs a region to
		// address STS with. Unlike a profile (whose own config file may
		// carry a region) there is nothing else here for the SDK to fall
		// back to, so this is a hard configuration error rather than the
		// softer warning a profile-without-region gets below -- ec2-cli's
		// build_client makes the same "no region" case a ConfigError, not
		// a warning that is easy to miss.
		if opts.Region == "" {
			return aws.Config{}, "", &ConfigError{
				Message: fmt.Sprintf("AWS_ROLE_ARN %q is set but no AWS region is configured", roleARN),
				Remediation: "set Options.Region (there is no environment-variable fallback for role assumption specifically) " +
					"-- STS AssumeRole/AssumeRoleWithWebIdentity need a region to build their endpoint",
			}
		}
		if tokenFile != "" {
			return loadWebIdentity(ctx, opts, roleARN, tokenFile, logf)
		}
		return loadAssumeRole(ctx, opts, roleARN, logf)

	case tokenFile != "":
		// A web-identity token file with no role to assume it under. Real
		// IRSA always sets AWS_ROLE_ARN alongside AWS_WEB_IDENTITY_TOKEN_FILE
		// (the pod-identity webhook injects both together), so reaching
		// this case means a caller set the token file without the role --
		// the same shape open-bedrock-server's _web_identity_session raises
		// ValueError for.
		return aws.Config{}, "", &ConfigError{
			Message: "AWS_WEB_IDENTITY_TOKEN_FILE is set but no role ARN is configured",
			Remediation: "set Options.RoleARN (or AWS_ROLE_ARN) -- a web-identity token still names " +
				"the role STS should assume with it",
		}

	case profile != "":
		if opts.Region == "" {
			// A soft warning, not a ConfigError: a shared-config profile
			// can carry its own region in ~/.aws/config, so an empty
			// Options.Region here is not necessarily wrong -- but it is
			// worth surfacing, mirroring open-bedrock-server's
			// startup-validation idea of warning when a profile is set
			// without an explicit region.
			logf("awsauth: AWS_PROFILE %q is set but Options.Region is empty; "+
				"falling back to whatever region (if any) the profile itself configures", profile)
		}
		return loadProfile(ctx, opts, profile, logf)

	case accessKey != "" && secretKey != "":
		return loadStaticKeys(ctx, opts, accessKey, secretKey, logf)

	default:
		return loadAmbient(ctx, opts, logf)
	}
}

// baseLoadOptions builds the config.LoadOptions common to every link:
// region (when set) and an HTTP client override (when set).
func baseLoadOptions(opts Options) []func(*awsconfig.LoadOptions) error {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	if opts.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(opts.HTTPClient))
	}
	return loadOpts
}

// roleSessionName resolves Options.RoleSessionName, then AWS_ROLE_SESSION_NAME,
// then defaultRoleSessionName -- open-bedrock-server's own priority order.
func roleSessionName(opts Options) string {
	return firstNonEmpty(opts.RoleSessionName, os.Getenv("AWS_ROLE_SESSION_NAME"), defaultRoleSessionName)
}

// stsClientFor builds an STS client from base, pointed at opts.STSEndpoint
// when set. It never issues a request itself.
func stsClientFor(base aws.Config, opts Options) *sts.Client {
	return sts.NewFromConfig(base, func(o *sts.Options) {
		if opts.STSEndpoint != "" {
			endpoint := opts.STSEndpoint
			o.BaseEndpoint = &endpoint
		}
	})
}

// loadAssumeRole resolves credentials via a plain STS AssumeRole: the base
// config's own (ambient) credentials are used to call AssumeRole, and the
// result is cached and returned as the resolved aws.Config's Credentials.
//
// The AssumeRoleProvider this builds is never invoked here -- construction
// alone makes no AWS call (see the package doc's "No real AWS calls" note).
func loadAssumeRole(ctx context.Context, opts Options, roleARN string, logf func(string, ...any)) (aws.Config, Source, error) {
	base, err := awsconfig.LoadDefaultConfig(ctx, baseLoadOptions(opts)...)
	if err != nil {
		return aws.Config{}, "", MapAWSError(err)
	}

	client := stsClientFor(base, opts)
	provider := stscreds.NewAssumeRoleProvider(client, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = roleSessionName(opts)
	})
	base.Credentials = aws.NewCredentialsCache(provider)

	logf("awsauth: resolved credentials via STS AssumeRole (role_arn=%s, source=%s)", roleARN, SourceAssumeRole)
	return base, SourceAssumeRole, nil
}

// loadWebIdentity resolves credentials via STS AssumeRoleWithWebIdentity --
// the IRSA path. Unlike loadAssumeRole, the base config carries no
// credentials of its own: AssumeRoleWithWebIdentity authenticates with the
// bearer JWT from tokenFile, not with a pre-existing AWS identity, so there
// is nothing for an ambient credential chain to contribute here.
//
// Like loadAssumeRole, the WebIdentityRoleProvider this builds is never
// invoked here.
func loadWebIdentity(ctx context.Context, opts Options, roleARN, tokenFile string, logf func(string, ...any)) (aws.Config, Source, error) {
	// LoadDefaultConfig is used here purely for its usual defaults (HTTP
	// client, retryer, region) via baseLoadOptions -- not for whatever
	// credentials it would otherwise resolve, which base.Credentials
	// overwrites immediately below. AssumeRoleWithWebIdentity authenticates
	// with tokenFile's bearer JWT, not with a pre-existing AWS identity, so
	// nothing an ambient chain might contribute here is ever used.
	base, err := awsconfig.LoadDefaultConfig(ctx, baseLoadOptions(opts)...)
	if err != nil {
		return aws.Config{}, "", MapAWSError(err)
	}

	client := stsClientFor(base, opts)
	provider := stscreds.NewWebIdentityRoleProvider(client, roleARN, stscreds.IdentityTokenFile(tokenFile), func(o *stscreds.WebIdentityRoleOptions) {
		o.RoleSessionName = roleSessionName(opts)
	})
	base.Credentials = aws.NewCredentialsCache(provider)

	logf("awsauth: resolved credentials via STS AssumeRoleWithWebIdentity/IRSA (role_arn=%s, token_file=%s, source=%s)",
		roleARN, tokenFile, SourceWebIdentity)
	return base, SourceWebIdentity, nil
}

// loadProfile resolves credentials from a named shared-config profile.
// awsconfig.LoadDefaultConfig itself detects a profile missing from the
// shared config/credentials files synchronously (config.SharedConfigProfileNotExistError,
// with no network access -- see the package doc), so a bad profile surfaces
// here as a *ConfigError via MapAWSError, not later at first use.
func loadProfile(ctx context.Context, opts Options, profile string, logf func(string, ...any)) (aws.Config, Source, error) {
	loadOpts := append(baseLoadOptions(opts), awsconfig.WithSharedConfigProfile(profile))
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return aws.Config{}, "", MapAWSError(err)
	}
	logf("awsauth: resolved credentials via AWS_PROFILE %q (source=%s)", profile, SourceProfile)
	return cfg, SourceProfile, nil
}

// loadStaticKeys resolves credentials from explicit long-lived keys.
// AWS_SESSION_TOKEN is read directly (rather than threaded through Options)
// because a session token has no meaning without the access/secret key pair
// it is issued alongside, matching get_aws_session's own env-only handling
// of it.
func loadStaticKeys(ctx context.Context, opts Options, accessKey, secretKey string, logf func(string, ...any)) (aws.Config, Source, error) {
	sessionToken := os.Getenv("AWS_SESSION_TOKEN")
	loadOpts := append(baseLoadOptions(opts),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)))
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return aws.Config{}, "", MapAWSError(err)
	}
	logf("awsauth: resolved credentials via static AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY (source=%s)", SourceStaticKeys)
	return cfg, SourceStaticKeys, nil
}

// loadAmbient resolves credentials from the SDK's own default chain with no
// resolver-level option this package added beyond region/HTTP client.
func loadAmbient(ctx context.Context, opts Options, logf func(string, ...any)) (aws.Config, Source, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, baseLoadOptions(opts)...)
	if err != nil {
		return aws.Config{}, "", MapAWSError(err)
	}
	logf("awsauth: no role/profile/static keys configured; resolved credentials via the SDK's ambient default chain (source=%s)", SourceAmbient)
	return cfg, SourceAmbient, nil
}
