package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// explainCatalog is verbatim markdown keyed by a space-joined command path,
// mirroring culture_nodes/explain/catalog.py's tuple-keyed ENTRIES. The
// empty path and "nodes" both resolve to the root entry.
var explainCatalog = map[string]string{
	"":      explainRoot,
	"nodes": explainRoot,

	"whoami":   explainWhoami,
	"learn":    explainLearn,
	"explain":  explainExplain,
	"overview": explainOverview,
	"doctor":   explainDoctor,

	"cli":          explainCLI,
	"cli overview": explainCLI,

	"serve":            explainServe,
	"scheduler":        explainScheduler,
	"worker":           explainWorker,
	"all":              explainAll,
	"validate":         explainValidate,
	"run":              explainRun,
	"plan-import":      explainPlanImport,
	"chain-verify":     explainChainVerify,
	"cutover-adopt":    explainCutoverAdopt,
	"expire-approvals": explainExpireApprovals,
	"inspect":          explainStubMode("inspect", "inspect ledger records for a run"),
}

const explainRoot = `# nodes

The Culture Nodes control-plane CLI: the agent-first entry point for the
durable, ledger-native workflow orchestrator described in
docs/initial-design/.

## Verbs

- ` + "`nodes whoami`" + ` — identity probe from culture.yaml.
- ` + "`nodes learn`" + ` — structured self-teaching prompt.
- ` + "`nodes explain <path>`" + ` — markdown docs for any verb/mode.
- ` + "`nodes overview`" + ` — one-paragraph descriptive snapshot.
- ` + "`nodes doctor`" + ` — environment/identity checks.
- ` + "`nodes cli overview`" + ` — describe the CLI surface.
- ` + "`nodes validate <file>`" + ` — compile a workflow and report diagnostics.
- ` + "`nodes run`" + ` — create a first-class ad-hoc run from an instruction.
- ` + "`nodes plan-import`" + ` — import an external plan's faithful view (task t22).
- ` + "`nodes chain-verify`" + ` — check a run's claim/decision chain (task t24).

## Process modes

serve, scheduler, worker, all. (inspect is recognized but not yet
implemented.)

## Exit-code policy

- ` + "`0`" + ` success
- ` + "`1`" + ` user-input error
- ` + "`2`" + ` environment / setup error
- ` + "`3+`" + ` reserved

## See also

- ` + "`nodes explain whoami`" + `
- ` + "`nodes explain doctor`" + `
`

const explainWhoami = `# nodes whoami

Reports this binary's identity from culture.yaml: nick (` + "`suffix`" + `),
backend, and the CLI's own version. Falls back to the repo name when no
culture.yaml is found walking up from the current directory. Read-only.

## Usage

    nodes whoami
    nodes whoami --json
`

const explainLearn = `# nodes learn

Prints a structured self-teaching prompt covering purpose, the command map,
exit-code policy, --json support, and the explain pointer.

## Usage

    nodes learn
    nodes learn --json
`

const explainExplain = `# nodes explain <path>...

Prints markdown documentation for any verb/mode path. Unlike a per-command
usage line, explain is global and addressable by path — 'nodes explain cli
overview' resolves the same entry as running 'nodes cli overview --help'
would if that existed.

## Usage

    nodes explain nodes
    nodes explain whoami
    nodes explain cli overview
    nodes explain --json <path>...
`

const explainOverview = `# nodes overview

One-paragraph, read-only descriptive snapshot: this binary's identity (from
culture.yaml) plus its verb surface. Accepts and ignores a stray positional
argument so 'nodes overview <anything>' never hard-fails.

## Usage

    nodes overview
    nodes overview --json
`

const explainDoctor = `# nodes doctor

Environment/identity checks: the go binary is on PATH and reports a
version, and culture.yaml is present (found walking up from the current
directory). Reports a {check,status,detail} table; any failing check exits
2 — this is a domain outcome carried in the exit code and the result body,
not a CliError, so it still prints to stdout and stderr stays empty.

## Usage

    nodes doctor
    nodes doctor --json
`

const explainCLI = `# nodes cli

Noun group for CLI-surface introspection. 'cli overview' (and bare 'cli')
describe the CLI itself — distinct from the global 'overview', which
describes this agent/binary.

## Usage

    nodes cli overview
    nodes cli overview --json
`

const explainServe = `# nodes serve

Runs the Culture Nodes control-plane API (api/openapi/openapi.yaml,
internal/api) over HTTP: workflow publication, run orchestration, the
append-only work ledger, and review transactions, plus an SSE stream of
committed run events. Authless by design (spec decision c45) — meant to run
behind a private network or an authenticating proxy, never exposed directly.

Connects to PostgreSQL (NODES_DATABASE_URL), resolves the single default
namespace (creating it if absent), and serves until SIGINT/SIGTERM, then
shuts down gracefully. Does not apply schema migrations itself — run
'nodes migrate' first.

## Usage

    nodes serve
    nodes serve --listen :9000 --database-url postgres://...

## Environment

- ` + "`NODES_LISTEN`" + ` — listen address (default ` + "`:8080`" + `)
- ` + "`NODES_DATABASE_URL`" + ` — PostgreSQL connection URL (required)
`

