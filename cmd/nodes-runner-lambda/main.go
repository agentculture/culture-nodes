// Command nodes-runner-lambda is the function side of the Lambda runner
// lane: the process that runs inside the culture-nodes runner container
// image, receives one typed runner operation per invocation, executes its
// argv, and reports what the process said about itself.
//
// It implements the Lambda custom-runtime interface directly (the
// long-poll loop against AWS_LAMBDA_RUNTIME_API) rather than pulling in
// github.com/aws/aws-lambda-go: the loop is four HTTP calls, and
// internal/runners/lambda's doc.go names internal/runners/lambda,
// internal/queue/sqs, and internal/artifacts/s3 as the only packages that
// may import an AWS SDK (spec claim c17) — this binary imports none.
//
// The trust posture mirrors the adapter's (internal/runners/lambda): this
// process is the function, so everything it returns is process-reported
// content. It reports the exit status of the argv it ran because it waited
// on that process itself; it claims nothing about workspaces, artifacts, or
// the truth of anything the argv printed. The adapter turns the payload
// into a Result and the evidence mapping keeps exit_status
// {measured:false} on this platform — see the adapter's doc for why.
//
// What this minimal runner refuses, loudly, per invocation:
//
//   - an operation carrying a workspace (workspace materialisation from S3
//     refs is not implemented yet — accepting one and ignoring it would be
//     the silent-drop the operation schema forbids);
//   - environment_refs it cannot resolve (no environment store is wired);
//   - requires_shell — argv is an argument array, never a shell string.
//
// A refusal is posted to the runtime error endpoint, which surfaces as
// FunctionError on the invoke response and maps to a failed, non-retryable
// result adapter-side — a typed "no", never a fabricated "ran fine".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

const apiVersion = "2018-06-01"

// messageTailBytes bounds the combined-output tail carried in the report's
// message field on a non-zero exit. The full stream still goes to
// stdout/stderr (CloudWatch); the tail is a convenience, not the log
// evidence — the adapter's log observation comes from Lambda's LogResult.
const messageTailBytes = 1024

// report mirrors the payload contract internal/runners/lambda's
// interpretPayload reads: a JSON object carrying exit_code and optional
// signal/message. Kept as a local mirror because the adapter's struct is
// deliberately unexported — the JSON tags are the contract, not the type.
type report struct {
	ExitCode *int   `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	Message  string `json:"message,omitempty"`
}

func main() {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		log.Fatal("AWS_LAMBDA_RUNTIME_API is not set; this binary only runs inside the Lambda runtime")
	}
	base := "http://" + api + "/" + apiVersion + "/runtime"

	client := &http.Client{
		// The next-invocation call long-polls with no payload until work
		// arrives; a client timeout would turn idle waiting into an error.
		Timeout: 0,
	}

	for {
		if err := handleOne(client, base); err != nil {
			// A failure to even fetch or answer an invocation is a loop
			// error, not an invocation result. Log and keep serving; the
			// runtime kills the sandbox if the loop is truly wedged.
			log.Printf("runtime loop: %v", err)
		}
	}
}

// handleOne fetches the next invocation, runs it, and posts either a report
// or an invocation error. Exactly one POST answers each GET.
func handleOne(client *http.Client, base string) error {
	resp, err := client.Get(base + "/invocation/next")
	if err != nil {
		return fmt.Errorf("invocation/next: %w", err)
	}
	payload, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading invocation payload: %w", err)
	}
	requestID := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
	if requestID == "" {
		return errors.New("invocation/next returned no Lambda-Runtime-Aws-Request-Id")
	}

	rep, refusal := run(payload, deadlineFrom(resp.Header))
	if refusal != nil {
		return post(client, base+"/invocation/"+requestID+"/error", invocationError{
			ErrorType:    "OperationRefused",
			ErrorMessage: refusal.Error(),
		})
	}
	return post(client, base+"/invocation/"+requestID+"/response", rep)
}

// deadlineFrom reads the runtime's per-invocation deadline header. A missing
// or unreadable header yields a generous fallback rather than a guess of
// zero — the function-level timeout still bounds everything.
func deadlineFrom(h http.Header) time.Time {
	ms, err := strconv.ParseInt(h.Get("Lambda-Runtime-Deadline-Ms"), 10, 64)
	if err != nil {
		return time.Now().Add(15 * time.Minute)
	}
	return time.UnixMilli(ms)
}

// run executes one operation. It returns either a report to POST as the
// response, or a refusal to POST as an invocation error — never both.
func run(payload []byte, deadline time.Time) (*report, error) {
	var op runners.Operation
	if err := json.Unmarshal(payload, &op); err != nil {
		return nil, fmt.Errorf("payload is not a runner operation: %v", err)
	}

	// Refuse what this runner cannot honour. Silence here would be the
	// exact fabrication the operation schema forbids: an ignored request
	// looks identical to an honoured one from the caller's side.
	if op.Workspace != nil {
		return nil, errors.New("operation carries a workspace; this runner does not materialise workspaces yet and will not pretend it ran against one")
	}
	if len(op.Command.EnvironmentRefs) > 0 {
		return nil, fmt.Errorf("operation grants %d environment_refs; no environment store is wired to resolve them", len(op.Command.EnvironmentRefs))
	}
	if op.Command.RequiresShell != nil && *op.Command.RequiresShell {
		return nil, errors.New("operation requires a shell; argv execution is the only mode this runner offers")
	}
	if len(op.Command.Argv) == 0 {
		return nil, errors.New("operation has an empty argv")
	}

	// The effective timeout is the tighter of the operation's policy and
	// Lambda's own remaining time (less a small margin so the report is
	// posted by us, not preempted by the platform's timeout error).
	ctx := context.Background()
	limit := time.Until(deadline) - 2*time.Second
	if s := op.Policy.TimeoutSeconds; s > 0 && time.Duration(s)*time.Second < limit {
		limit = time.Duration(s) * time.Second
	}
	if limit <= 0 {
		return nil, errors.New("no time remains to run the operation inside this invocation")
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	tail := &tailBuffer{limit: messageTailBytes}
	cmd := exec.CommandContext(ctx, op.Command.Argv[0], op.Command.Argv[1:]...) // #nosec G204 -- executing the operation's argv is this binary's entire purpose
	cmd.Dir = op.Command.WorkingDirectory
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	err := cmd.Run()
	switch {
	case err == nil:
		code := 0
		return &report{ExitCode: &code}, nil
	case cmd.ProcessState != nil && cmd.ProcessState.Exited():
		code := cmd.ProcessState.ExitCode()
		return &report{ExitCode: &code, Message: tail.String()}, nil
	case cmd.ProcessState != nil:
		// Killed by a signal (including our own timeout's SIGKILL). The
		// convention for a signal death's exit code is 128+signal, and the
		// signal is named so the caller does not have to decode the code.
		code := cmd.ProcessState.ExitCode()
		return &report{
			ExitCode: &code,
			Signal:   cmd.ProcessState.String(),
			Message:  tail.String(),
		}, nil
	default:
		// The process never started (argv[0] not found, permission, ...).
		return nil, fmt.Errorf("argv did not start: %v", err)
	}
}

// invocationError is the runtime error document shape
// (lambdaErrorPayload on the adapter side).
type invocationError struct {
	ErrorType    string `json:"errorType"`
	ErrorMessage string `json:"errorMessage"`
}

func post(client *http.Client, url string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, msg)
	}
	return nil
}

// tailBuffer keeps the last limit bytes written to it. It exists so a
// failing command's report can carry the end of its output without this
// process holding an unbounded stream in memory.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
