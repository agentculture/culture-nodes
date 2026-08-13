package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Binding resolution (PRD §11.2). The compiler has already proven every
// binding is a well-formed RFC 6901 pointer addressing a surface that exists,
// so this resolver's job is only to *read* one, and its errors are about
// runtime data ("that node has not produced output yet"), never about syntax.
//
// This resolver answers the surfaces a run's own state can answer: /run/input,
// /nodes/<id>/output, and /nodes/<id>/evidence — the same node-surface set
// internal/worker's resolver accepts, so a pointer the compiler admits never
// resolves in one runtime and fails in the other. Evidence resolves to a JSON
// array of the node's live evidence records in id order; evidence identity is
// the node run (the engine stamps node_run_id on every accepted delta
// record), and a node that appended none resolves to [] — zero records is
// itself the answer, not an absence to fail on. /ledger/projections/<name>
// needs the ledger projection reader and still fails loudly here rather than
// resolving to an empty value a caller would mistake for a real answer.

func resolveBinding(ctx context.Context, tx Tx, run Run, pointer string) (json.RawMessage, error) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("the empty pointer addresses the whole document")
	}

	var base json.RawMessage
	var rest []string

	switch tokens[0] {
	case "run":
		if len(tokens) < 2 || tokens[1] != "input" {
			return nil, fmt.Errorf("the only run surface is /run/input")
		}
		base, rest = run.Input, tokens[2:]

	case "nodes":
		if len(tokens) < 3 {
			return nil, fmt.Errorf("a node binding names a node and a surface, e.g. /nodes/<node>/output")
		}
		switch tokens[2] {
		case "output":
			output, err := tx.NodeOutput(ctx, run.ID, tokens[1])
			if err != nil {
				return nil, err
			}
			if output == nil {
				return nil, fmt.Errorf("node %q has no succeeded attempt in this run, so it has no output", tokens[1])
			}
			base, rest = output, tokens[3:]
		case "evidence":
			records, err := tx.NodeEvidence(ctx, run.ID, tokens[1])
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
			return nil, fmt.Errorf("surface %q is not resolvable; this engine resolves /run/input, /nodes/<node>/output, and /nodes/<node>/evidence", tokens[2])
		}

	default:
		return nil, fmt.Errorf("binding root %q is not resolvable yet; this engine resolves /run/input and /nodes/<node>/output", tokens[0])
	}

	return traverse(base, rest)
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
