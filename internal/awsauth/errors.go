package awsauth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go"
)

// ConfigError is a configuration problem this package (or MapAWSError, for a
// caller's own later AWS calls) can name and remediate -- never a bare
// wrapped SDK error a caller has to pattern-match on message text.
//
// It mirrors ec2-cli's CliError{message, remediation} shape (READ
// ec2/aws/client.py) and this repo's own internal/clifmt.CliError, which
// carries the same two fields for the same reason: an error a human or an
// agent reads should say what to do about it, not just what went wrong.
type ConfigError struct {
	Message     string
	Remediation string
}

// Error implements the error interface. It returns Message alone --
// Remediation is a separate field precisely so a caller (a CLI's
// error/hint renderer, a structured log) can present the two independently,
// the same reason internal/clifmt.CliError.Error() returns only its own
// Message.
func (e *ConfigError) Error() string {
	return e.Message
}

// MapAWSError translates err -- typically returned from a real AWS call made
// with an aws.Config LoadConfig produced, or (as LoadConfig's own callers
// above already do) from awsconfig.LoadDefaultConfig itself -- into a
// *ConfigError with an actionable Remediation. Calling it with err == nil
// returns nil.
//
// Detection order mirrors ec2-cli's map_aws_error: a typed check via
// errors.As wherever the SDK exposes a distinguishable type (smithy.APIError
// for service error codes such as AccessDenied; this module's own
// aws.MissingRegionError and config.SharedConfigProfileNotExistError), then
// a substring match on err.Error() as a fallback. The fallback exists
// because aws-sdk-go-v2 does not expose one dedicated type for "no
// credential provider in the chain produced anything" the way botocore's
// NoCredentialsError did for v1 -- ec2-cli's own _is_no_credentials needed
// no botocore import to make its name-based check either, and this is the
// same trade-off in Go: matching text is more fragile than matching a type,
// but the alternative is vendoring botocore-equivalent internals this
// package has no reason to depend on.
func MapAWSError(err error) *ConfigError {
	if err == nil {
		return nil
	}

	// Idempotent: a *ConfigError passed back in (e.g. a caller re-wrapping
	// an error LoadConfig already returned) comes back unchanged rather than
	// flattened into the generic fallback below.
	var already *ConfigError
	if errors.As(err, &already) {
		return already
	}

	var missingRegion *aws.MissingRegionError
	if errors.As(err, &missingRegion) {
		return &ConfigError{
			Message:     "no AWS region configured: " + err.Error(),
			Remediation: "set Options.Region, or export AWS_REGION/AWS_DEFAULT_REGION",
		}
	}

	var badProfile awsconfig.SharedConfigProfileNotExistError
	if errors.As(err, &badProfile) {
		return &ConfigError{
			Message: fmt.Sprintf("AWS profile %q does not exist in the shared config/credentials files", badProfile.Profile),
			Remediation: "check AWS_PROFILE (or Options.Profile) names a profile present in ~/.aws/config or ~/.aws/credentials, " +
				"or AWS_CONFIG_FILE/AWS_SHARED_CREDENTIALS_FILE if those are overridden",
		}
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		switch code {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedException":
			return &ConfigError{
				Message:     fmt.Sprintf("AWS denied the request (%s: %s)", code, apiErr.ErrorMessage()),
				Remediation: "check the IAM policy attached to the resolved credentials grants the required actions",
			}
		case "ExpiredToken", "ExpiredTokenException", "RequestExpired":
			return &ConfigError{
				Message:     fmt.Sprintf("AWS credentials have expired (%s: %s)", code, apiErr.ErrorMessage()),
				Remediation: "refresh the credential source (re-assume the role, refresh the web-identity token, or rotate static keys)",
			}
		}
		return &ConfigError{
			Message:     fmt.Sprintf("AWS request failed (%s: %s)", code, apiErr.ErrorMessage()),
			Remediation: "check AWS service status and the resolved credentials' permissions, then retry",
		}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower, "no valid providers", "nocredentialproviders", "nocredentialsproviders",
		"failed to retrieve credentials", "could not find credentials", "no credentials"):
		return &ConfigError{
			Message: "AWS credentials are not configured: " + msg,
			Remediation: "set Options.RoleARN, Options.Profile, AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, " +
				"or run where an ambient credential source is available (EC2/ECS/EKS instance role)",
		}
	case containsAny(lower, "noregion", "region is not set", "missing region", "no region"):
		return &ConfigError{
			Message:     "no AWS region configured: " + msg,
			Remediation: "set Options.Region, or export AWS_REGION/AWS_DEFAULT_REGION",
		}
	}

	return &ConfigError{
		Message:     "AWS error: " + msg,
		Remediation: "check the AWS configuration (region, credentials, and IAM permissions) and retry",
	}
}

// containsAny reports whether s contains any of substrs.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
