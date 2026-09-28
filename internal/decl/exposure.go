package decl

// The `exposes` list (task t30b, #328, owner decision d4): exposure is
// approved per variable. A declaration names, in its own body, each variable
// reference it may render into a wider audience than the variable came from;
// the variable's owner approves each entry. The entry is the reference as a
// template writes it, so an author copies it straight from `action.with`:
//
//	summary               the triggering event's own variable ({summary} or {0:summary})
//	1:summary             a lineage variable by distance ({1:summary})
//	jira-intake:reporter  a lineage variable by declaration name ({jira-intake:reporter})
//
// `{1:x}` and `{jira-intake:x}` may name the same value in a given lineage,
// but they are different references and so different entries: what is
// approved is exactly what the template says.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/decl/template"
)

// ParseExposureEntry parses one exposes entry into the reference it names.
// It is the same grammar as a template reference without braces or a
// default: `name` or `step:name`.
func ParseExposureEntry(entry string) (template.Reference, error) {
	if strings.ContainsAny(entry, "{} \t\r\n") {
		return template.Reference{}, fmt.Errorf("declaration exposes entry %q: write the reference without braces or spaces, e.g. \"summary\" or \"jira-intake:reporter\"", entry)
	}
	fields := strings.Split(entry, ":")
	ref := template.Reference{Step: "0", Name: fields[0]}
	switch len(fields) {
	case 1:
	case 2:
		ref.Step, ref.Name = fields[0], fields[1]
	default:
		return template.Reference{}, fmt.Errorf("declaration exposes entry %q: an entry is \"name\" or \"step:name\" and takes no default", entry)
	}
	if ref.Step == "" || ref.Name == "" {
		return template.Reference{}, fmt.Errorf("declaration exposes entry %q: both the step and the variable name are required", entry)
	}
	return ref, nil
}

// ExposureEntry is the canonical exposes entry for a template reference: the
// bare name for step 0, `step:name` otherwise. The default is not part of it.
func ExposureEntry(ref template.Reference) string {
	if ref.Step == "0" {
		return ref.Name
	}
	return ref.Step + ":" + ref.Name
}

// NormalizeExposes returns the canonical list: each entry in canonical form
// (`0:x` becomes `x`), deduplicated and sorted, so the list's order and
// spelling never change a declaration's digest. An entry that does not
// parse is kept verbatim (Parse has already refused it on every authoring
// path). An empty list normalizes to nil, which the canonical body omits.
func NormalizeExposes(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(entries))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if ref, err := ParseExposureEntry(e); err == nil {
			e = ExposureEntry(ref)
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// Lists reports whether d's exposes list carries entry (canonical form).
func (d Declaration) Lists(entry string) bool {
	for _, e := range NormalizeExposes(d.Exposes) {
		if e == entry {
			return true
		}
	}
	return false
}
