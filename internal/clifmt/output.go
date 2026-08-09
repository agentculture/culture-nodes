package clifmt

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// EmitResult writes a human-readable command result to stdout, ensuring
// exactly one trailing newline.
func EmitResult(text string) {
	emitResult(os.Stdout, text)
}

// EmitResultJSON writes v to stdout as single-line JSON followed by a
// newline.
func EmitResultJSON(v any) error {
	return emitResultJSON(os.Stdout, v)
}

// EmitError writes err to stderr. In text mode it renders the two-line
// agent-first shape:
//
//	error: <message>
//	hint: <remediation>
//
// (the hint line only when Remediation is non-empty). In JSON mode it
// writes {"code","message","remediation"} as single-line JSON.
func EmitError(err *CliError, jsonMode bool) error {
	return emitError(os.Stderr, err, jsonMode)
}

// EmitDiagnostic writes a plain-text human diagnostic (progress, summary)
// to stderr, ensuring exactly one trailing newline. Diagnostics are never
// JSON-rendered — they are for human eyes, not the machine-readable
// result/error channels.
func EmitDiagnostic(message string) {
	emitDiagnostic(os.Stderr, message)
}

// The emitXxx helpers below take an explicit io.Writer so this package's
// own tests can assert behaviour against a bytes.Buffer instead of the
// process's real stdout/stderr. The public Emit* functions above are the
// stable contract callers use; production code always writes to the real
// streams.

func emitResult(w io.Writer, text string) {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	fmt.Fprint(w, text)
}

func emitResultJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func emitError(w io.Writer, err *CliError, jsonMode bool) error {
	if jsonMode {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(err)
	}
	if _, wErr := fmt.Fprintf(w, "error: %s\n", err.Message); wErr != nil {
		return wErr
	}
	if err.Remediation != "" {
		if _, wErr := fmt.Fprintf(w, "hint: %s\n", err.Remediation); wErr != nil {
			return wErr
		}
	}
	return nil
}

func emitDiagnostic(w io.Writer, message string) {
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	fmt.Fprint(w, message)
}
