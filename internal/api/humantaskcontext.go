package api

// Resolved context for a human task (issue #332, task t43).
//
// A human task's request carries its node's input binding as POINTERS
// (internal/engine/humantask.go's humanTaskContext): `fix: /nodes/fix/output`,
// `from: /run/input`. That is the right thing to store — it is what the
// workflow declares — and the wrong thing to show a person deciding a merge:
// the owner could not tell what PR #326 was, or that it had already merged,
// from three JSON-pointer paths. This file resolves each ref at read time and
// returns the values next to the raw refs, which stay in `request` verbatim.
//
// # What a caller can see
//
// Nothing here widens a read. Every surface a ref can resolve to is already
// returned to the same caller by GET /v1alpha1/runs/{id}: `/run/input` is
// that view's `run.input`, and `/nodes/<id>/output` is the `result` of the
// node's newest succeeded attempt, which that view lists under
// `node_runs[].attempts[]`. Both routes are GETs, which principalPolicy
// (principal.go) leaves ungated alike, and both are scoped to this server's
// namespace — the run read below joins `runs.namespace_id` as well, so a
// task row can never pull a value out of another namespace's run. Node
// evidence is NOT inlined: it is a ledger read with its own route, and
// folding it in here would be a second rendering of the ledger to keep in
// step; the ref says so instead.
//
// Declaration exposure (internal/decl/exposure.go) governs what a
// declaration's action may RENDER into a wider audience at firing time. A
// human.ask declaration's task binds `from: /run/input`, the input the
// declaration already rendered and stored on its run — this reads that
// stored value back, it does not render a template, so no unexposed
// variable becomes visible here that the run view was not already showing.
//
// # Size
//
// A value is bounded to contextValueMaxBytes of JSON. A larger one is shrunk
// (long strings cut, long arrays shortened) and marked `truncated`; one that
// cannot be shrunk under the bound is omitted and named `unresolved`, with
// the run view as the place to read it whole.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/agentculture/culture-nodes/internal/engine"
)

const (
	// contextValueMaxBytes bounds one resolved value's encoded JSON.
	contextValueMaxBytes = 16 << 10
	// contextFromName is the name the single `from` pointer is listed under.
	contextFromName = "from"
)

// shrinkPasses are the successive string/array caps a too-large value is
// cut to: the first keeps a long markdown summary legible, the second is the
// last attempt before the value is dropped.
var shrinkPasses = []struct{ maxString, maxItems int }{
	{8 << 10, 50},
	{1 << 10, 10},
}

