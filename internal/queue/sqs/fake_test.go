package sqs_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	queuesqs "github.com/agentculture/culture-nodes/internal/queue/sqs"
)

// fakeSQS is a minimal in-process double for the four SQS Standard-queue
// operations Driver calls (SendMessage, ReceiveMessage, DeleteMessage,
// ChangeMessageVisibility), speaking the same AWS JSON 1.0 RPC protocol
// (POST /, header "X-Amz-Target: AmazonSQS.<Op>",
// "application/x-amz-json-1.0") the real aws-sdk-go-v2 SQS client sends, so
// the driver under test never has to know it is talking to a fake. It never
// checks SigV4 signatures -- Driver's static test credentials are
// syntactically valid but never verified.
//
// The chaos knobs (chaos field, set directly by each test before use) are
// deliberately simple, deterministic-given-a-seed, and independent of each
// other:
//
//   - duplicateProbability / forceDuplicateEveryN: when a ready message is
//     handed out by ReceiveMessage, a copy of it is (probabilistically, or
//     forced by position) left behind in the ready set so a later Receive
//     call sees it again -- simulating SQS's at-least-once redelivery. Each
//     message can be duplicated at most once (see fakeMessage.duplicated),
//     so a fully-forced run still terminates with a bounded number of
//     extra deliveries rather than duplicating forever.
//   - reorderWindow: ReceiveMessage picks the next message to deliver from
//     a random index within the first reorderWindow ready messages instead
//     of always the front -- a bounded "reorder buffer" that scrambles
//     local delivery order while still eventually delivering everything.
//   - dropSendFor: a set of message bodies (matched by substring, normally
//     a WorkID) whose SendMessage call fails outright (a simulated AWS
//     JSON error response) rather than enqueuing anything -- simulating a
//     publish that never reaches SQS at all.
type fakeSQS struct {
	mu       sync.Mutex
	nextID   int
	ready    []*fakeMessage
	inflight map[string]*fakeMessage // receipt handle -> message

	chaos  chaosConfig
	server *httptest.Server
}

type chaosConfig struct {
	duplicateProbability float64
	forceDuplicateEveryN int
	reorderWindow        int
	dropSendFor          map[string]bool
	rng                  *rand.Rand
}

type fakeMessage struct {
	id         string
	body       string
	duplicated bool
}

// newFakeSQS starts the fake server. Callers must call Close when done.
func newFakeSQS(t *testing.T) *fakeSQS {
	t.Helper()
	f := &fakeSQS{
		inflight: make(map[string]*fakeMessage),
		chaos:    chaosConfig{rng: rand.New(rand.NewSource(1))},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// driver builds a queue.Queue Driver pointed at this fake, with static
// credentials the fake never validates.
func (f *fakeSQS) driver(t *testing.T, cfg queuesqs.Config) *queuesqs.Driver {
	t.Helper()
	cfg.Endpoint = f.server.URL
	cfg.Region = "us-east-1"
	if cfg.QueueURL == "" {
		cfg.QueueURL = f.server.URL + "/000000000000/test-queue"
	}
	cfg.Credentials = credentials.NewStaticCredentialsProvider("fake-access-key", "fake-secret-key", "")
	cfg.HTTPClient = f.server.Client()

	d, err := queuesqs.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("queuesqs.New: %v", err)
	}
	return d
}

// setDropSendFor replaces the set of message-body substrings (normally
// WorkIDs) whose SendMessage call is forced to fail, guarded by f.mu so it
// is safe to call after requests have already been served (the drop chaos
// test lifts this mid-test, between two Relay.Run calls).
func (f *fakeSQS) setDropSendFor(needles ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := make(map[string]bool, len(needles))
	for _, n := range needles {
		m[n] = true
	}
	f.chaos.dropSendFor = m
}

// clearDropSendFor lifts every forced-drop chaos previously set.
func (f *fakeSQS) clearDropSendFor() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chaos.dropSendFor = nil
}

