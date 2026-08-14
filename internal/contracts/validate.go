package contracts

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/agentculture/culture-nodes/schemas"
)

// Schema names are the embedded paths under schemas/, which are also the
// suffixes of each schema's $id.
const (
	SchemaWorkflow = "workflow/workflow.schema.json"

	SchemaLedgerEnvelope      = "ledger/envelope.schema.json"
	SchemaLedgerRecord        = "ledger/record.schema.json"
	SchemaLedgerAnnouncement  = "ledger/announcement.schema.json"
	SchemaLedgerClaim         = "ledger/claim.schema.json"
	SchemaLedgerAssumption    = "ledger/assumption.schema.json"
	SchemaLedgerQuestion      = "ledger/question.schema.json"
	SchemaLedgerTask          = "ledger/task.schema.json"
	SchemaLedgerDecision      = "ledger/decision.schema.json"
	SchemaLedgerSuccessSignal = "ledger/success_signal.schema.json"
	SchemaLedgerEvidence      = "ledger/evidence.schema.json"
	SchemaLedgerResult        = "ledger/result.schema.json"
	SchemaLedgerReview        = "ledger/review.schema.json"
	SchemaLedgerGrade         = "ledger/grade.schema.json"

	SchemaLedgerDispatchPreflight       = "ledger/dispatch_preflight.schema.json"
	SchemaLedgerDispatchAcknowledgement = "ledger/dispatch_acknowledgement.schema.json"

	SchemaRunnerOperation = "runner/operation.schema.json"
	SchemaRunnerResult    = "runner/result.schema.json"
)

// ledgerRecordTypes lists the MVP record types in PRD §10.2 order, followed
// by the ones registered additively after them: `grade` (issue #28 item 1)
// and the clarify-then-commit gate's `dispatch_preflight` /
// `dispatch_acknowledgement` pair (issue #67, task t14).
var ledgerRecordTypes = []string{
	"announcement",
	"claim",
	"assumption",
	"question",
	"task",
	"decision",
	"success_signal",
	"evidence",
	"result",
	"review",
	"grade",
	"dispatch_preflight",
	"dispatch_acknowledgement",
}

// LedgerRecordTypes returns the registered work-ledger record types.
func LedgerRecordTypes() []string {
	out := make([]string, len(ledgerRecordTypes))
	copy(out, ledgerRecordTypes)
	return out
}

// LedgerRecordSchema returns the schema name for a record type. An unregistered
// record type yields a name no validator knows, so validation fails loudly
// rather than falling back to a permissive schema.
func LedgerRecordSchema(recordType string) string {
	return "ledger/" + recordType + ".schema.json"
}

// errorPrinter renders library diagnostics. English is the fixed language: the
// messages are machine-consumed diagnostics, and a locale-dependent one would
// change what tests and agents match on.
var errorPrinter = message.NewPrinter(language.English)

// Violation is one schema failure, located in the document by JSON Pointer.
type Violation struct {
	// Pointer locates the offending value in the validated document. For a
	// missing required property or an unexpected extra one it points at the
	// property itself, not merely at the object that should have held it.
	Pointer string
	// Keyword is the schema keyword that failed, e.g. "required" or "enum".
	Keyword string
	// SchemaLocation is where in the schemas the rule lives, e.g.
	// "workflow/workflow.schema.json#/$defs/node/required".
	SchemaLocation string
	// Message is the human-readable failure.
	Message string
}

// ValidationError reports every violation found in one document.
type ValidationError struct {
	// Schema is the schema name the document was validated against.
	Schema string
	// Violations are deduplicated and ordered by pointer, so the same bad
	// document always produces the same diagnostic text.
	Violations []Violation
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	noun := "violations"
	if len(e.Violations) == 1 {
		noun = "violation"
	}
	fmt.Fprintf(&b, "%s: %d %s", e.Schema, len(e.Violations), noun)
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "\n  at %s: %s [%s]", v.Pointer, v.Message, v.Keyword)
	}
	return b.String()
}

// Validator validates documents against the embedded schemas. It is read-only
// after construction and safe for concurrent use.
type Validator struct {
	compiled map[string]*jsonschema.Schema
	names    []string
}

