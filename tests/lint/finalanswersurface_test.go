package testslint

import "testing"

// The five agent bridges share one terminal-object parser. Provider-specific
// final-text selection stays in mapping.py; this parser must not drift.
func TestAgentFinalAnswerParserIsByteIdentical(t *testing.T) {
	want := map[string]bool{"claude-code": true, "codex": true, "qwen": true, "pi": true, "colleague": true}
	var baseline string
	for _, pkg := range discoverAdapterPackages(t) {
		if !want[pkg.adapter] {
			continue
		}
		if !pkg.has(t, "final_answer.py") {
			t.Errorf("%s has no final_answer.py", pkg.adapter)
			continue
		}
		body := pkg.read(t, "final_answer.py")
		if baseline != "" && body != baseline {
			t.Errorf("%s final_answer.py differs from the shared parser", pkg.adapter)
		}
		baseline = body
		delete(want, pkg.adapter)
	}
	for adapter := range want {
		t.Errorf("%s agent bridge was not checked", adapter)
	}
}
