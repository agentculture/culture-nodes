package compiler

import (
	"errors"
	"strings"
)

// errMalformedPointer is returned for a string that is not an RFC 6901 JSON
// Pointer at all, as opposed to one that is well-formed but resolves nowhere.
var errMalformedPointer = errors.New("not a valid JSON Pointer")

// parsePointer splits an RFC 6901 JSON Pointer into unescaped reference
// tokens. The empty pointer ("") denotes the whole document and yields no
// tokens. Anything that does not start with '/' — or that contains a '~' not
// followed by '0' or '1' — is malformed.
func parsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, errMalformedPointer
	}
	raw := strings.Split(p[1:], "/")
	tokens := make([]string, 0, len(raw))
	for _, token := range raw {
		unescaped, err := unescapePointerToken(token)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, unescaped)
	}
	return tokens, nil
}

func unescapePointerToken(token string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			b.WriteByte(token[i])
			continue
		}
		if i+1 >= len(token) {
			return "", errMalformedPointer
		}
		switch token[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", errMalformedPointer
		}
		i++
	}
	return b.String(), nil
}

// escapePointerToken escapes a token for use in a diagnostic's Path, so a
// binding key containing '/' or '~' still yields a pointer that resolves.
func escapePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

// pointerJoin builds a JSON Pointer from already-known-literal segments plus
// tokens that must be escaped. Segments are joined with '/'.
func pointerJoin(base string, tokens ...string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, token := range tokens {
		b.WriteByte('/')
		b.WriteString(escapePointerToken(token))
	}
	return b.String()
}
