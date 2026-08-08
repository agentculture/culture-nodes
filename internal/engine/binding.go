package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Binding resolution (PRD §11.2). The compiler has already proven every
// binding is a well-formed RFC 6901 pointer addressing a surface that exists,
// so this resolver's job is only to *read* one, and its errors are about
// runtime data ("that node has not produced output yet"), never about syntax.
//
// The MVP resolves the two surfaces a run's own state can answer: /run/input
// and /nodes/<id>/output. The others the compiler admits — /nodes/<id>/
// evidence, /artifacts, and /ledger/projections/<name> — need the runner
// boundary and the ledger projection reader, which belong to their own tasks;
// asking for one here fails loudly rather than resolving to an empty value a
// caller would mistake for a real answer.

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
		if tokens[2] != "output" {
			return nil, fmt.Errorf("surface %q is not resolvable yet; this engine resolves /run/input and /nodes/<node>/output", tokens[2])
		}
		output, err := tx.NodeOutput(ctx, run.ID, tokens[1])
		if err != nil {
			return nil, err
		}
		if output == nil {
			return nil, fmt.Errorf("node %q has no succeeded attempt in this run, so it has no output", tokens[1])
		}
		base, rest = output, tokens[3:]

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
