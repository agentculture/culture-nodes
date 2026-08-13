package actors

import (
	"bytes"
	"encoding/json"
)

// MergeWorkspaceMeasured folds an actor-reported `workspace_measured` block
// into the node output the completion will persist, so downstream nodes can
// bind it through /nodes/<id>/output (issue #33a).
//
// The block is merged ONLY when the actor reported one: an absent block
// leaves output byte-for-byte untouched, because "the actor said nothing
// about its workspace" is a different fact from any block at all. When the
// actor did report one, the block goes in verbatim — a `measured:false`
// block with every fact null round-trips exactly as sent, never re-rendered
// as an empty diff and never dropped, because "I could not measure" is a
// measurement outcome the downstream reader must be able to see.
//
// The typed top-level block wins over a `workspace_measured` key the actor
// happened to place inside its own output: the top-level block is the one
// the bridge measured around the session (structurally separate from the
// agent-authored output by design), and it is the one this seam is the
// contract for.
//
// Two shapes cannot hold a merged key and are left untouched: an output that
// is not a JSON object (an array or scalar — rewriting it would corrupt the
// actor's own answer, which the node's contract validates as-is), and an
// output that is not valid JSON at all (the engine will reject it with a
// better diagnostic than this seam could produce). An empty or null output
// becomes an object holding only the block, so a failure diagnostic or an
// output-less completion still carries the measurement.
//
// This is actor-reported data riding inside node output — §10.4 authority is
// unchanged, and no path through here writes an `observed` evidence record.
func MergeWorkspaceMeasured(output, measured json.RawMessage) json.RawMessage {
	if isEmptyJSON(measured) {
		return output
	}
	if isEmptyJSON(output) {
		merged, err := json.Marshal(map[string]json.RawMessage{"workspace_measured": measured})
		if err != nil {
			return output
		}
		return merged
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output, &fields); err != nil {
		// Not a JSON object: there is nowhere to put the key without
		// rewriting the actor's own answer.
		return output
	}
	fields["workspace_measured"] = measured
	merged, err := json.Marshal(fields)
	if err != nil {
		return output
	}
	return merged
}

// isEmptyJSON reports whether raw carries no value: absent, blank, or a JSON
// null. All three mean "nothing was said", which merging must preserve.
func isEmptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
