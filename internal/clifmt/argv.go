package clifmt

import "strings"

// HasJSONFlag reports whether argv contains --json or --json=<value>
// anywhere. Callers pre-scan raw argv with this *before* any flag parsing
// happens, so a parse-time failure (unknown verb, unknown flag) still
// renders through EmitError's JSON shape when the caller asked for it —
// flag parsing itself never gets a chance to observe --json if parsing
// fails first.
func HasJSONFlag(argv []string) bool {
	for _, a := range argv {
		if isJSONFlag(a) {
			return true
		}
	}
	return false
}

// StripJSONFlag returns argv with every --json / --json=<value> token
// removed, plus whether any were found. Verb dispatch operates on the
// stripped slice so --json is recognised no matter where in the
// invocation it appears, rather than only in a fixed position.
func StripJSONFlag(argv []string) (rest []string, found bool) {
	rest = make([]string, 0, len(argv))
	for _, a := range argv {
		if isJSONFlag(a) {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, found
}

func isJSONFlag(arg string) bool {
	return arg == "--json" || strings.HasPrefix(arg, "--json=")
}
