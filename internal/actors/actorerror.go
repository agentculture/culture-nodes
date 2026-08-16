package actors

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The actor's own account of a refusal (task t3, issue #125).
//
// Before this, a refused dispatch recorded a status line and nothing else.
// Run 01M04Q26ZPNKTXVNBGSDS1YR9F's attempt said
//
//	actor_rejected_input (HTTP 400): actor answered Bad Request
//
// while the bridge's response body said what was actually wrong:
//
//	{"error":"input.instruction is required and must be a non-empty string",
//	 "class":"actor_rejected_input"}
//
// "Bad Request" is the name of a status code, not a reason, so diagnosing the
// refusal meant reproducing the call by hand against the bridge. This file
// lifts the bridge's own words out of that body so the attempt records them.
//
// Three properties make that safe to do with a body this process did not
// write:
//
//  1. It is BOUNDED. An actor is entitled to return a large error document;
//     it is not entitled to write an unbounded amount of text into an attempt
//     result an operator reads and internal/events carries.
//  2. It is SANITIZED. Diagnostic text is data, not markup and not terminal
//     control. A bridge relaying a subprocess's stderr can easily include ANSI
//     escapes; those reach an operator's terminal through `nodes run <id>` and
//     a browser through the run view, and neither should be steerable by the
//     thing that just failed.
//  3. It is kept SEPARATE from the engine's own classification. See
//     ActorError.Class.

// Bounds on what an actor's error body may contribute to an attempt result.
//
// MaxActorMessageBytes is 2048 for the same reason maxCapturedBodyBytes is:
// that is this package's existing, already-justified answer to "how much of
// an actor's response body may we carry into a diagnostic", and answering the
// same question twice with two different numbers would be a bug waiting to
// happen. It is deliberately three orders of magnitude below the 60000-char
// cap the adapters use when truncating a bound-inputs block into a workspace
// file (adapters/*/src/*/server.py): that bound guards one file on one host,
// this one guards a row that is written on EVERY failed attempt, is read back
// by the run view, and rides into an event payload — so the budget per copy
// has to be small. A real bridge rejection is tens of bytes ("input.repo is
// required" is 24), so 2048 is ~80x the observed need and still bounded.
//
// MaxActorClassBytes is 64 because a class is an identifier, not prose: the
// longest §13.5 class name is "retryable_transport" at 19 characters, and a
// bridge sending anything near 64 is already telling us its "class" field is
// not a class.
const (
	MaxActorMessageBytes = 2048
	MaxActorClassBytes   = 64
)

// ActorError is what the actor ITSELF said about a refusal, lifted out of a
// non-2xx response body and carried onto the attempt result so a refused
// dispatch can be diagnosed from GET /v1alpha1/runs/{id} alone.
//
// Every field here is the actor's text, not this control plane's. It is
// recorded as a report, the same way §10.4 records an agent's completion
// claim as a claim: useful, attributable, and never promoted to a fact the
// engine acts on.
type ActorError struct {
	// Message is the actor's own `error` string, sanitized and bounded. It
	// is empty when the body carried no `error` string — see Body.
	Message string `json:"message,omitempty"`
	// Class is the actor's `class` CLAIM, recorded verbatim (sanitized and
	// bounded), and it is NOT the engine's classification of the failure.
	//
	// The two are separate facts and are kept in separate fields on purpose.
	// The engine's class is derived by classifyStatus from the HTTP status
	// the actor returned, and is trusted precisely because the actor cannot
	// choose it: §13.5's classes drive retry policy and the §3.4 technical
	// status, so a bridge that could name its own class could talk itself out
	// of policy_denied or into an infinite retry. classifyBody is the single,
	// narrowly-argued exception (capacity_exhausted, which no status code
	// distinguishes); everything else stays status-derived.
	//
	// So the engine does not adopt this value — it records it. When the two
	// disagree, that disagreement is itself the diagnostic: a bridge claiming
	// "actor_rejected_input" on a 500 is a bridge with a status-code bug, and
	// collapsing the two fields into one would hide exactly that.
	Class string `json:"class,omitempty"`
	// Body is a bounded, sanitized snippet of a response body that declared
	// no `error` string at all — a proxy's HTML error page, a body cut off
	// mid-write, plain text, a JSON object using some other key. It is a
	// separate field from Message so a reader can always tell "the bridge
	// said this" from "the bridge said something this control plane could not
	// parse, and here is the start of it". It is empty whenever Message is
	// set.
	Body string `json:"body,omitempty"`
	// Truncated reports that Message, Class, or Body was cut to its bound.
	// The strings themselves carry no ellipsis, so a consumer measuring one
	// against its bound measures the real thing.
	Truncated bool `json:"truncated,omitempty"`
}

// actorErrorFrom reads an actor's own account of a refusal out of a non-2xx
// response body, returning nil when the body says nothing at all (the empty
// body a bare `w.WriteHeader(400)` produces).
//
// It reads the full payload rather than the truncated Body capture, for the
// same reason telemetryFromErrorBody does: a large error document must not
// cost the attempt its diagnostic. It applies its own bound afterwards.
//
// Nothing here can fail: an unparseable body is a normal outcome, not an
// error, and a refusal whose body cannot be read is still a refusal that must
// be recorded.
func actorErrorFrom(payload []byte) *ActorError {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}

	var declared struct {
		// Error is decoded as raw JSON rather than as a string so a body
		// whose `error` is an object or an array is treated as unstructured
		// (and snipped below) instead of failing the whole decode and losing
		// the `class` beside it.
		Error json.RawMessage `json:"error"`
		Class string          `json:"class"`
	}
	if err := json.Unmarshal(payload, &declared); err == nil {
		var message string
		if len(declared.Error) > 0 {
			// A non-string `error` leaves message empty on purpose; the
			// snippet path below then records the body as what it is.
			_ = json.Unmarshal(declared.Error, &message)
		}
		text, textCut := sanitizeDiagnostic(message, MaxActorMessageBytes)
		class, classCut := sanitizeDiagnostic(declared.Class, MaxActorClassBytes)
		if text != "" || class != "" {
			return &ActorError{Message: text, Class: class, Truncated: textCut || classCut}
		}
	}

	snippet, cut := sanitizeDiagnostic(string(payload), MaxActorMessageBytes)
	if snippet == "" {
		return nil
	}
	return &ActorError{Body: snippet, Truncated: cut}
}

// sanitizeDiagnostic makes one string from an actor safe to record and bounds
// it, reporting whether it had to be cut.
//
// Three things happen, in order:
//
//   - Invalid UTF-8 becomes U+FFFD. A []byte off the wire is not a Go string
//     just because it was assigned to one, and PostgreSQL rejects invalid
//     UTF-8 in a text/jsonb value outright — an unsanitized byte would turn a
//     recorded diagnostic into a failed write.
//   - Every C0 control, DEL, and C1 control becomes a space. That includes
//     ESC (so an ANSI colour sequence relayed from a subprocess cannot repaint
//     an operator's terminal), BEL, and newlines — this field is a one-line
//     diagnostic, and a multi-line body's structure is not what a reader needs
//     from it.
//   - The result is trimmed and cut to maxBytes on a rune boundary, so the
//     bound holds in bytes and the string stays valid UTF-8.
//
// No ellipsis is appended: ActorError.Truncated says it was cut, and a
// caller measuring the string against its bound should measure the string.
func sanitizeDiagnostic(s string, maxBytes int) (string, bool) {
	if s == "" {
		return "", false
	}
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRightFunc(s[:cut], unicode.IsSpace), true
}
