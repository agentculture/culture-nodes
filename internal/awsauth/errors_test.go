package awsauth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go"

	"github.com/agentculture/culture-nodes/internal/awsauth"
)

// TestMapAWSErrorNil proves MapAWSError(nil) returns nil, so a caller can
// call it unconditionally on whatever a real AWS SDK call returned.
func TestMapAWSErrorNil(t *testing.T) {
	if got := awsauth.MapAWSError(nil); got != nil {
		t.Fatalf("MapAWSError(nil) = %v, want nil", got)
	}
}

// TestMapAWSErrorIdempotent proves a *ConfigError passed back through
// MapAWSError comes back unchanged rather than re-wrapped or flattened into
// the generic fallback.
func TestMapAWSErrorIdempotent(t *testing.T) {
	original := &awsauth.ConfigError{Message: "already mapped", Remediation: "do the thing"}
	got := awsauth.MapAWSError(original)
	if got != original {
		t.Fatalf("MapAWSError(alreadyConfigError) = %v, want the same *ConfigError back unchanged", got)
	}
}

// TestMapAWSErrorTable is the table-driven acceptance test for MapAWSError's
// name-based detection (ec2-cli's map_aws_error split -- READ
// ec2/aws/client.py): every case names an error MapAWSError must recognize
// and the substrings its Message/Remediation must carry so the mapping is
// provably about the right thing, not just "produced some ConfigError".
func TestMapAWSErrorTable(t *testing.T) {
	cases := []struct {
		name               string
		err                error
		wantMessageHas     string
		wantRemediationHas string
	}{
		{
			name:               "missing region typed error",
			err:                &aws.MissingRegionError{},
			wantMessageHas:     "region",
			wantRemediationHas: "Region",
		},
		{
			name:               "shared config profile not exist typed error",
			err:                awsconfig.SharedConfigProfileNotExistError{Profile: "ghost-profile"},
			wantMessageHas:     "ghost-profile",
			wantRemediationHas: "AWS_PROFILE",
		},
		{
			name: "smithy AccessDenied API error",
			err: &smithy.GenericAPIError{
				Code:    "AccessDenied",
				Message: "User is not authorized to perform sts:AssumeRole",
			},
			wantMessageHas:     "AccessDenied",
			wantRemediationHas: "IAM",
		},
		{
			name: "smithy AccessDeniedException API error",
			err: &smithy.GenericAPIError{
				Code:    "AccessDeniedException",
				Message: "not authorized",
			},
			wantMessageHas:     "AccessDeniedException",
			wantRemediationHas: "IAM",
		},
		{
			name: "smithy ExpiredToken API error",
			err: &smithy.GenericAPIError{
				Code:    "ExpiredToken",
				Message: "the security token included in the request is expired",
			},
			wantMessageHas:     "expired",
			wantRemediationHas: "refresh",
		},
		{
			name: "smithy unrecognized service error falls back to a generic mapping",
			err: &smithy.GenericAPIError{
				Code:    "ThrottlingException",
				Message: "Rate exceeded",
			},
			wantMessageHas:     "ThrottlingException",
			wantRemediationHas: "retry",
		},
		{
			name:               "string fallback: no credential providers",
			err:                errors.New("failed to retrieve credentials: no valid providers in chain"),
			wantMessageHas:     "credentials are not configured",
			wantRemediationHas: "RoleARN",
		},
		{
			name:               "string fallback: NoCredentialProviders verbatim",
			err:                errors.New("NoCredentialProviders: no valid providers in chain"),
			wantMessageHas:     "credentials are not configured",
			wantRemediationHas: "AWS_ACCESS_KEY_ID",
		},
		{
			name:               "string fallback: no region text",
			err:                errors.New("could not resolve endpoint: region is not set"),
			wantMessageHas:     "region",
			wantRemediationHas: "Region",
		},
		{
			name:               "generic unrecognized error",
			err:                errors.New("dial tcp: connection refused"),
			wantMessageHas:     "connection refused",
			wantRemediationHas: "check",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := awsauth.MapAWSError(tc.err)
			if got == nil {
				t.Fatal("MapAWSError returned nil, want a *ConfigError")
			}
			if got.Message == "" {
				t.Fatal("ConfigError.Message is empty")
			}
			if got.Remediation == "" {
				t.Fatal("ConfigError.Remediation is empty")
			}
			if !strings.Contains(strings.ToLower(got.Message), strings.ToLower(tc.wantMessageHas)) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, tc.wantMessageHas)
			}
			if !strings.Contains(strings.ToLower(got.Remediation), strings.ToLower(tc.wantRemediationHas)) {
				t.Errorf("Remediation = %q, want it to contain %q", got.Remediation, tc.wantRemediationHas)
			}
			// Every mapped error must itself satisfy the error interface and
			// be findable via errors.As, since that is how a caller (a CLI
			// error renderer, a structured log) is expected to consume it.
			var asErr *awsauth.ConfigError
			if !errors.As(error(got), &asErr) {
				t.Error("errors.As(got, &*ConfigError) failed on MapAWSError's own return value")
			}
		})
	}
}

// TestConfigErrorImplementsError proves ConfigError.Error() returns Message
// alone (matching internal/clifmt.CliError's own convention of keeping
// Message and Remediation separately presentable).
func TestConfigErrorImplementsError(t *testing.T) {
	err := &awsauth.ConfigError{Message: "boom", Remediation: "fix it"}
	if err.Error() != "boom" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "boom")
	}
	var asError error = err
	if asError.Error() != "boom" {
		t.Fatalf("as error interface, Error() = %q, want %q", asError.Error(), "boom")
	}
}
