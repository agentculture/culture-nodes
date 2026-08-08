package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/compiler"
)

// validatePayload is `nodes validate --json`'s stable result shape. Diagnostics
// is never null: a caller iterating the list should not have to special-case a
// clean document.
type validatePayload struct {
	Valid       bool                  `json:"valid"`
	Digest      string                `json:"digest"`
	Diagnostics []compiler.Diagnostic `json:"diagnostics"`
}

// cmdValidate compiles a workflow definition and reports every diagnostic.
//
// Stream and exit contract:
//
//	stdout, exit 0   the workflow compiles (warnings may still be reported)
//	stdout, exit 1   the workflow has error diagnostics
//	stderr, exit 1   the invocation itself was wrong (no file, too many args)
//	stderr, exit 2   the file could not be read
//
// An invalid workflow is a *domain outcome*, not a technical failure, so its
// diagnostics are a result on stdout — the same distinction `nodes doctor`
// draws for an unhealthy verdict (PRD §3.4).
func cmdValidate(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("validate")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("validate", err)
	}

	rest := fs.Args()
	if len(rest) != 1 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("validate takes exactly one workflow file, got %d", len(rest)),
			Remediation: "run 'nodes validate <file.yaml|file.json>'",
		}
	}
	path := rest[0]

	source, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's argument; reading it is the command.
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("cannot read workflow file %q: %v", path, err),
			Remediation: "check the path and that the file is readable",
		}
	}

	compiled, diagnostics, err := compiler.Compile(source, compiler.FormatForPath(path))
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("compiler failed on %q: %v", path, err),
			Remediation: fmt.Sprintf("this is a compiler fault rather than a problem with the file; file a bug at %s", clifmt.IssuesURL),
		}
	}

	valid := compiled != nil
	digest := ""
	if valid {
		digest = compiled.Digest
	}

	if jsonMode {
		if diagnostics == nil {
			diagnostics = []compiler.Diagnostic{}
		}
		if err := clifmt.EmitResultJSON(validatePayload{
			Valid:       valid,
			Digest:      digest,
			Diagnostics: diagnostics,
		}); err != nil {
			return 0, err
		}
	} else {
		clifmt.EmitResult(renderValidateText(compiled, diagnostics))
	}

	if valid {
		return clifmt.ExitSuccess, nil
	}
	return clifmt.ExitUserError, nil
}

// renderValidateText renders one line per diagnostic plus a summary line.
func renderValidateText(compiled *compiler.CompiledWorkflow, diagnostics []compiler.Diagnostic) string {
	var b strings.Builder
	for _, d := range diagnostics {
		path := d.Path
		if path == "" {
			path = "<document>"
		}
		fmt.Fprintf(&b, "%s %s %s: %s", d.Level, path, d.Code, d.Message)
		if d.Hint != "" {
			fmt.Fprintf(&b, " | hint: %s", d.Hint)
		}
		b.WriteByte('\n')
	}

	errorCount, warningCount := compiler.CountByLevel(diagnostics)
	counts := fmt.Sprintf("%s, %s", plural(errorCount, "error"), plural(warningCount, "warning"))
	if compiled == nil {
		fmt.Fprintf(&b, "invalid: %s\n", counts)
		return b.String()
	}
	fmt.Fprintf(&b, "valid: %s %s (%s)\ndigest: %s\n",
		compiled.Name, compiled.Version, counts, compiled.Digest)
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// explainValidate is the `nodes explain validate` entry. It lives here rather
// than in explain.go so the verb's implementation and its documentation move
// together.
const explainValidate = `# nodes validate

Compiles a workflow definition (YAML or JSON) and reports every diagnostic.
` + "`.json`" + ` is read as JSON; anything else is read as YAML, which is safe
because YAML is a JSON superset.

The compiler runs the PRD §11.4 validation levels in order — syntax,
structure, graph, contract, ledger, policy, owners — and produces the
normalized IR plus its content digest only when nothing blocks it.

## Usage

    nodes validate workflow.yaml
    nodes validate workflow.json --json

## Output

Text mode prints one line per diagnostic:

    <level> <json-pointer> <code>: <message> | hint: <remediation>

followed by a summary, and a ` + "`digest:`" + ` line when the workflow
compiles. JSON mode prints ` + "`{valid, digest, diagnostics[]}`" + `.
Diagnostics are always sorted by path, then code, so two runs over the same
file produce the same sequence.

## Exit codes

- ` + "`0`" + ` the workflow compiles (warnings do not block)
- ` + "`1`" + ` the workflow has error diagnostics, or the invocation was wrong
- ` + "`2`" + ` the file could not be read

An invalid workflow is a domain outcome, not a technical failure: its
diagnostics go to stdout, and stderr stays empty.
`
