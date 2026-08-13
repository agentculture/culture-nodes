package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// apiErrorBody mirrors internal/clifmt.CliError's JSON encoding — every
// non-2xx response's documented shape (see api/openapi/openapi.yaml's Error
// schema and internal/api's package doc).
type apiErrorBody struct {
	Code        int    `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// doJSON sends a request with an optional JSON body and decodes the
// response body into out (skipped if out is nil or the body is empty). It
// returns the raw response and body bytes so a caller can additionally
// assert on status or headers.
func doJSON(t *testing.T, client *http.Client, method, url string, body, out any) (*http.Response, []byte) {
	t.Helper()
	return doJSONBearer(t, client, method, url, "", body, out)
}

// doJSONBearer is doJSON with an Authorization: Bearer header — for the
// mutating routes the t15 auth-hardening gate closed (empty token sends no
// header, keeping doJSON's authless shape for everything else).
func doJSONBearer(t *testing.T, client *http.Client, method, url, token string, body, out any) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read response body: %v", method, url, err)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode response %s: %v", method, url, data, err)
		}
	}
	return resp, data
}

// requireStatus fails the test with the response body attached if got does
// not match want — every failure here is far easier to debug with the
// actual error body in hand than with just a status code mismatch.
func requireStatus(t *testing.T, resp *http.Response, body []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status = %d, want %d; body = %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, want, body)
	}
}

// decodeAPIError decodes body as the documented Error shape, failing the
// test if it does not match — this is the "every error response matches
// the documented shape" contract check used throughout this package's
// tests, not only in contract_test.go's exhaustive sweep.
func decodeAPIError(t *testing.T, body []byte) apiErrorBody {
	t.Helper()
	var apiErr apiErrorBody
	if err := json.Unmarshal(body, &apiErr); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if apiErr.Message == "" {
		t.Fatalf("error body %s has an empty message", body)
	}
	if apiErr.Remediation == "" {
		t.Fatalf("error body %s has an empty remediation", body)
	}
	if apiErr.Code != 1 && apiErr.Code != 2 {
		t.Fatalf("error body %s has code %d, want 1 or 2", body, apiErr.Code)
	}
	return apiErr
}

// sseEvent is one parsed "id: / event: / data:" frame.
type sseEvent struct {
	ID   int64
	Type string
	Data []byte
}

// openSSE opens the events stream for runID, optionally resuming from
// lastEventID (empty means from the beginning), and fails the test if the
// server does not answer 200.
func (f *fixture) openSSE(t *testing.T, runID, lastEventID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.url("/v1alpha1/runs/"+runID+"/events"), nil)
	if err != nil {
		t.Fatalf("new SSE request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream for run %s: %v", runID, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("open SSE stream for run %s: status %d: %s", runID, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("open SSE stream for run %s: content-type = %q, want text/event-stream", runID, ct)
	}
	return resp
}

// streamSSEEvents parses body as a stream of SSE frames onto ch, closing ch
// when the stream ends (server close, or the caller closing body). It is
// meant to run in its own goroutine, started before whatever action the
// test expects to produce more events.
func streamSSEEvents(body io.Reader, ch chan<- sseEvent) {
	defer close(ch)
	r := bufio.NewReader(body)
	var cur sseEvent
	haveEvent := false
	for {
		line, err := r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			if haveEvent {
				ch <- cur
				cur = sseEvent{}
				haveEvent = false
			}
		case strings.HasPrefix(trimmed, "id: "):
			id, _ := strconv.ParseInt(strings.TrimPrefix(trimmed, "id: "), 10, 64)
			cur.ID = id
			haveEvent = true
		case strings.HasPrefix(trimmed, "event: "):
			cur.Type = strings.TrimPrefix(trimmed, "event: ")
			haveEvent = true
		case strings.HasPrefix(trimmed, "data: "):
			cur.Data = []byte(strings.TrimPrefix(trimmed, "data: "))
			haveEvent = true
		}
		if err != nil {
			return
		}
	}
}

// drainClosed reads ch until it closes (the stream ended), or fails the
// test after timeout — used once a test expects no more events to matter.
func drainClosed(t *testing.T, ch <-chan sseEvent, timeout time.Duration) []sseEvent {
	t.Helper()
	var got []sseEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %s waiting for the SSE stream to close; got %d events so far: %+v", timeout, len(got), got)
			return got
		}
	}
}
