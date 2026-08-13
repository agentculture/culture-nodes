package notifier

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Frame is one parsed Server-Sent Event: the id/event/data triple
// GET {base}/v1alpha1/events writes per committed event (see
// writeCrossRunSSEEvent in internal/api/events.go -- "id: <ulid>\nevent:
// <type>\ndata: <envelope JSON>\n\n"). Frame carries the raw data bytes
// (the JSON-encoded events.Envelope), not a parsed Envelope: parsing is
// the caller's job, so a frame this package cannot make sense of (an id
// with no matching frame content, say) can still be reported for the
// malformed-event tolerance this daemon promises.
type Frame struct {
	ID   string
	Type string
	Data []byte
}

// openStream issues one GET against {apiBase}/v1alpha1/events, scoped by
// runs (nil/empty means the server's own active-run+lifecycle default) and
// resumed from afterID (empty means from the start of the stream). It sets
// both the "?from=" query parameter and the Last-Event-ID header to the
// same value -- the server accepts either (resumeEventID in
// internal/api/events.go prefers the header, falling back to the query
// parameter), and setting both means this client's behavior does not
// depend on which one a future server revision prefers.
//
// The returned response's Body is the caller's to close; openStream
// returns an error (never a non-2xx response) for anything short of a
// successful, streaming connection.
func openStream(ctx context.Context, client *http.Client, apiBase string, runs []string, afterID string) (*http.Response, error) {
	u, err := url.Parse(strings.TrimRight(apiBase, "/") + "/v1alpha1/events")
	if err != nil {
		return nil, fmt.Errorf("notifier: parse api base %q: %w", apiBase, err)
	}
	q := u.Query()
	if afterID != "" {
		q.Set("from", afterID)
	}
	if len(runs) > 0 {
		q.Set("runs", strings.Join(runs, ","))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("notifier: build events request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if afterID != "" {
		req.Header.Set("Last-Event-ID", afterID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notifier: connect to %s: %w", u.String(), err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("notifier: GET %s: status %d: %s", u.String(), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// readFrames scans body for SSE frames ("id: "/"event: "/"data: " lines
// terminated by a blank line -- the exact shape handleStreamEvents writes,
// mirrored here rather than pulled from a third-party SSE client library
// since this daemon has exactly one server to talk to and the framing is a
// few lines of code) and calls handle for each complete one, in order.
//
// readFrames returns nil on a clean end of stream (the server closed the
// connection, or ctx was cancelled) and a non-nil error only for a
// scanner failure (e.g. a frame exceeding the buffer cap) -- either way
// the caller (streamOnce) treats returning as "reconnect", so this
// function's only job is to keep calling handle for as long as frames
// keep arriving.
//
// A frame with no id (a malformed or heartbeat-only frame) is passed to
// handle with FrameID == "" rather than dropped silently -- callers that
// care about malformed-event tolerance decide there, in one place, rather
// than this parser guessing what "malformed" should mean for every caller.
func readFrames(ctx context.Context, body io.Reader, handle func(Frame) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var current Frame
	var haveFrame bool
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			current.ID = strings.TrimPrefix(line, "id: ")
			haveFrame = true
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
			haveFrame = true
		case strings.HasPrefix(line, "data: "):
			current.Data = []byte(strings.TrimPrefix(line, "data: "))
			haveFrame = true
		case line == "":
			if !haveFrame {
				continue // a blank keep-alive line between frames
			}
			if err := handle(current); err != nil {
				return err
			}
			current, haveFrame = Frame{}, false
		}
	}
	if err := ctx.Err(); err != nil {
		return nil // caller asked us to stop; not a stream failure
	}
	return scanner.Err() // nil on clean EOF, non-nil on a real scan failure -- either way the caller reconnects
}
