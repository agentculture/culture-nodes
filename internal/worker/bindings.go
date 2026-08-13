package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Input binding resolution (PRD §11.2).
//
// §11.2 is emphatic about what this is and is not: "use JSON Pointer for
// direct data movement… do not invent a template language for field
// interpolation." So a binding is an RFC 6901 pointer, resolution is a read,
// and there is no expression evaluation anywhere in this file. A binding that
// does not resolve is an error, never a silently-empty value — an actor
// handed `{}` where it expected the run input would fail in a much more
// confusing place than here.
//
// # The four surfaces, and what each actually reads
//
//	/run/input                        the run's immutable input document
//	/nodes/<id>/output                the output of that node's most recent
//	                                  SUCCEEDED attempt in this run. A failed
//	                                  or contract-rejected attempt produced no
//	                                  answer a binding may treat as the node's
//	                                  output, so it is not visible here.
//	/nodes/<id>/evidence              the run's live evidence records selected
//	                                  by that node's node runs, as a JSON
//	                                  array in append (id) order. Evidence
//	                                  identity is the node run: the engine
//	                                  stamps node_run_id on every accepted
//	                                  delta record (internal/engine/
//	                                  ledgerdelta.go), and node evidence
//	                                  carries no SubjectRef. A node that has
//	                                  appended none resolves to [] — unlike a
//	                                  missing output, "zero evidence records"
//	                                  is itself the true answer, not an
//	                                  absence a caller could mistake for one.
//	/ledger/projections/<name>        a §10.9 projection over this run's
//	                                  ledger records, computed on read.
//
// Any pointer may address deeper into the resolved value: /run/input/subject
// reads the run input and then walks to its `subject` member.
//
// # What is deliberately not resolvable
//
// /nodes/<id>/artifacts and /nodes/<id>/error need the artifact router and
// the error payload surface. The compiler rejects a data binding naming them
// (internal/compiler/contract.go's deferredNodeBindingSurfaces), and this
// resolver refuses them too — the verdicts must agree from both sides, so a
// published workflow can never carry a binding that only fails at dispatch.

// bindingSources is where a resolver reads from. It is an interface-free
// struct of closures because the three surfaces have nothing in common to
// abstract over — one is a field, one is a query, one is a computation.
type bindingSources struct {
	RunID    string
	RunInput json.RawMessage
	// NodeOutput returns a node's most recent succeeded output, or nil when
	// the node has not produced one in this run.
	NodeOutput func(ctx context.Context, nodeID string) (json.RawMessage, error)
	// NodeEvidence returns the run's live evidence records belonging to a
	// node's node runs, in id order. Zero records is an answer, not an error.
	NodeEvidence func(ctx context.Context, nodeID string) ([]ledger.Record, error)
	// Projection computes a §10.9 projection over the run's ledger records.
	Projection func(ctx context.Context, kind ledger.ProjectionKind, subject string) (ledger.Projection, error)
}

// resolveNodeInput builds a node's dispatch payload from its declared binding
// (§11.2).
//
// A node with no declared binding gets `{}`, not the run input. Inheriting
// the run input by default would make every actor's contract implicitly
// depend on the workflow's, which is exactly the coupling explicit bindings
// exist to remove — and the PRD's own §11.1 example declares a binding on
// every node that needs one.
func resolveNodeInput(ctx context.Context, src bindingSources, binding *inputBinding) (json.RawMessage, error) {
	if !binding.declared() {
		return json.RawMessage(`{}`), nil
	}
	if binding.From != "" {
		return resolvePointer(ctx, src, binding.From)
	}

	// A `bindings` map becomes an object with the same keys. Keys are
	// resolved in sorted order so a failure reports the same binding every
	// time rather than whichever one the map iteration happened to reach.
	names := make([]string, 0, len(binding.Bindings))
	for name := range binding.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)

	members := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		value, err := resolvePointer(ctx, src, binding.Bindings[name])
		if err != nil {
			return nil, fmt.Errorf("binding %q: %w", name, err)
		}
		members[name] = value
	}
	encoded, err := json.Marshal(members)
	if err != nil {
		return nil, fmt.Errorf("bindings could not be encoded: %w", err)
	}
	return encoded, nil
}

// resolvableSurfaces is the error message's list of what this resolver can
// answer. It is a constant so the message never drifts from the switch below.
const resolvableSurfaces = "/run/input, /nodes/<node>/output, /nodes/<node>/evidence, /ledger/projections/<name>"

