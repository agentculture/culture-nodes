package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Format names the authoring syntax of a submitted document.
type Format string

const (
	// FormatYAML is the authoring sugar. It is converted to JSON before any
	// other level runs, so every downstream diagnostic can speak in JSON
	// Pointers regardless of how the document was written.
	FormatYAML Format = "yaml"
	// FormatJSON is canonical (PRD §7: "JSON is authoritative").
	FormatJSON Format = "json"
)

// FormatForPath picks a format from a filename. Only an explicit .json
// extension selects JSON; everything else is read as YAML, which is safe
// because YAML 1.2 is a JSON superset — a .txt file holding JSON still parses.
func FormatForPath(path string) Format {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return FormatJSON
	}
	return FormatYAML
}

// toJSON converts a submitted document to JSON bytes. It returns a diagnostic
// rather than an error for anything the *document* got wrong, and a Go error
// only for a caller mistake (an unknown format).
func toJSON(source []byte, format Format) ([]byte, *Diagnostic, error) {
	switch format {
	case FormatJSON:
		if !json.Valid(source) {
			// Decode again to get the parser's own message, which names the
			// offset; json.Valid only answers yes or no.
			var probe any
			err := json.Unmarshal(source, &probe)
			return nil, parseDiagnostic("JSON", err), nil
		}
		return source, nil, nil
	case FormatYAML:
		converted, err := yaml.YAMLToJSON(source)
		if err != nil {
			return nil, parseDiagnostic("YAML", err), nil
		}
		return converted, nil, nil
	default:
		return nil, nil, fmt.Errorf("compiler: unknown source format %q (want %q or %q)", format, FormatYAML, FormatJSON)
	}
}

func parseDiagnostic(syntax string, err error) *Diagnostic {
	message := fmt.Sprintf("%s source could not be parsed", syntax)
	if err != nil {
		message = fmt.Sprintf("%s source could not be parsed: %v", syntax, err)
	}
	return &Diagnostic{
		Level:   LevelError,
		Path:    "",
		Code:    CodeSyntaxParse,
		Message: message,
		Hint:    fmt.Sprintf("fix the %s syntax; nothing downstream of the syntax level can run until the document parses", syntax),
	}
}

// isJSONObject reports whether data is a JSON object. A workflow document is
// always an object; a list or a scalar is a syntax-level mistake, not a schema
// violation worth a dozen structural complaints.
func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}