// HumanTaskContextValueOut is components.schemas.HumanTaskContextValue: one
// context ref resolved for display. Name is the binding's name ("from" for
// the single pointer); Ref is the pointer exactly as authored (empty for a
// literal). Exactly one of Value and Unresolved is set.
type HumanTaskContextValueOut struct {
	Name       string          `json:"name"`
	Ref        string          `json:"ref,omitempty"`
	Literal    bool            `json:"literal,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
	Unresolved string          `json:"unresolved,omitempty"`
}

// storedContextRefs is the part of human_tasks.request this file reads.
type storedContextRefs struct {
	ContextRefs *struct {
		From     string                     `json:"from"`
		Bindings map[string]json.RawMessage `json:"bindings"`
	} `json:"context_refs"`
}

// contextRef is one ref awaiting resolution.
type contextRef struct {
	task    int
	name    string
	pointer string
	literal json.RawMessage
	tokens  []string
	// parseErr is set when the stored pointer does not parse.
	parseErr error
}

type runNodeKey struct{ runID, nodeID string }

// resolveHumanTaskContexts fills ResolvedContext on every task that carries
// context refs, with one run-input read and one node-output read for the
// whole page — never a query per task.
func (s *Server) resolveHumanTaskContexts(ctx context.Context, tasks []HumanTaskOut) error {
	var refs []contextRef
	runSet := map[string]bool{}
	nodeSet := map[string]bool{}
	for i := range tasks {
		for _, ref := range taskContextRefs(i, tasks[i].Request) {
			if ref.literal == nil && ref.parseErr == nil && len(ref.tokens) > 0 {
				switch ref.tokens[0] {
				case "run":
					runSet[tasks[i].RunID] = true
				case "nodes":
					if len(ref.tokens) >= 3 && ref.tokens[2] == "output" {
						runSet[tasks[i].RunID] = true
						nodeSet[ref.tokens[1]] = true
					}
				}
			}
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}

	inputs, err := s.contextRunInputs(ctx, contextSetKeys(runSet))
	if err != nil {
		return err
	}
	outputs := map[runNodeKey]json.RawMessage{}
	if len(nodeSet) > 0 {
		outputs, err = s.contextNodeOutputs(ctx, contextSetKeys(runSet), contextSetKeys(nodeSet))
		if err != nil {
			return err
		}
	}

	for _, ref := range refs {
		task := &tasks[ref.task]
		task.ResolvedContext = append(task.ResolvedContext,
			resolveContextRef(ref, task.RunID, inputs, outputs))
	}
	return nil
}

// taskContextRefs lists a request's refs in display order: `from` first,
// then bindings by name. A request that does not parse has none — the raw
// request is still returned verbatim, so nothing is hidden by skipping it.
func taskContextRefs(task int, request json.RawMessage) []contextRef {
	var stored storedContextRefs
	if len(request) == 0 || json.Unmarshal(request, &stored) != nil || stored.ContextRefs == nil {
		return nil
	}
	var out []contextRef
	if stored.ContextRefs.From != "" {
		out = append(out, pointerRef(task, contextFromName, stored.ContextRefs.From))
	}
	names := make([]string, 0, len(stored.ContextRefs.Bindings))
	for name := range stored.ContextRefs.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := bytes.TrimSpace(stored.ContextRefs.Bindings[name])
		var pointer string
		if json.Unmarshal(raw, &pointer) == nil {
			out = append(out, pointerRef(task, name, pointer))
			continue
		}
		var literal struct {
			Literal json.RawMessage `json:"literal"`
		}
		if json.Unmarshal(raw, &literal) == nil && literal.Literal != nil {
			out = append(out, contextRef{task: task, name: name, literal: literal.Literal})
			continue
		}
		out = append(out, contextRef{task: task, name: name,
			parseErr: fmt.Errorf("binding is neither a pointer nor a literal")})
	}
	return out
}

func pointerRef(task int, name, pointer string) contextRef {
	tokens, err := engine.ParseBindingPointer(pointer)
	if err == nil && len(tokens) == 0 {
		err = fmt.Errorf("the empty pointer addresses the whole document")
	}
	return contextRef{task: task, name: name, pointer: pointer, tokens: tokens, parseErr: err}
}

// resolveContextRef resolves one ref against the page's reads, with the
// same surface rules as internal/engine/binding.go's resolveBinding.
func resolveContextRef(ref contextRef, runID string, inputs map[string]json.RawMessage, outputs map[runNodeKey]json.RawMessage) HumanTaskContextValueOut {
	out := HumanTaskContextValueOut{Name: ref.name, Ref: ref.pointer}
	if ref.literal != nil {
		out.Literal = true
		out.Value, out.Truncated, out.Unresolved = boundContextValue(ref.literal)
		return out
	}
	if ref.parseErr != nil {
		out.Unresolved = ref.parseErr.Error()
		return out
	}

	var base json.RawMessage
	var rest []string
	switch ref.tokens[0] {
	case "run":
		if len(ref.tokens) < 2 || ref.tokens[1] != "input" {
			out.Unresolved = "the only run surface is /run/input"
			return out
		}
		input, ok := inputs[runID]
		if !ok {
			out.Unresolved = "the task's run is not readable in this namespace"
			return out
		}
		base, rest = input, ref.tokens[2:]
	case "nodes":
		if len(ref.tokens) < 3 {
			out.Unresolved = "a node binding names a node and a surface, e.g. /nodes/<node>/output"
			return out
		}
		switch ref.tokens[2] {
		case "output":
			output, ok := outputs[runNodeKey{runID, ref.tokens[1]}]
			if !ok || output == nil {
				out.Unresolved = fmt.Sprintf("node %q has no succeeded attempt in this run, so it has no output", ref.tokens[1])
				return out
			}
			base, rest = output, ref.tokens[3:]
		case "evidence":
			out.Unresolved = "node evidence is not inlined here; read it on the run's ledger view"
			return out
		default:
			out.Unresolved = fmt.Sprintf("surface %q is not resolvable", ref.tokens[2])
			return out
		}
	default:
		out.Unresolved = fmt.Sprintf("binding root %q is not resolvable here", ref.tokens[0])
		return out
	}

	value, err := engine.TraverseJSON(base, rest)
	if err != nil {
		out.Unresolved = err.Error()
		return out
	}
	out.Value, out.Truncated, out.Unresolved = boundContextValue(value)
	return out
}

// boundContextValue returns raw unchanged when it fits contextValueMaxBytes,
// a shrunk copy marked truncated when shrinking makes it fit, and no value
// with a reason when nothing does.
func boundContextValue(raw json.RawMessage) (json.RawMessage, bool, string) {
	if len(raw) <= contextValueMaxBytes {
		return raw, false, ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false, fmt.Sprintf("value is %d bytes and not valid JSON; read it on the run view", len(raw))
	}
	for _, pass := range shrinkPasses {
		encoded, err := json.Marshal(shrinkJSON(decoded, pass.maxString, pass.maxItems))
		if err == nil && len(encoded) <= contextValueMaxBytes {
			return encoded, true, ""
		}
	}
	return nil, false, fmt.Sprintf("value is %d bytes, over the %d-byte inbox bound; read it whole on the run view",
		len(raw), contextValueMaxBytes)
}

// shrinkJSON cuts strings to maxString bytes (on a rune boundary, with an
// ellipsis) and arrays to maxItems elements, recursively.
func shrinkJSON(value any, maxString, maxItems int) any {
	switch v := value.(type) {
	case string:
		if len(v) <= maxString {
			return v
		}
		cut := maxString
		for cut > 0 && !utf8.RuneStart(v[cut]) {
			cut--
		}
		return v[:cut] + "…"
	case []any:
		if len(v) > maxItems {
			v = v[:maxItems]
		}
		out := make([]any, len(v))
		for i := range v {
			out[i] = shrinkJSON(v[i], maxString, maxItems)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = shrinkJSON(item, maxString, maxItems)
		}
		return out
	default:
		return v
	}
}

// contextRunInputs reads the inputs of runIDs in this namespace.
func (s *Server) contextRunInputs(ctx context.Context, runIDs []string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.Store.Pool().Query(ctx,
		`SELECT id, COALESCE(input, 'null'::jsonb) FROM runs WHERE namespace_id = $1 AND id = ANY($2)`,
		s.NamespaceID, runIDs)
	if err != nil {
		return nil, fmt.Errorf("api: human task context: read run inputs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var input []byte
		if err := rows.Scan(&id, &input); err != nil {
			return nil, fmt.Errorf("api: human task context: scan run input: %w", err)
		}
		out[id] = json.RawMessage(input)
	}
	return out, rows.Err()
}

// contextNodeOutputs reads, per (run, node), the result of the newest
// succeeded attempt — the exact row and order postgres.NodeOutput uses for a
// `/nodes/<id>/output` binding, so the inbox shows what the engine resolves.
// Every (run, node) cross pair is read; unrelated pairs simply go unused.
func (s *Server) contextNodeOutputs(ctx context.Context, runIDs, nodeIDs []string) (map[runNodeKey]json.RawMessage, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT DISTINCT ON (nr.run_id, nr.node_key) nr.run_id, nr.node_key, a.result
		FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		JOIN runs AS r ON r.id = nr.run_id
		WHERE r.namespace_id = $1 AND nr.run_id = ANY($2) AND nr.node_key = ANY($3)
		  AND a.status = 'succeeded'
		ORDER BY nr.run_id, nr.node_key, a.completed_at DESC, a.id DESC`,
		s.NamespaceID, runIDs, nodeIDs)
	if err != nil {
		return nil, fmt.Errorf("api: human task context: read node outputs: %w", err)
	}
	defer rows.Close()
	out := map[runNodeKey]json.RawMessage{}
	for rows.Next() {
		var key runNodeKey
		var result []byte
		if err := rows.Scan(&key.runID, &key.nodeID, &result); err != nil {
			return nil, fmt.Errorf("api: human task context: scan node output: %w", err)
		}
		if result != nil {
			out[key] = json.RawMessage(result)
		}
	}
	return out, rows.Err()
}

func contextSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
