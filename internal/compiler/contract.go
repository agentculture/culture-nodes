package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// bindingRoots are the three data surfaces a JSON Pointer binding may reach
// (PRD §11.2). Anything else is unresolvable: there is no template language
// and no ambient scope to fall back on.
const (
	bindingRootRun    = "run"
	bindingRootNodes  = "nodes"
	bindingRootLedger = "ledger"
)

// nodeBindingSurfaces are the per-node surfaces a binding may address and a
// resolver actually answers: `output` is the node's declared output;
// `evidence` is the node's ledger evidence records, selected by node run —
// the engine stamps node_run_id on every accepted delta record, so the node
// run is evidence's identity. The PRD §11.1 example binds both.
var nodeBindingSurfaces = map[string]bool{
	"output":   true,
	"evidence": true,
}

// deferredNodeBindingSurfaces are per-node surfaces the PRD names that no
// input/output resolver answers yet: `artifacts` needs the artifact router
// and `error` needs the error payload surface. A data binding (input.from,
// input.bindings, output.from) naming one is rejected here rather than
// accepted-and-refused-at-dispatch, so the compiler's verdict and the
// resolvers' stay in agreement from both sides — a workflow cannot publish a
// binding every runtime would fail loudly on (task t7). Moving one of these
// into nodeBindingSurfaces requires teaching both resolvers
// (internal/worker/bindings.go and internal/engine/binding.go) to answer it.
//
// operation.workspaceRef is exempt: it is not resolved by either resolver —
// the worker hands it opaque to the runner boundary as a workspace SourceRef
// (internal/worker/code.go) — and the PRD §11.1 example points it at
// /nodes/build/artifacts/workspace.
var deferredNodeBindingSurfaces = map[string]bool{
	"artifacts": true,
	"error":     true,
}

// checkContracts is the §11.4 contract level: schemas are valid, every node
// declares the outcomes its kind needs, bindings resolve, and CEL compiles.
func (c *compilation) checkContracts() {
	for i, trigger := range c.doc.Spec.Triggers {
		if trigger.When != "" {
			c.compileCEL(fmt.Sprintf("/spec/triggers/%d/when", i), trigger.When)
		}
	}
	c.checkSchemaSource("/spec/contract/input", c.doc.Spec.Contract.Input)
	c.checkSchemaSource("/spec/contract/output", c.doc.Spec.Contract.Output)

	for _, id := range c.nodeIDs {
		n := c.doc.Spec.Nodes[id]
		base := pointerJoin("/spec/nodes", id)
		c.checkNodeOutcomes(base, id, n)
		c.checkNodeSchemas(base, n)
		c.checkNodeBindings(base, n)
		c.checkNodeHooks(base, id, n)
		if n.Continue != nil {
			for j, expression := range n.Continue.While {
				c.compileCEL(base+"/continue/while/"+strconv.Itoa(j), expression)
			}
		}

		for j, port := range n.Select {
			if port.When != "" {
				c.compileCEL(base+"/select/"+strconv.Itoa(j)+"/when", port.When)
			}
		}
	}

	for i, e := range c.doc.Spec.Edges {
		if e.When != "" {
			c.compileCEL("/spec/edges/"+strconv.Itoa(i)+"/when", e.When)
		}
	}
}

// checkNodeOutcomes enforces the per-kind minimum every node needs before an
// edge can route out of it. The authoring schema stops at what the PRD states
// outright; the rest lives here so authoring stays open and publication does
// not.
func (c *compilation) checkNodeOutcomes(base, id string, n *node) {
	switch n.Kind {
	case KindAgent, KindCode, KindActionHTTP:
		if n.Contract == nil || len(n.Contract.Outcomes) == 0 {
			c.add(LevelError, base+"/contract/outcomes", CodeContractOutcomesMissing,
				fmt.Sprintf("node %q of kind %q declares no domain outcomes", id, n.Kind),
				"declare one output schema per outcome under contract.outcomes (PRD §9.3)")
		}
	case KindDecision:
		if len(n.Select) == 0 {
			c.add(LevelError, base+"/select", CodeContractSelectMissing,
				fmt.Sprintf("decision node %q declares no output ports", id),
				"add a select list of {outcome, when} pairs (PRD §9.2)")
		}
	case KindWait:
		if n.Until == nil {
			c.add(LevelError, base+"/until", CodeContractUntilMissing,
				fmt.Sprintf("wait node %q declares no resume condition", id),
				"add an until block with a duration, timestamp, or signal (PRD §9.2)")
		}
	}
}