// injectRawMessage appends body directly to the ready set, bypassing
// SendMessage entirely -- used to put a message on the queue that Driver's
// own Publish would never write (an unrecognized schema version, or
// invalid JSON), simulating a message some other, incompatible producer
// wrote.
func (f *fakeSQS) injectRawMessage(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.ready = append(f.ready, &fakeMessage{id: fmt.Sprintf("msg-%d", f.nextID), body: body})
}

// handle dispatches on the AWS JSON 1.0 "X-Amz-Target: AmazonSQS.<Op>"
// header, exactly as the real SQS endpoint does.
func (f *fakeSQS) handle(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	op := strings.TrimPrefix(target, "AmazonSQS.")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.writeError(w, http.StatusBadRequest, "SerializationException", err.Error())
		return
	}

	switch op {
	case "SendMessage":
		f.handleSendMessage(w, body)
	case "ReceiveMessage":
		f.handleReceiveMessage(w, r, body)
	case "DeleteMessage":
		f.handleDeleteMessage(w, body)
	case "ChangeMessageVisibility":
		f.handleChangeMessageVisibility(w, body)
	default:
		f.writeError(w, http.StatusBadRequest, "UnknownOperationException", "fake sqs: unsupported operation "+op)
	}
}

func (f *fakeSQS) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeSQS) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.Header().Set("X-Amzn-ErrorType", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"__type":  code,
		"message": message,
	})
}

// --- SendMessage ---

type sendMessageRequest struct {
	MessageBody string `json:"MessageBody"`
}

func (f *fakeSQS) handleSendMessage(w http.ResponseWriter, body []byte) {
	var req sendMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeError(w, http.StatusBadRequest, "SerializationException", err.Error())
		return
	}

	f.mu.Lock()
	drop := false
	for needle := range f.chaos.dropSendFor {
		if strings.Contains(req.MessageBody, needle) {
			drop = true
			break
		}
	}
	f.mu.Unlock()

	if drop {
		// Simulated loss: SQS (real or otherwise) occasionally fails a
		// SendMessage outright. The caller (Driver.Publish, then
		// internal/events.Relay) is expected to treat this exactly like
		// any other publish failure -- see chaos_test.go's drop test.
		f.writeError(w, http.StatusInternalServerError, "InternalError", "fake sqs: simulated drop-on-send")
		return
	}

	f.mu.Lock()
	f.nextID++
	msg := &fakeMessage{id: fmt.Sprintf("msg-%d", f.nextID), body: req.MessageBody}
	f.ready = append(f.ready, msg)
	f.mu.Unlock()

	f.writeJSON(w, map[string]string{"MessageId": msg.id})
}

// --- ReceiveMessage ---

// fakePollInterval is how often a blocked ReceiveMessage rechecks for
// ready messages while long-polling -- an implementation detail of the
// fake's server-side blocking, not something Driver ever sees or depends
// on.
const fakePollInterval = 15 * time.Millisecond

type receiveMessageRequest struct {
	MaxNumberOfMessages int32 `json:"MaxNumberOfMessages"`
	WaitTimeSeconds     int32 `json:"WaitTimeSeconds"`
}

type wireMessage struct {
	MessageId     string `json:"MessageId"`
	ReceiptHandle string `json:"ReceiptHandle"`
	Body          string `json:"Body"`
}

// handleReceiveMessage implements real server-side long polling (blocking
// inside the HTTP handler, checking periodically, up to WaitTimeSeconds)
// rather than returning empty immediately -- matching real SQS closely
// enough that Driver.Receive's own wait-bounding logic (which assumes a
// single ReceiveMessage call can block) behaves the same against this fake
// as it would against real SQS.
func (f *fakeSQS) handleReceiveMessage(w http.ResponseWriter, r *http.Request, body []byte) {
	var req receiveMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeError(w, http.StatusBadRequest, "SerializationException", err.Error())
		return
	}
	max := int(req.MaxNumberOfMessages)
	if max <= 0 {
		max = 1
	}

	deadline := time.Now().Add(time.Duration(req.WaitTimeSeconds) * time.Second)
	for {
		if out := f.drainReady(max); len(out) > 0 {
			f.writeJSON(w, map[string]any{"Messages": out})
			return
		}
		if time.Now().After(deadline) {
			f.writeJSON(w, map[string]any{"Messages": []wireMessage{}})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(fakePollInterval):
		}
	}
}