const explainAll = `# nodes all

Local-development mode: everything ` + "`nodes serve`" + ` runs, plus the
scheduler (durable timers, retry availability, lease recovery — PRD §12.7)
in the same process. Worker wiring lands in a later task; internal/worker
carries no implementation on this branch, so 'all' here is serve +
scheduler only — stated here rather than left to be discovered.

## Usage

    nodes all
    nodes all --listen :9000 --database-url postgres://...

## Environment

Same as ` + "`nodes serve`" + `: ` + "`NODES_LISTEN`" + `, ` + "`NODES_DATABASE_URL`" + `.
`

const explainCutoverAdopt = `# nodes cutover-adopt

One-shot production cutover command. It reads pending Jira history cutover
markers, fetches each issue's current Jira Cloud REST v3 changelog/comment
head, and adopts that head without emitting signal facts. Re-running it skips
already-adopted rows. Run host-side after migrations and before resuming the
history sweep; never give Jira read credentials to a control-plane process.

## Environment

- ` + "`NODES_DATABASE_URL`" + ` — PostgreSQL connection URL.
- ` + "`JIRA_ACCOUNT_EMAIL`" + ` and ` + "`JIRA_API_TOKEN`" + ` — Jira Basic-auth pair.
`

func explainStubMode(name, summary string) string {
	return fmt.Sprintf(`# nodes %s

%s. Recognized by the CLI but not implemented yet — invoking it returns a
CliError (code 2, "not implemented yet") pointing at the build plan.

## Usage

    nodes %s
`, name, summary, name)
}

// knownExplainPaths lists the catalog's addressable paths for the
// remediation hint on an unknown path — "nodes" already stands in for the
// empty root path, so the empty key is skipped.
func knownExplainPaths() []string {
	paths := make([]string, 0, len(explainCatalog))
	for path := range explainCatalog {
		if path == "" {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func resolveExplain(path []string) (string, error) {
	key := strings.Join(path, " ")
	if markdown, ok := explainCatalog[key]; ok {
		return markdown, nil
	}
	display := key
	if display == "" {
		display = "<root>"
	}
	return "", &clifmt.CliError{
		Code:        clifmt.ExitUserError,
		Message:     fmt.Sprintf("no explain entry for: %s", display),
		Remediation: "known paths: " + strings.Join(knownExplainPaths(), ", "),
	}
}

func cmdExplain(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("explain")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("explain", err)
	}
	path := fs.Args()

	markdown, err := resolveExplain(path)
	if err != nil {
		return 0, err
	}

	if jsonMode {
		pathOut := path
		if pathOut == nil {
			pathOut = []string{}
		}
		if err := clifmt.EmitResultJSON(map[string]any{"path": pathOut, "markdown": markdown}); err != nil {
			return 0, err
		}
		return clifmt.ExitSuccess, nil
	}
	clifmt.EmitResult(markdown)
	return clifmt.ExitSuccess, nil
}

const explainExpireApprovals = `# nodes expire-approvals

One-shot backfill: expire pending human tasks whose subject pull request has
already been merged, with reason ` + "`pr_merged`" + `, routing each run down its
node's ` + "`expired`" + ` edge.

The periodic consumer inside the scheduler
(internal/humanfanout) already does this for every pull request the control
plane holds a delivered ` + "`pr.merged`" + ` fact for. This verb exists for the ones
it holds no fact for: the sweep only emits ` + "`pr.merged`" + ` when a merged pull
request's branch or body carries a correlatable Jira key, so an approval for a
PR without one waits forever with nothing able to notice.

## Usage

    nodes expire-approvals --namespace <id>
    nodes expire-approvals --namespace <id> --pr agentculture/culture-nodes#236 --apply

## Flags

- ` + "`--database-url`" + ` — PostgreSQL connection URL (defaults to NODES_DATABASE_URL).
- ` + "`--namespace`" + ` — the namespace to sweep, by id (defaults to NODES_NAMESPACE_ID).
- ` + "`--pr owner/repo#number`" + ` — repeatable. Selects pending approvals whose run
  subject is that pull request, WITHOUT requiring a delivered fact. The
  recorded expiry detail says an operator named it, because that is the
  provenance: a person's assertion, not a measured fact.
- ` + "`--apply`" + ` — actually write. Without it this is a dry run that prints
  exactly the task ids the same invocation would expire.
- ` + "`--limit`" + ` — maximum number of tasks to select (default 200).
- ` + "`--producer-actor-id`" + ` — the registered identity the derived expiry record
  is written under (defaults to NODES_HUMAN_TASK_EXPIRY_ACTOR_ID, then
  ` + "`human_task_expiry`" + `).

## Registration obligation

An expiry appends one ` + "`derived`" + ` ledger record, and
ledger_records.origin_actor_id is a foreign key to actors(id). Register the
producer BEFORE running this with --apply, or every expiry refuses:

    deploy/prod/register-actor.sh --engine human_task_expiry

## Exit codes

0 when every selected task expired (or when the dry run completed), 1 when a
task refused — an already-decided task, a terminal run, or a node whose
contract declares no ` + "`expired`" + ` outcome. A refusal is a domain outcome: it is
printed as a result on stdout, never as an error.
`