// checkNodeHooks is part of the §11.4 contract level (task t14, spec claim
// c37): pre_run/post_run run around an agent node's own actor dispatch, so
// they are declared only on agent nodes, and a post-run hook's on_failure
// names either the reject_assurance sentinel or an outcome the node itself
// declares — routing a check failure to an outcome the node never promised
// would be exactly the silent gap honesty condition h32 forbids.
func (c *compilation) checkNodeHooks(base, id string, n *node) {
	if n.PreRun == nil && n.PostRun == nil {
		return
	}
	if n.Kind != KindAgent {
		if n.PreRun != nil {
			c.add(LevelError, base+"/pre_run", CodeHookKindNotAgent,
				fmt.Sprintf("node %q declares pre_run but is kind %q; pre-run/post-run hooks run around an agent's own actor dispatch and are only meaningful on an agent node", id, n.Kind),
				"remove pre_run, or change the node's kind to agent")
		}
		if n.PostRun != nil {
			c.add(LevelError, base+"/post_run", CodeHookKindNotAgent,
				fmt.Sprintf("node %q declares post_run but is kind %q; pre-run/post-run hooks run around an agent's own actor dispatch and are only meaningful on an agent node", id, n.Kind),
				"remove post_run, or change the node's kind to agent")
		}
		return
	}

	if n.PostRun == nil {
		return
	}
	onFailure := n.PostRun.OnFailure
	if onFailure.RejectAssurance || onFailure.Outcome == "" {
		// An empty, non-sentinel outcome only reaches here when the document
		// decoded despite a schema violation (on_failure is required, and
		// its object shape requires a non-empty outcome) — the structure
		// level already reported that with its own pointer.
		return
	}
	if !contains(declaredOutcomes(n), onFailure.Outcome) {
		c.add(LevelError, base+"/post_run/on_failure/outcome", CodeHookOutcomeUndeclared,
			fmt.Sprintf("node %q post_run.on_failure names outcome %q, which the node does not declare", id, onFailure.Outcome),
			fmt.Sprintf("declare it under the node's contract.outcomes, or route to one of: %s", strings.Join(declaredOutcomes(n), ", ")))
	}
}

// checkNodeSchemas validates every inline JSON Schema the node carries.
// A schemaRef is left alone: resolving and bundling a referenced schema needs
// a source root, which belongs to the deployment level (PRD §11.4).
func (c *compilation) checkNodeSchemas(base string, n *node) {
	if n.Contract == nil {
		return
	}
	c.checkSchemaSource(base+"/contract/input", n.Contract.Input)
	c.checkSchemaSource(base+"/contract/error", n.Contract.Error)
	for _, outcome := range sortedKeys(n.Contract.Outcomes) {
		c.checkSchemaSource(pointerJoin(base+"/contract/outcomes", outcome), n.Contract.Outcomes[outcome])
	}
}

// checkSchemaSource compiles an inline schema so an unusable contract is
// caught at publish time rather than at the first payload that hits it.
func (c *compilation) checkSchemaSource(path string, source *schemaSource) {
	if source == nil || source.Schema == nil {
		return
	}
	if _, err := compileInlineSchema(source.Schema); err != nil {
		c.add(LevelError, path+"/schema", CodeContractSchemaInvalid,
			fmt.Sprintf("inline schema is not a usable JSON Schema Draft 2020-12 document: %v", err),
			"fix the schema, or move it to a file and reference it with schemaRef")
	}
}

