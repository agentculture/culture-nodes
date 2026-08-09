package clifmt

import (
	"errors"
	"strings"
	"testing"
)

func TestCliErrorImplementsError(t *testing.T) {
	err := &CliError{Code: ExitUserError, Message: "bad flag", Remediation: "try again"}

	var asError error = err
	if asError.Error() != "bad flag" {
		t.Fatalf("Error() = %q, want %q", asError.Error(), "bad flag")
	}

	var target *CliError
	if !errors.As(asError, &target) {
		t.Fatalf("errors.As failed to unwrap *CliError")
	}
	if target.Code != ExitUserError {
		t.Fatalf("unwrapped Code = %d, want %d", target.Code, ExitUserError)
	}
}

func TestNewEnvErrorWrapsCause(t *testing.T) {
	err := NewEnvError("boom")

	if err.Code != ExitEnvError {
		t.Fatalf("Code = %d, want %d", err.Code, ExitEnvError)
	}
	if !strings.Contains(err.Message, "boom") {
		t.Fatalf("Message %q does not mention the cause", err.Message)
	}
	if !strings.Contains(err.Remediation, IssuesURL) {
		t.Fatalf("Remediation %q does not point at %s", err.Remediation, IssuesURL)
	}
}

func TestNewEnvErrorWrapsArbitraryPanicValue(t *testing.T) {
	// panic() accepts any value, not just errors/strings; NewEnvError must
	// not blow up formatting one.
	err := NewEnvError(42)
	if !strings.Contains(err.Message, "42") {
		t.Fatalf("Message %q does not mention the panic value", err.Message)
	}
}

func TestExitCodePolicyValues(t *testing.T) {
	// Locks the exit-code policy documented in `nodes learn` — these are a
	// stable contract other repos in the family rely on.
	if ExitSuccess != 0 {
		t.Fatalf("ExitSuccess = %d, want 0", ExitSuccess)
	}
	if ExitUserError != 1 {
		t.Fatalf("ExitUserError = %d, want 1", ExitUserError)
	}
	if ExitEnvError != 2 {
		t.Fatalf("ExitEnvError = %d, want 2", ExitEnvError)
	}
}