func resolvePointer(ctx context.Context, src bindingSources, pointer string) (json.RawMessage, error) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("the empty pointer addresses the whole document; bind one of %s", resolvableSurfaces)
	}

	var base json.RawMessage
	var rest []string

	switch tokens[0] {
	case "run":
		if len(tokens) < 2 || tokens[1] != "input" {
			return nil, fmt.Errorf("the only run surface is /run/input")
		}
		base, rest = jsonOrNull(src.RunInput), tokens[2:]

	case "nodes":
		if len(tokens) < 3 {
			return nil, fmt.Errorf("a node binding names a node and a surface, e.g. /nodes/<node>/output")
		}
		switch tokens[2] {
		case "output":
			if src.NodeOutput == nil {
				return nil, fmt.Errorf("no node-output source is configured")
			}
			output, err := src.NodeOutput(ctx, tokens[1])
			if err != nil {
				return nil, err
			}
			if output == nil {
				return nil, fmt.Errorf("node %q has no succeeded attempt in this run, so it has no output", tokens[1])
			}
			base, rest = output, tokens[3:]
		case "evidence":
			if src.NodeEvidence == nil {
				return nil, fmt.Errorf("no node-evidence source is configured")
			}
			records, err := src.NodeEvidence(ctx, tokens[1])
			if err != nil {
				return nil, err
			}
			if records == nil {
				records = []ledger.Record{}
			}
			encoded, err := json.Marshal(records)
			if err != nil {
				return nil, fmt.Errorf("evidence of node %q could not be encoded: %w", tokens[1], err)
			}
			base, rest = encoded, tokens[3:]
		default:
			return nil, fmt.Errorf(
				"surface /nodes/<node>/%s is not resolvable; this worker resolves %s",
				tokens[2], resolvableSurfaces)
		}

	case "ledger":
		if len(tokens) < 3 || tokens[1] != "projections" {
			return nil, fmt.Errorf("the only ledger surface is /ledger/projections/<name>")
		}
		if src.Projection == nil {
			return nil, fmt.Errorf("no ledger projection source is configured")
		}
		kind, subject, ok := projectionKindFor(tokens[2])
		if !ok {
			return nil, fmt.Errorf("projection %q is not one of PRD §10.9's projections", tokens[2])
		}
		projection, err := src.Projection(ctx, kind, subject)
		if err != nil {
			return nil, fmt.Errorf("projection %s: %w", tokens[2], err)
		}
		encoded, err := json.Marshal(projection)
		if err != nil {
			return nil, fmt.Errorf("projection %s could not be encoded: %w", tokens[2], err)
		}
		base, rest = encoded, tokens[3:]

	default:
		return nil, fmt.Errorf("binding root %q is not resolvable; this worker resolves %s", tokens[0], resolvableSurfaces)
	}

	return traverse(base, rest)
}

// projectionKindFor maps a binding's projection name onto a ledger projection.
//
// The two vocabularies are not identical, and the mismatch is real rather
// than a mistake to paper over. The compiler's accepted names come from PRD
// §10.9's prose list; internal/ledger's kinds come from the functions that
// compute them, and two of those cover more than one listed name:
//
//   - `open_assumptions` and `open_questions` are both answered by the single
//     open-assumptions-and-questions projection, which selects both record
//     types. Binding either name gets that projection.
//   - `evidence` maps to the evidence projection with an empty subject,
//     which ledger.EvidenceForSubject reads as unscoped: it selects all of
//     the run's live evidence records rather than one reference's.
//
// A name outside the list is refused rather than guessed at: §10.9's
// vocabulary is closed on purpose, so a typo should fail loudly rather than
// silently bind an actor to an empty view.
func projectionKindFor(name string) (kind ledger.ProjectionKind, subject string, ok bool) {
	switch name {
	case "current_scope":
		return ledger.KindCurrentScope, "", true
	case "confirmed_claims":
		return ledger.KindConfirmedClaims, "", true
	case "open_assumptions", "open_questions":
		return ledger.KindOpenAssumptions, "", true
	case "ready_tasks":
		return ledger.KindReadyTasks, "", true
	case "active_tasks":
		return ledger.KindActiveTasks, "", true
	case "verification_queue":
		return ledger.KindVerificationQ, "", true
	case "decision_history":
		return ledger.KindDecisionHistory, "", true
	case "evidence":
		return ledger.KindEvidenceFor, "", true
	case "delivery_summary":
		return ledger.KindDeliverySummary, "", true
	default:
		return "", "", false
	}
}

// traverse walks the remaining pointer tokens into a JSON document.
func traverse(document json.RawMessage, tokens []string) (json.RawMessage, error) {
	if len(tokens) == 0 {
		return jsonOrNull(document), nil
	}

	var value any
	if err := json.Unmarshal(jsonOrNull(document), &value); err != nil {
		return nil, fmt.Errorf("value is not valid JSON: %w", err)
	}

	for i, token := range tokens {
		switch container := value.(type) {
		case map[string]any:
			next, ok := container[token]
			if !ok {
				return nil, fmt.Errorf("no member %q at /%s", token, strings.Join(tokens[:i+1], "/"))
			}
			value = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(container) {
				return nil, fmt.Errorf("no element %q at /%s", token, strings.Join(tokens[:i+1], "/"))
			}
			value = container[index]
		default:
			return nil, fmt.Errorf("cannot address %q inside a scalar at /%s", token, strings.Join(tokens[:i], "/"))
		}
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("resolved value could not be encoded: %w", err)
	}
	return encoded, nil
}

// parsePointer decodes an RFC 6901 JSON Pointer into its tokens.
func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("%q is not a JSON Pointer: it must start with '/'", pointer)
	}
	parts := strings.Split(pointer[1:], "/")
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		// Unescape in RFC 6901 order: ~1 before ~0, so an escaped tilde
		// followed by a one cannot be misread as an escaped slash.
		part = strings.ReplaceAll(part, "~1", "/")
		part = strings.ReplaceAll(part, "~0", "~")
		tokens = append(tokens, part)
	}
	return tokens, nil
}

func jsonOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