// NewValidator compiles every embedded schema. Compiling eagerly means a
// malformed schema or a dangling $ref is a startup failure with a named file,
// not a surprise on the first document that happens to reach it.
func NewValidator() (*Validator, error) {
	names, err := embeddedSchemaNames()
	if err != nil {
		return nil, err
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()

	for _, name := range names {
		data, err := schemas.FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("contracts: read embedded schema %s: %w", name, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("contracts: parse embedded schema %s: %w", name, err)
		}
		if err := compiler.AddResource(schemas.BaseURI+name, doc); err != nil {
			return nil, fmt.Errorf("contracts: register embedded schema %s: %w", name, err)
		}
	}

	v := &Validator{compiled: make(map[string]*jsonschema.Schema, len(names)), names: names}
	for _, name := range names {
		compiled, err := compiler.Compile(schemas.BaseURI + name)
		if err != nil {
			return nil, fmt.Errorf("contracts: compile embedded schema %s: %w", name, err)
		}
		v.compiled[name] = compiled
	}
	return v, nil
}

// SchemaNames returns the compiled schema names, sorted.
func (v *Validator) SchemaNames() []string {
	out := make([]string, len(v.names))
	copy(out, v.names)
	return out
}

// ValidateJSON validates raw JSON bytes against the named schema. A schema
// failure returns *ValidationError; a malformed document or an unknown schema
// name returns an ordinary error, because neither is a statement about the
// document's contents.
func (v *Validator) ValidateJSON(name string, data []byte) error {
	compiled, ok := v.compiled[name]
	if !ok {
		return fmt.Errorf("contracts: unknown schema %q", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("contracts: parse document for %s: %w", name, err)
	}
	if err := compiled.Validate(doc); err != nil {
		var schemaErr *jsonschema.ValidationError
		if errors.As(err, &schemaErr) {
			return &ValidationError{Schema: name, Violations: violationsOf(schemaErr)}
		}
		return fmt.Errorf("contracts: validate against %s: %w", name, err)
	}
	return nil
}

// Validate validates any marshalable Go value — a struct, a decoded map, a
// json.RawMessage — against the named schema. The value goes through
// CanonicalJSON first, so what is validated is exactly what would be digested.
func (v *Validator) Validate(name string, value any) error {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return err
	}
	return v.ValidateJSON(name, canonical)
}

// embeddedSchemaNames lists the embedded schema files, sorted for determinism.
func embeddedSchemaNames() ([]string, error) {
	var names []string
	err := fs.WalkDir(schemas.FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".schema.json") {
			names = append(names, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("contracts: walk embedded schemas: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("contracts: no embedded schemas found")
	}
	sort.Strings(names)
	return names, nil
}

// Violations flattens a library validation error into this package's located,
// deduplicated violations. It is exported for callers that compile a schema
// themselves rather than reaching one through a Validator — the compiler's
// literal-binding check validates against an inline node contract, and its
// diagnostics should locate a failure exactly the way every other schema
// diagnostic in this codebase does.
func Violations(err *jsonschema.ValidationError) []Violation {
	return violationsOf(err)
}

// violationsOf flattens the library's error tree to its leaves — the specific
// failures — and drops the structural nodes above them, which restate rather
// than locate.
func violationsOf(err *jsonschema.ValidationError) []Violation {
	var out []Violation
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, cause := range e.Causes {
				walk(cause)
			}
			return
		}
		out = append(out, leafViolations(e)...)
	}
	walk(err)
	return deduplicate(out)
}

// leafViolations turns one leaf failure into violations. `required` and
// `additionalProperties` are reported by the library against the enclosing
// object; naming the property is the whole point of the diagnostic, so the
// pointer is extended to it here — one violation per property.
func leafViolations(e *jsonschema.ValidationError) []Violation {
	base := jsonPointer(e.InstanceLocation)
	keyword := strings.Join(e.ErrorKind.KeywordPath(), "/")
	location := schemaLocation(e)

	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		out := make([]Violation, 0, len(k.Missing))
		for _, missing := range k.Missing {
			out = append(out, Violation{
				Pointer:        base + "/" + escapePointerToken(missing),
				Keyword:        keyword,
				SchemaLocation: location,
				Message:        fmt.Sprintf("missing required property %q", missing),
			})
		}
		return out
	case *kind.AdditionalProperties:
		out := make([]Violation, 0, len(k.Properties))
		for _, extra := range k.Properties {
			out = append(out, Violation{
				Pointer:        base + "/" + escapePointerToken(extra),
				Keyword:        keyword,
				SchemaLocation: location,
				Message:        fmt.Sprintf("unexpected property %q", extra),
			})
		}
		return out
	default:
		return []Violation{{
			Pointer:        base,
			Keyword:        keyword,
			SchemaLocation: location,
			Message:        e.ErrorKind.LocalizedString(errorPrinter),
		}}
	}
}

// schemaLocation renders where the failing rule lives, with the shared base URI
// trimmed so the reader sees a path they can open.
func schemaLocation(e *jsonschema.ValidationError) string {
	location := strings.TrimPrefix(e.SchemaURL, schemas.BaseURI)
	if fragment := jsonPointer(e.ErrorKind.KeywordPath()); fragment != "" {
		if !strings.Contains(location, "#") {
			location += "#"
		}
		location += fragment
	}
	return location
}

// jsonPointer renders tokens as an RFC 6901 pointer. No tokens means the whole
// document, whose pointer is the empty string; that reads badly in a message,
// so callers see the document root as "".
func jsonPointer(tokens []string) string {
	var b strings.Builder
	for _, token := range tokens {
		b.WriteByte('/')
		b.WriteString(escapePointerToken(token))
	}
	return b.String()
}

func escapePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

// deduplicate collapses repeats — a oneOf reports the same envelope failure
// once per branch — and orders the result so identical documents always
// produce identical diagnostics.
func deduplicate(violations []Violation) []Violation {
	seen := make(map[Violation]bool, len(violations))
	out := make([]Violation, 0, len(violations))
	for _, v := range violations {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pointer != out[j].Pointer {
			return out[i].Pointer < out[j].Pointer
		}
		if out[i].SchemaLocation != out[j].SchemaLocation {
			return out[i].SchemaLocation < out[j].SchemaLocation
		}
		return out[i].Message < out[j].Message
	})
	return out
}
