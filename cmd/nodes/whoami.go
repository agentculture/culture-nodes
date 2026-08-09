package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// fallbackNick is used when no culture.yaml can be found — the repo's own
// name, mirroring culture_nodes/cli/_commands/whoami.py's _FALLBACK_NICK.
const fallbackNick = "culture-nodes"

// whoamiReport is the identity payload for `nodes whoami`.
type whoamiReport struct {
	Nick    string `json:"nick"`
	Version string `json:"version"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// findCultureYAML walks up from the process's current working directory
// looking for culture.yaml, stopping at the filesystem root. Unlike the
// Python CLI (which walks up from the installed package's own __file__ and
// so always finds *its* culture.yaml regardless of invocation directory), a
// compiled Go binary has no equivalent notion of "where it was installed
// from" — cwd-based discovery is the same convention `git`/`npm`/etc. use
// for locating a project root, and is what the conformance tests exercise.
func findCultureYAML() (path string, ok bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, "culture.yaml")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// readAgentFields extracts nick/backend/model from the first agent block in
// culture.yaml, without a YAML dependency (this module is stdlib-only) —
// same scalar-line scan as culture_nodes/cli/_commands/whoami.py's
// read_agent_fields/_scalar.
func readAgentFields() (nick, backend, model string) {
	nick, backend, model = fallbackNick, "unknown", "unknown"

	path, ok := findCultureYAML()
	if !ok {
		return nick, backend, model
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nick, backend, model
	}

	seenAgent := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- suffix:"), strings.HasPrefix(trimmed, "suffix:"):
			if seenAgent {
				// A second agent block starts here — stop at the first,
				// same as the Python implementation.
				return nick, backend, model
			}
			seenAgent = true
			nick = scalarAfter(trimmed, "suffix")
		case seenAgent && strings.HasPrefix(trimmed, "backend:"):
			backend = scalarAfter(trimmed, "backend")
		case seenAgent && strings.HasPrefix(trimmed, "model:"):
			model = scalarAfter(trimmed, "model")
		}
	}
	return nick, backend, model
}

// scalarAfter extracts the scalar value after "<key>:" on a culture.yaml
// line, stripping surrounding quotes. Returns "unknown" when the value is
// empty (mirrors the Python `_scalar` helper).
func scalarAfter(line, key string) string {
	_, after, found := strings.Cut(line, key+":")
	if !found {
		return "unknown"
	}
	value := strings.Trim(strings.TrimSpace(after), `'"`)
	if value == "" {
		return "unknown"
	}
	return value
}

func newWhoamiReport() whoamiReport {
	nick, backend, model := readAgentFields()
	return whoamiReport{Nick: nick, Version: version, Backend: backend, Model: model}
}

func cmdWhoami(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("whoami")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("whoami", err)
	}

	report := newWhoamiReport()
	if jsonMode {
		if err := clifmt.EmitResultJSON(report); err != nil {
			return 0, err
		}
		return clifmt.ExitSuccess, nil
	}
	text := fmt.Sprintf(
		"nick: %s\nversion: %s\nbackend: %s\nmodel: %s",
		report.Nick, report.Version, report.Backend, report.Model,
	)
	clifmt.EmitResult(text)
	return clifmt.ExitSuccess, nil
}
