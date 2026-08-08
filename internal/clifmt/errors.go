package clifmt

import "fmt"

// Exit-code policy (documented in `nodes learn`):
//
//	0   success
//	1   user-input error (bad flag, missing/invalid arg, unknown verb/path)
//	2   environment/setup error (tool not installed, feature not implemented)
//	3+  reserved for future categorisation
const (
	ExitSuccess   = 0
	ExitUserError = 1
	ExitEnvError  = 2
)

// IssuesURL is where NewEnvError points agents/humans to file a bug when an
// unexpected panic or error is caught by Guard.
const IssuesURL = "https://github.com/agentculture/culture-nodes/issues"

// CliError is the structured error every command-line failure surfaces as.
// The top-level dispatcher renders it via EmitError and exits with Code.
//
// JSON field order matches the field declaration order below (code,
// message, remediation) since encoding/json preserves struct field order.
type CliError struct {
	Code        int    `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// Error implements the error interface so CliError can be returned/wrapped
// through normal Go error-handling paths.
func (e *CliError) Error() string {
	return e.Message
}

// NewEnvError builds a CliError in the ExitEnvError bucket wrapping an
// unexpected panic or error value. Used by Guard so no stack trace ever
// reaches the user.
func NewEnvError(cause any) *CliError {
	return &CliError{
		Code:        ExitEnvError,
		Message:     fmt.Sprintf("unexpected: %v", cause),
		Remediation: fmt.Sprintf("file a bug at %s", IssuesURL),
	}
}