// drainReady pops up to max ready messages, applying duplicate chaos and
// moving each into inflight under a freshly minted receipt handle.
func (f *fakeSQS) drainReady(max int) []wireMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []wireMessage
	for len(out) < max && len(f.ready) > 0 {
		msg := f.popReady()

		f.maybeDuplicate(msg)

		f.nextID++
		receipt := fmt.Sprintf("receipt-%d", f.nextID)
		f.inflight[receipt] = msg

		out = append(out, wireMessage{MessageId: msg.id, ReceiptHandle: receipt, Body: msg.body})
	}
	return out
}

// popReady removes and returns the next message to deliver. When
// chaos.reorderWindow > 0, it picks a random index within the first
// reorderWindow ready messages (a bounded reorder buffer) instead of always
// the front, so local delivery order can differ from publish order while
// every message is still eventually delivered.
func (f *fakeSQS) popReady() *fakeMessage {
	idx := 0
	if f.chaos.reorderWindow > 1 {
		window := f.chaos.reorderWindow
		if window > len(f.ready) {
			window = len(f.ready)
		}
		idx = f.chaos.rng.Intn(window)
	}
	msg := f.ready[idx]
	f.ready = append(f.ready[:idx], f.ready[idx+1:]...)
	return msg
}

// maybeDuplicate re-inserts a copy of msg into the ready set (to be
// delivered again later) when the configured chaos fires. Guarded by
// msg.duplicated so a single message is ever duplicated at most once,
// bounding total deliveries and guaranteeing test loops terminate even
// under "duplicate everything" chaos. Caller must hold f.mu.
func (f *fakeSQS) maybeDuplicate(msg *fakeMessage) {
	if msg.duplicated {
		return
	}

	dup := false
	if f.chaos.forceDuplicateEveryN > 0 && f.nextID%f.chaos.forceDuplicateEveryN == 0 {
		dup = true
	} else if f.chaos.duplicateProbability > 0 && f.chaos.rng.Float64() < f.chaos.duplicateProbability {
		dup = true
	}
	if !dup {
		return
	}

	msg.duplicated = true
	f.ready = append(f.ready, &fakeMessage{id: msg.id, body: msg.body, duplicated: true})
}

// --- DeleteMessage ---

type deleteMessageRequest struct {
	ReceiptHandle string `json:"ReceiptHandle"`
}

func (f *fakeSQS) handleDeleteMessage(w http.ResponseWriter, body []byte) {
	var req deleteMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeError(w, http.StatusBadRequest, "SerializationException", err.Error())
		return
	}

	f.mu.Lock()
	delete(f.inflight, req.ReceiptHandle)
	f.mu.Unlock()

	// Real SQS treats deleting an already-gone (or never-issued but
	// well-formed) receipt handle as a harmless success too -- see
	// Driver.Ack's doc comment. The fake matches that rather than
	// modeling ReceiptHandleIsInvalid, which only fires for a
	// malformed-not-just-stale handle.
	f.writeJSON(w, map[string]any{})
}

// --- ChangeMessageVisibility ---

type changeMessageVisibilityRequest struct {
	ReceiptHandle     string `json:"ReceiptHandle"`
	VisibilityTimeout int32  `json:"VisibilityTimeout"`
}

func (f *fakeSQS) handleChangeMessageVisibility(w http.ResponseWriter, body []byte) {
	var req changeMessageVisibilityRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeError(w, http.StatusBadRequest, "SerializationException", err.Error())
		return
	}

	f.mu.Lock()
	msg, ok := f.inflight[req.ReceiptHandle]
	if ok && req.VisibilityTimeout == 0 {
		// Immediately visible again: return it to the ready set under the
		// same receipt bookkeeping semantics as SQS (a fresh receipt will
		// be minted on the next receive).
		delete(f.inflight, req.ReceiptHandle)
		f.ready = append(f.ready, msg)
	}
	f.mu.Unlock()

	f.writeJSON(w, map[string]any{})
}
