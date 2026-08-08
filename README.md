# culture-nodes

A workflow front and framework for Culture. Composes mesh agents and their
verbs into node-based workflows — define a graph of nodes, run it across the
Culture mesh, and inspect what each node did.

> **Every node has a contract. Every result has evidence.**

## Status

The product — **Culture Nodes**, a durable, ledger-native workflow
orchestrator for agents, code, services, and people — is fully specified but
not yet implemented:

- [`docs/initial-design/culture-nodes-prd-spec.md`](docs/initial-design/culture-nodes-prd-spec.md)
  — the PRD and technical specification: graph model, contract-first nodes,
  the Devague-derived work ledger (proposed / confirmed / observed / derived
  authority), the headspace-cli code-runner boundary, runtime architecture,
  and delivery phases.
- [`docs/initial-design/culture-nodes-implementation-issue.md`](docs/initial-design/culture-nodes-implementation-issue.md)
  — the Phase 0/1 implementation issue (contracts + compiler, then a durable
  vertical slice).

What exists today is the repo's mesh-agent baseline, scaffolded from
`culture-agent-template`:

- **An agent-first CLI** (installed as `nodes`) cited from
  [teken](https://github.com/agentculture/teken) — the runtime package has no
  third-party dependencies.
- **A mesh identity** — `culture.yaml` (`suffix: culture-nodes`,
  `backend: colleague`) with the matching resident prompt file
  (`AGENTS.colleague.md`).
- **The vendored guildmaster/devague skill kit** under `.claude/skills/`,
  cite-don't-import. See [`docs/skill-sources.md`](docs/skill-sources.md).
- **A build + deploy baseline** — pytest, lint, the agent-first rubric gate,
  and PyPI Trusted Publishing wired into GitHub Actions.

## Quickstart

```bash
uv sync
uv run pytest -n auto              # run the test suite
uv run nodes whoami                # identity from culture.yaml
uv run nodes learn                 # self-teaching prompt (add --json)
uv run teken cli doctor . --strict # the agent-first rubric gate CI runs
```

## CLI

| Verb | What it does |
|------|--------------|
| `nodes whoami` | Report this agent's nick, version, backend, and model from `culture.yaml`. |
| `nodes learn` | Print a structured self-teaching prompt. |
| `nodes explain <path>` | Markdown docs for any noun/verb path. |
| `nodes overview` | Read-only descriptive snapshot of the agent. |
| `nodes doctor` | Check the agent-identity invariants (prompt-file-present, backend-consistency, skills-present). |
| `nodes cli overview` | Describe the CLI surface itself. |

Every command supports `--json`. Results go to stdout, errors/diagnostics to
stderr (never mixed). Exit codes: `0` success, `1` user error, `2` environment
error, `3+` reserved.

## Contributing

See [`CLAUDE.md`](CLAUDE.md) for the working conventions: the design ground
rules distilled from the PRD, the version-bump-every-PR rule, the `cicd` PR
lane, and the vendored-skills policy.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
