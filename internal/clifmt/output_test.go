package clifmt

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitResultAddsTrailingNewline(t *testing.T) {
	var buf bytes.Buffer
	emitResult(&buf, "hello")
	if buf.String() != "hello\n" {
		t.Fatalf("got %q, want %q", buf.String(), "hello\n")
	}
}

func TestEmitResultDoesNotDoubleNewline(t *testing.T) {
	var buf bytes.Buffer
	emitResult(&buf, "hello\n")
	if buf.String() != "hello\n" {
		t.Fatalf("got %q, want %q", buf.String(), "hello\n")
	}
}

func TestEmitResultJSONIsSingleLine(t *testing.T) {
	var buf bytes.Buffer
	if err := emitResultJSON(&buf, map[string]any{"a": 1, "b": "two"}); err != nil {
		t.Fatalf("emitResultJSON: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected exactly one trailing newline, got %q", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(out, "\n")), &decoded); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
}

func TestEmitErrorText(t *testing.T) {
	var buf bytes.Buffer
	err := &CliError{Code: ExitUserError, Message: "bad thing", Remediation: "fix it"}
	if wErr := emitError(&buf, err, false); wErr != nil {
		t.Fatalf("emitError: %v", wErr)
	}
	want := "error: bad thing\nhint: fix it\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

func TestEmitErrorTextOmitsHintWhenRemediationEmpty(t *testing.T) {
	var buf bytes.Buffer
	err := &CliError{Code: ExitUserError, Message: "bad thing"}
	if wErr := emitError(&buf, err, false); wErr != nil {
		t.Fatalf("emitError: %v", wErr)
	}
	want := "error: bad thing\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q (no hint: line when Remediation is empty)", buf.String(), want)
	}
}

func TestEmitErrorJSON(t *testing.T) {
	var buf bytes.Buffer
	err := &CliError{Code: ExitEnvError, Message: "boom", Remediation: "retry"}
	if wErr := emitError(&buf, err, true); wErr != nil {
		t.Fatalf("emitError: %v", wErr)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected exactly one trailing newline, got %q", out)
	}
	var decoded struct {
		Code        int    `json:"code"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
	}
	if jErr := json.Unmarshal([]byte(strings.TrimSuffix(out, "\n")), &decoded); jErr != nil {
		t.Fatalf("invalid JSON %q: %v", out, jErr)
	}
	if decoded.Code != ExitEnvError || decoded.Message != "boom" || decoded.Remediation != "retry" {
		t.Fatalf("decoded = %+v, want code=%d message=boom remediation=retry", decoded, ExitEnvError)
	}
}

func TestEmitDiagnosticAddsTrailingNewline(t *testing.T) {
	var buf bytes.Buffer
	emitDiagnostic(&buf, "progress update")
	if buf.String() != "progress update\n" {
		t.Fatalf("got %q, want %q", buf.String(), "progress update\n")
	}
}
