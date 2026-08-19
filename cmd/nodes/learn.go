package main

import "github.com/agentculture/culture-nodes/internal/clifmt"

// learnText is the self-teaching prompt for `nodes learn`. It must mention
// purpose, the command map, exit codes, --json, and explain — the same
// agent-first rubric culture_nodes/cli/_commands/learn.py satisfies.
const learnText = `nodes — the Culture Nodes control-plane CLI.

Purpose
-------
Agent-first entry point for Culture Nodes, the durable ledger-native
workflow orchestrator described in docs/initial-design/. Today it exposes
introspection verbs (this command family), compiles workflow definitions
(validate), runs the control-plane API (serve, all), and recognizes — but
does not yet implement — the remaining process-lifecycle modes (scheduler,
worker, run, inspect); those land in later build-plan tasks.

Commands
--------
  nodes whoami             Identity from culture.yaml (nick, backend, version).
  nodes learn               This self-teaching prompt.
  nodes explain <path>...   Markdown docs for any verb/mode path.
  nodes overview            One-paragraph descriptive snapshot.
  nodes doctor               Environment/identity checks.
  nodes cli overview         Describe the CLI surface itself.
  nodes validate <file>      Compile a workflow definition; report diagnostics.
  nodes serve                Run the control-plane API (api/openapi/openapi.yaml).
  nodes all                  serve + scheduler, for local development.
  nodes scheduler|worker|run|inspect
                             Recognized process modes (not implemented yet).

Machine-readable output
------------------------
Every command supports --json, including parse-time failures (an unknown
command or an unrecognized flag). Errors in JSON mode emit
{"code","message","remediation"} to stderr. Stdout and stderr never mix:
results always go to stdout, errors and diagnostics always go to stderr.

Exit-code policy
-----------------
  0  success
  1  user-input error (bad flag, unknown command, unknown explain path)
  2  environment/setup error (missing tool, feature not implemented yet)
  3+ reserved

More detail
------------
  nodes explain nodes
  nodes explain <verb>
`

type learnCommand struct {
	Path    []string `json:"path"`
	Summary string   `json:"summary"`
}

type learnPayload struct {
	Tool        string            `json:"tool"`
	Version     string            `json:"version"`
	Purpose     string            `json:"purpose"`
	Commands    []learnCommand    `json:"commands"`
	ExitCodes   map[string]string `json:"exit_codes"`
	JSONSupport bool              `json:"json_support"`
}

func newLearnPayload() learnPayload {
	commands := []learnCommand{
		{Path: []string{"whoami"}, Summary: "Identity probe from culture.yaml."},
		{Path: []string{"learn"}, Summary: "This self-teaching prompt."},
		{Path: []string{"explain"}, Summary: "Markdown docs by verb/mode path."},
		{Path: []string{"overview"}, Summary: "One-paragraph descriptive snapshot."},
		{Path: []string{"doctor"}, Summary: "Environment/identity checks."},
		{Path: []string{"cli", "overview"}, Summary: "Describe the CLI surface itself."},
		{Path: []string{"validate"}, Summary: "Compile a workflow definition and report diagnostics."},
	}
	implementedModeSummaries := map[string]string{
		"serve": "Run the control-plane API (api/openapi/openapi.yaml).",
		"all":   "serve + scheduler, for local development.",
	}
	for _, mode := range processModes {
		summary := "Recognized process mode (not implemented yet)."
		if s, ok := implementedModeSummaries[mode]; ok {
			summary = s
		}
		commands = append(commands, learnCommand{Path: []string{mode}, Summary: summary})
	}
	return learnPayload{
		Tool:     "nodes",
		Version:  version,
		Purpose:  "Agent-first control-plane CLI for Culture Nodes, the durable ledger-native workflow orchestrator.",
		Commands: commands,
		ExitCodes: map[string]string{
			"0": "success",
			"1": "user-input error",
			"2": "environment/setup error",
		},
		JSONSupport: true,
	}
}

func cmdLearn(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("learn")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("learn", err)
	}

	if jsonMode {
		if err := clifmt.EmitResultJSON(newLearnPayload()); err != nil {
			return 0, err
		}
		return clifmt.ExitSuccess, nil
	}
	clifmt.EmitResult(learnText)
	return clifmt.ExitSuccess, nil
}