// compileInlineSchema round-trips the decoded schema through canonical JSON so
// the compiler validates exactly the bytes it will digest, then compiles it.
// The compiled schema is returned for the callers that go on to validate
// something against it (checkNodeLiterals); checkSchemaSource wants only the
// verdict.
func compileInlineSchema(schema map[string]any) (*jsonschema.Schema, error) {
	canonical, err := contracts.CanonicalJSON(schema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const inlineURI = "https://nodes.culture.dev/inline/schema.json"
	if err := compiler.AddResource(inlineURI, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(inlineURI)
}

// checkNodeBindings validates every JSON Pointer the node uses to move data,
// and every literal it declares in their place (issue #73).
func (c *compilation) checkNodeBindings(base string, n *node) {
	if n.Input != nil {
		// input.from is a whole-value move and stays pointer-only: a literal
		// there would be a node whose entire input is a constant, which is a
		// fixture rather than a workflow step.
		if n.Input.From != "" {
			c.checkBinding(base+"/input/from", n.Input.From)
		}
		for _, key := range sortedKeys(n.Input.Bindings) {
			value := n.Input.Bindings[key]
			if value.isLiteral() {
				continue
			}
			c.checkBinding(pointerJoin(base+"/input/bindings", key), value.Pointer)
		}
		c.checkNodeLiterals(base, n)
	}
	if n.Output != nil && n.Output.From != "" {
		c.checkBinding(base+"/output/from", n.Output.From)
	}
	if n.Operation != nil && n.Operation.WorkspaceRef != "" {
		c.checkWorkspaceRef(base+"/operation/workspaceRef", n.Operation.WorkspaceRef)
	}
}

// literalCheckBlockers are top-level keywords that make a node input schema's
// verdict on one member depend on members the compiler cannot see. A literal is
// known at publish time but its pointer-bound siblings are not, so under a
// combinator the "which branch applies" question has no answer yet and a
// failure reported here could be a branch the full payload would never take.
// Rather than guess, the literal check stands down for such a schema and the
// contract is enforced where it always was — at dispatch, against the whole
// resolved payload.
var literalCheckBlockers = []string{
	"allOf", "anyOf", "oneOf", "not", "if",
	"$ref", "dependentSchemas", "dependentRequired",
}

// checkNodeLiterals validates each declared literal against the node's own
// input contract. This is what makes a literal worth more than an opaque blob:
// the value is fully known at publish time, so a literal the node itself
// refuses is a publish-time error rather than a first-dispatch surprise.
//
// One literal is checked at a time, as a single-member object, and only the
// violations located inside that member are reported. Members supplied by
// pointer bindings do not exist yet, so a `required` verdict over the whole
// payload would be an error about data the author did move — just not here.
// Everything the contract says about the member's own SHAPE still applies,
// including an `additionalProperties: false` that forbids the member outright.
func (c *compilation) checkNodeLiterals(base string, n *node) {
	if n.Contract == nil || n.Contract.Input == nil || n.Contract.Input.Schema == nil {
		return
	}
	literals := make([]string, 0, len(n.Input.Bindings))
	for _, key := range sortedKeys(n.Input.Bindings) {
		if n.Input.Bindings[key].isLiteral() {
			literals = append(literals, key)
		}
	}
	if len(literals) == 0 {
		return
	}
	for _, blocker := range literalCheckBlockers {
		if _, ok := n.Contract.Input.Schema[blocker]; ok {
			return
		}
	}

	schema, err := compileInlineSchema(n.Contract.Input.Schema)
	if err != nil {
		// checkSchemaSource already reported the unusable schema with its own
		// pointer; there is nothing to check a literal against.
		return
	}
	for _, key := range literals {
		literalPath := pointerJoin(base+"/input/bindings", key) + "/" + bindingLiteralKey
		for _, violation := range literalViolations(schema, key, n.Input.Bindings[key].Literal) {
			c.add(LevelError, literalPath+violation.Pointer, CodeContractLiteralInvalid,
				fmt.Sprintf("literal binding %q does not satisfy the node's input contract: %s", key, violation.Message),
				"correct the literal, or widen the node's contract.input schema to admit it")
		}
	}
}

// literalViolations validates {key: literal} against the node input schema and
// keeps only the failures located inside `key`.
func literalViolations(schema *jsonschema.Schema, key string, literal json.RawMessage) []contracts.Violation {
	instance, err := json.Marshal(map[string]json.RawMessage{key: literal})
	if err != nil {
		return nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance))
	if err != nil {
		return nil
	}
	err = schema.Validate(doc)
	if err == nil {
		return nil
	}
	var schemaErr *jsonschema.ValidationError
	if !errors.As(err, &schemaErr) {
		return nil
	}

	prefix := "/" + escapePointerToken(key)
	var out []contracts.Violation
	for _, violation := range contracts.Violations(schemaErr) {
		if violation.Pointer != prefix && !strings.HasPrefix(violation.Pointer, prefix+"/") {
			continue
		}
		violation.Pointer = strings.TrimPrefix(violation.Pointer, prefix)
		out = append(out, violation)
	}
	return out
}

// checkBinding decides whether a data-binding pointer is well-formed and
// addresses something a resolver will answer. It deliberately stops short of
// the ledger *vocabulary*: whether `/ledger/projections/foo` names a real
// projection is the ledger level's verdict, reported with the ledger level's
// code.
func (c *compilation) checkBinding(path, pointer string) {
	c.checkPointer(path, pointer, false)
}

// checkWorkspaceRef validates an operation.workspaceRef, which reaches the
// runner boundary opaque rather than either input/output resolver — so the
// deferred surfaces (artifacts) stay addressable from it.
func (c *compilation) checkWorkspaceRef(path, pointer string) {
	c.checkPointer(path, pointer, true)
}

func (c *compilation) checkPointer(path, pointer string, allowDeferredSurfaces bool) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		c.add(LevelError, path, CodeContractBindingMalformed,
			fmt.Sprintf("%q is not a valid JSON Pointer (RFC 6901)", pointer),
			"start the pointer with '/' and escape '~' as '~0' and '/' as '~1'")
		return
	}
	if len(tokens) == 0 {
		c.unresolvedBinding(path, pointer, "the empty pointer addresses the whole document")
		return
	}

	switch tokens[0] {
	case bindingRootRun:
		if len(tokens) < 2 || tokens[1] != "input" {
			c.unresolvedBinding(path, pointer, "the only run surface is /run/input")
		}
	case bindingRootNodes:
		if len(tokens) < 2 {
			c.unresolvedBinding(path, pointer, "name a node, e.g. /nodes/<node>/output")
			return
		}
		if _, ok := c.doc.Spec.Nodes[tokens[1]]; !ok {
			c.add(LevelError, path, CodeContractBindingNodeUnknown,
				fmt.Sprintf("binding %q references node %q, which is not declared in spec.nodes", pointer, tokens[1]),
				fmt.Sprintf("bind to one of: %s", strings.Join(c.nodeIDs, ", ")))
			return
		}
		if len(tokens) >= 3 && deferredNodeBindingSurfaces[tokens[2]] {
			if !allowDeferredSurfaces {
				c.unresolvedBinding(path, pointer,
					fmt.Sprintf("surface %q is not resolvable by any runtime yet; bind one of: %s",
						tokens[2], strings.Join(sortedKeys(nodeBindingSurfaces), ", ")))
			}
			return
		}
		if len(tokens) < 3 || !nodeBindingSurfaces[tokens[2]] {
			c.unresolvedBinding(path, pointer,
				fmt.Sprintf("a node surface must be one of: %s", strings.Join(sortedKeys(nodeBindingSurfaces), ", ")))
		}
	case bindingRootLedger:
		if len(tokens) < 3 || tokens[1] != "projections" {
			c.unresolvedBinding(path, pointer, "the only ledger surface is /ledger/projections/<projection>")
		}
	default:
		c.unresolvedBinding(path, pointer,
			fmt.Sprintf("a binding must start at /%s, /%s, or /%s", bindingRootRun, bindingRootNodes, bindingRootLedger))
	}
}

func (c *compilation) unresolvedBinding(path, pointer, why string) {
	c.add(LevelError, path, CodeContractBindingUnresolved,
		fmt.Sprintf("binding %q does not address any run, node, or ledger data", pointer),
		why)
}
