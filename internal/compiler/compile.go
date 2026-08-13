package compiler

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// CompiledWorkflow is a workflow that passed every validation level: the exact
// source the author submitted, the normalized IR the runtime executes, and the
// digest that addresses it.
//
// PRD §11.3 requires both halves to be stored: the source so a human can
// review what was written, the IR because the runtime executes only the
// normalized representation.
type CompiledWorkflow struct {
	// Name and Version come from the document's metadata.
	Name    string
	Version string

	// Format and Source are the submission, byte for byte.
	Format Format
	Source []byte

	// IR is the normalized representation, and Normalized is its canonical
	// JSON encoding — the exact bytes Digest addresses.
	IR         *IR
	Normalized []byte
	Digest     string

	// Programs holds the compiled CEL guards keyed by the JSON path of the
	// expression they came from (e.g. "/spec/edges/3/when").
	//
	// They are deliberately *not* serialized into the IR: an AST encoding
	// would tie the content digest to cel-go's internal representation, so a
	// dependency upgrade would silently re-digest every published workflow.
	// The IR keeps the expression text, which is what the author wrote and
	// what the digest should address.
	Programs map[string]cel.Program
}

// compilation carries one document through the validation levels.
type compilation struct {
	doc      *document
	nodeIDs  []string
	diags    []Diagnostic
	env      *cel.Env
	programs map[string]cel.Program
}

func (c *compilation) add(level Level, path, code, message, hint string) {
	c.diags = append(c.diags, Diagnostic{
		Level:   level,
		Path:    path,
		Code:    code,
		Message: message,
		Hint:    hint,
	})
}

// sharedValidator and sharedCELEnv are built once. Both are read-only after
// construction; rebuilding them per call would recompile every embedded schema
// for every document validated.
var (
	sharedValidator = sync.OnceValues(contracts.NewValidator)
	sharedCELEnv    = sync.OnceValues(newCELEnv)
)

// Compile runs a workflow document through every validation level the MVP
// implements (PRD §11.4, in order: syntax, structure, graph, contract, ledger,
// policy, owners) and, if nothing blocks it, produces the normalized IR and
// its content digest.
//
// The three return values are three different kinds of answer:
//
//   - the error is reserved for failures of the *compiler* — an unknown
//     format, an embedded schema that will not compile — never for a document
//     that is merely wrong;
//   - the diagnostics describe the document, and are always sorted by path
//     then code so two compilations of the same bytes are comparable;
//   - the CompiledWorkflow is non-nil only when no diagnostic is an error.
//
// Levels do not stop each other. A schema violation and an unresolvable owner
// at the same path are two findings from two levels, and reporting only the
// first would hide the second the moment the author fixed it.
func Compile(source []byte, format Format) (*CompiledWorkflow, []Diagnostic, error) {
	// Syntax.
	jsonBytes, syntaxDiag, err := toJSON(source, format)
	if err != nil {
		return nil, nil, err
	}
	if syntaxDiag != nil {
		return nil, []Diagnostic{*syntaxDiag}, nil
	}
	if !isJSONObject(jsonBytes) {
		return nil, []Diagnostic{{
			Level:   LevelError,
			Path:    "",
			Code:    CodeSyntaxNotAnObject,
			Message: "a workflow document must be a JSON object",
			Hint:    "the document should start with apiVersion, kind, metadata, and spec",
		}}, nil
	}

	env, err := sharedCELEnv()
	if err != nil {
		return nil, nil, err
	}
	c := &compilation{env: env, programs: make(map[string]cel.Program)}

	// Structure.
	if err := c.checkStructure(jsonBytes); err != nil {
		return nil, nil, err
	}

	// Structure failures do not stop the deeper levels, but an undecodable
	// document does: there is nothing left to reason about.
	var doc document
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		c.add(LevelError, "", CodeStructureDecode,
			fmt.Sprintf("document could not be read as a workflow: %v", err),
			"fix the reported schema violations first; the document's shape does not match the workflow contract")
		return nil, c.finishDiagnostics(), nil
	}
	c.doc = &doc
	c.nodeIDs = sortedKeys(doc.Spec.Nodes)

	c.checkGraph()
	c.checkParallelJoin()
	c.checkContracts()
	c.checkLedger()
	c.checkPolicy()
	c.checkOwners()

	diags := c.finishDiagnostics()
	if HasErrors(diags) {
		return nil, diags, nil
	}

	ir, err := c.normalize()
	if err != nil {
		return nil, nil, err
	}
	normalized, err := contracts.CanonicalJSON(ir)
	if err != nil {
		return nil, nil, err
	}

	return &CompiledWorkflow{
		Name:       doc.Metadata.Name,
		Version:    doc.Metadata.Version,
		Format:     format,
		Source:     source,
		IR:         ir,
		Normalized: normalized,
		Digest:     contracts.Digest(normalized),
		Programs:   c.programs,
	}, diags, nil
}

// checkStructure is the §11.4 structure level: the document validates against
// the embedded workflow schema. Each violation keeps the schema's own JSON
// Pointer, including the synthesized pointer to a missing required property.
func (c *compilation) checkStructure(jsonBytes []byte) error {
	validator, err := sharedValidator()
	if err != nil {
		return err
	}
	err = validator.ValidateJSON(contracts.SchemaWorkflow, jsonBytes)
	if err == nil {
		return nil
	}

	var validationErr *contracts.ValidationError
	if !errors.As(err, &validationErr) {
		return err
	}
	for _, violation := range validationErr.Violations {
		c.add(LevelError, violation.Pointer, CodeStructureSchema,
			violation.Message,
			fmt.Sprintf("see %s in the workflow schema", violation.SchemaLocation))
	}
	return nil
}

func (c *compilation) finishDiagnostics() []Diagnostic {
	diags := dedupeDiagnostics(c.diags)
	sortDiagnostics(diags)
	return diags
}
