// Package template parses and renders declaration variable templates.
package template

import (
	"fmt"
	"strings"
)

// Reference identifies a variable lookup. Step is "0" for the current
// declaration, a positive decimal lineage distance, or a declaration name.
// DefaultPresent distinguishes no default from an explicitly empty default.
type Reference struct {
	Step           string
	Name           string
	Default        string
	DefaultPresent bool
}

type part struct {
	text string
	ref  *Reference
	raw  string
}

// Template is a parsed template. Its references retain source order.
type Template struct {
	source string
	parts  []part
}

// Parse parses {name}, {N:name}, and {N:name:default} references. A doubled
// opening brace emits one literal opening brace.
func Parse(source string) (*Template, error) {
	t := &Template{source: source}
	var literal strings.Builder
	flush := func() {
		if literal.Len() != 0 {
			t.parts = append(t.parts, part{text: literal.String()})
			literal.Reset()
		}
	}
	for i := 0; i < len(source); {
		if strings.HasPrefix(source[i:], "{{") {
			literal.WriteByte('{')
			i += 2
			continue
		}
		if source[i] != '{' {
			literal.WriteByte(source[i])
			i++
			continue
		}
		end := strings.IndexByte(source[i+1:], '}')
		if end < 0 {
			return nil, fmt.Errorf("template: unclosed reference at byte %d", i)
		}
		end += i + 1
		body := source[i+1 : end]
		fields := strings.SplitN(body, ":", 3)
		ref := Reference{Step: "0", Name: fields[0]}
		if len(fields) > 1 {
			ref.Step, ref.Name = fields[0], fields[1]
		}
		if len(fields) > 2 {
			ref.Default, ref.DefaultPresent = fields[2], true
		}
		if ref.Step == "" {
			return nil, fmt.Errorf("template: empty step at byte %d; use 0 for this run", i)
		}
		if ref.Name == "" {
			return nil, fmt.Errorf("template: empty variable name at byte %d", i)
		}
		flush()
		t.parts = append(t.parts, part{ref: &ref, raw: source[i : end+1]})
		i = end + 1
	}
	flush()
	return t, nil
}

// References returns all lookups in source order.
func (t *Template) References() []Reference {
	refs := make([]Reference, 0)
	for _, p := range t.parts {
		if p.ref != nil {
			refs = append(refs, *p.ref)
		}
	}
	return refs
}

// Render substitutes each reference using lookup. A missing value uses an
// explicit default when present, otherwise the original placeholder verbatim.
func (t *Template) Render(lookup func(Reference) (string, bool)) string {
	var out strings.Builder
	for _, p := range t.parts {
		if p.ref == nil {
			out.WriteString(p.text)
			continue
		}
		value, ok := lookup(*p.ref)
		if ok {
			out.WriteString(value)
		} else if p.ref.DefaultPresent {
			out.WriteString(p.ref.Default)
		} else {
			out.WriteString(p.raw)
		}
	}
	return out.String()
}
