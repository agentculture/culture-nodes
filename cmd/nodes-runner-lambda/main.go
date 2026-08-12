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
// signal/message. Kept as a local mirror, deliberately: sharing a struct
// with the adapter would make this binary import the adapter's package and
// so link the AWS SDK into the one process built to carry none of it — the
// JSON tags are the contract, not the type, and a contract change must
// touch both sides on purpose.
//
// Signal carries os/exec's ProcessState.String() verbatim (e.g. "signal:
// killed"), not a bare signal name — the adapter stores it as
// process-reported content without parsing it, so verbatim is honest.
type report struct {
	ExitCode *int   `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	Message  string `json:"message,omitempty"`
}

// refusalError is a refusal or failure that must answer the invocation via
// the runtime error endpoint, typed so a policy refusal ("this runner will
// not do that") never masquerades as a runtime failure ("the argv could not
// start") in the adapter's error message.
type refusalError struct {
	kind string // the errorType the runtime error document carries
	err  error
}

func (r *refusalError) Error() string { return r.err.Error() }

func refuse(err error) *refusalError    { return &refusalError{kind: "OperationRefused", err: err} }
func runnerErr(err error) *refusalError { return &refusalError{kind: "RunnerError", err: err} }

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

	// Capped backoff on consecutive loop errors, reset on any success: a
	// persistently failing runtime API must not become a hot spin that
	// burns CPU and floods the log (review finding). The cap stays low
	// because the platform, not this loop, owns liveness — it kills the
	// sandbox if the loop is truly wedged.
	backoff := time.Duration(0)
	for {
		if err := handleOne(client, base); err != nil {
			// A failure to even fetch or answer an invocation is a loop
			// error, not an invocation result. Log, back off, keep serving.
			log.Printf("runtime loop: %v", err)
			if backoff < 100*time.Millisecond {
				backoff = 100 * time.Millisecond
			} else if backoff *= 2; backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			time.Sleep(backoff)
			continue
		}
		backoff = 0
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
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("invocation/next: status %d: %s",
			resp.StatusCode, truncate(payload, 512))
	}
	requestID := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
	if requestID == "" {
		return errors.New("invocation/next returned no Lambda-Runtime-Aws-Request-Id")
	}

	rep, refusal := run(payload, deadlineFrom(resp.Header))
	if refusal != nil {
		return post(client, base+"/invocation/"+requestID+"/error", invocationError{
			ErrorType:    refusal.kind,
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
// response, or a typed refusal/failure to POST as an invocation error —
// never both.
func run(payload []byte, deadline time.Time) (*report, *refusalError) {
	var op runners.Operation
	if err := json.Unmarshal(payload, &op); err != nil {
		return nil, refuse(fmt.Errorf("payload is not a runner operation: %v", err))
	}

	// Refuse what this runner cannot honour. Silence here would be the
	// exact fabrication the operation schema forbids: an ignored request
	// looks identical to an honoured one from the caller's side.
	if op.Workspace != nil {
		return nil, refuse(errors.New("operation carries a workspace; this runner does not materialise workspaces yet and will not pretend it ran against one"))
	}
	if len(op.Command.EnvironmentRefs) > 0 {
		return nil, refuse(fmt.Errorf("operation grants %d environment_refs; no environment store is wired to resolve them", len(op.Command.EnvironmentRefs)))
	}
	if op.Command.RequiresShell != nil && *op.Command.RequiresShell {
		return nil, refuse(errors.New("operation requires a shell; argv execution is the only mode this runner offers"))
	}
	if len(op.Command.Argv) == 0 {
		return nil, refuse(errors.New("operation has an empty argv"))
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
		return nil, runnerErr(errors.New("no time remains to run the operation inside this invocation"))
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
		// The process never started (argv[0] not found, permission, ...) —
		// a runner-side failure, not a policy refusal, and typed as such.
		return nil, runnerErr(fmt.Errorf("argv did not start: %v", err))
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
	// One short-backoff retry: an unanswered invocation hangs until the
	// platform times it out, so a single transient POST failure is worth
	// one more attempt before surrendering the invocation to that fate.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		lastErr = postOnce(client, url, encoded)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func postOnce(client *http.Client, url string, encoded []byte) error {
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
// process holding an unbounded stream in memory — which means the bound
// must hold for the backing array, not just the slice length: an
// append-then-reslice implementation would retain capacity proportional to
// the largest single write ever seen (review finding). The buffer is
// allocated once at cap=limit and never grows.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.limit <= 0 || n == 0 {
		return n, nil
	}
	if t.buf == nil {
		t.buf = make([]byte, 0, t.limit)
	}
	if n >= t.limit {
		// The write alone fills the window: keep exactly its last limit
		// bytes, never materialising the rest.
		t.buf = t.buf[:t.limit]
		copy(t.buf, p[n-t.limit:])
		return n, nil
	}
	if overflow := len(t.buf) + n - t.limit; overflow > 0 {
		copy(t.buf, t.buf[overflow:])
		t.buf = t.buf[:len(t.buf)-overflow]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// truncate bounds an error-message body snippet.
func truncate(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "…"
}
