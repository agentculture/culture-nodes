package compiler

import (
	"bytes"
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

// nodeBindingSurfaces are the per-node surfaces a binding may address.
// `output` is the node's declared output; `evidence` is what a runner
// observed; `artifacts` are the files it produced; `error` is its error
// payload. The PRD §11.1 example binds all but the last.
var nodeBindingSurfaces = map[string]bool{
	"output":    true,
	"evidence":  true,
	"artifacts": true,
	"error":     true,
}

// checkContracts is the §11.4 contract level: schemas are valid, every node
// declares the outcomes its kind needs, bindings resolve, and CEL compiles.
func (c *compilation) checkContracts() {
	c.checkSchemaSource("/spec/contract/input", c.doc.Spec.Contract.Input)
	c.checkSchemaSource("/spec/contract/output", c.doc.Spec.Contract.Output)

	for _, id := range c.nodeIDs {
		n := c.doc.Spec.Nodes[id]
		base := pointerJoin("/spec/nodes", id)
		c.checkNodeOutcomes(base, id, n)
		c.checkNodeSchemas(base, n)
		c.checkNodeBindings(base, n)

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
	if err := compileInlineSchema(source.Schema); err != nil {
		c.add(LevelError, path+"/schema", CodeContractSchemaInvalid,
			fmt.Sprintf("inline schema is not a usable JSON Schema Draft 2020-12 document: %v", err),
			"fix the schema, or move it to a file and reference it with schemaRef")
	}
}

// compileInlineSchema round-trips the decoded schema through canonical JSON so
// the compiler validates exactly the bytes it will digest, then compiles it.
func compileInlineSchema(schema map[string]any) error {
	canonical, err := contracts.CanonicalJSON(schema)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const inlineURI = "https://nodes.culture.dev/inline/schema.json"
	if err := compiler.AddResource(inlineURI, doc); err != nil {
		return err
	}
	if _, err := compiler.Compile(inlineURI); err != nil {
		return err
	}
	return nil
}

// checkNodeBindings validates every JSON Pointer the node uses to move data.
func (c *compilation) checkNodeBindings(base string, n *node) {
	if n.Input != nil {
		if n.Input.From != "" {
			c.checkBinding(base+"/input/from", n.Input.From)
		}
		for _, key := range sortedKeys(n.Input.Bindings) {
			c.checkBinding(pointerJoin(base+"/input/bindings", key), n.Input.Bindings[key])
		}
	}
	if n.Output != nil && n.Output.From != "" {
		c.checkBinding(base+"/output/from", n.Output.From)
	}
	if n.Operation != nil && n.Operation.WorkspaceRef != "" {
		c.checkBinding(base+"/operation/workspaceRef", n.Operation.WorkspaceRef)
	}
}

// checkBinding decides whether a pointer is well-formed and addresses
// something that exists. It deliberately stops short of the ledger
// *vocabulary*: whether `/ledger/projections/foo` names a real projection is
// the ledger level's verdict, reported with the ledger level's code.
func (c *compilation) checkBinding(path, pointer string) {
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
