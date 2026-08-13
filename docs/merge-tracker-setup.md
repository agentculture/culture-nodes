# Merge tracker setup

The merge tracker is the component that makes a human's real-world action
complete their task: when a human task declares an observable and the human
merges the PR, the tracker observes the merge and submits the outcome. The
person acts once. (Issue #54; spec claim c34 in
`docs/specs/2026-08-13-economy-discord-graphs.md`.)

This document is the operator's setup guide. It covers the public-repo path
(no credential), the private / higher-cadence path (a token), and how to
verify the thing actually works.

## What it is, and where it runs

The tracker is a small stdlib-only Python process living beside the
human-inbox bridge in `adapters/human-inbox`. It runs **outside the
culture-nodes deployment**, as a systemd user unit on the bridge host.

That placement is deliberate and load-bearing: **the control plane holds no
GitHub credential and never calls the GitHub API.** A CI gate enforces it
(`tests/lint/github_isolation_test.go` scans `internal/` and `cmd/`). If you
find yourself putting `GITHUB_TOKEN` into `prod.env` for the API, worker or
scheduler, something has gone wrong — it belongs only to the tracker.

The tracker never merges anything. It only reads whether a PR is merged. The
human still performs the merge, and the flow still holds zero merge
credentials.

## What it does each cycle

1. Reads the bridge's durable task store for open tasks.
2. Keeps only tasks whose input declares an observable, e.g.
   `observe: {kind: github_pr_merged, pr: 60}`. Tasks without one are never
   touched — they stay purely manual.
3. Asks GitHub whether each declared PR is merged
   (`GET /repos/{owner}/{repo}/pulls/{n}`), grouping duplicate watches into
   one request and stopping at a per-cycle request budget.
4. On `merged: true`, submits through the bridge's own submit surface with an
   observed marker, producing a `proposed` claim carrying
   `collection_method: github_pr_merged` and the merge commit SHA.
5. A PR **closed without merging** is left alone. Only the merged state is
   unambiguous enough to auto-complete; anything else is a human judgment
   call and stays on the manual lane.

## Path A — public repository, no credential (the default)

For a public repository, PR merge state is readable anonymously. No token,
no secret, nothing to rotate.

The cost is rate limit: GitHub allows **60 requests per hour** for
unauthenticated calls, per source IP. In practice that is fine for a handful
of parked tasks — one request per watched PR per cycle, deduplicated — and
tight if you watch many PRs on a short cadence. Size the cadence against the
ceiling:

```text
requests/hour  ≈  (distinct PRs watched)  ×  (3600 / poll_seconds)
```

With the default cadence and a couple of open merge gates you are far under
60. With 10 PRs on a 60-second cadence you would need 600, and you must
either slow the cadence or use Path B.

Install nothing extra:

```bash
deploy/prod/install-secrets.sh thor      # generates the bridge auth token
BRANCH=<your-branch> deploy/prod/deploy.sh thor
```

## Path B — private repository, or a higher cadence

Supply a token. It raises the ceiling to **5,000 requests/hour** and is the
only way to read a private repository's PRs at all.

### What the token needs

Read-only. The tracker never writes.

- **Fine-grained PAT** (preferred): repository access limited to the specific
  repository, with the single permission **Pull requests: Read**. Nothing
  else — not Contents: Write, not Administration, not workflow scopes.
- **Classic PAT**: `public_repo` for a public repository; `repo` for a
  private one. Note that classic `repo` is coarse — it grants write access
  the tracker will never use, which is a good reason to prefer fine-grained.
- **GitHub App installation token**: works too, and is the better long-term
  shape for an organization (short-lived, per-installation, revocable
  centrally) — but nothing in this repo issues or refreshes one today.

Set an expiry you are willing to rotate on. There is no automatic renewal.

### Installing it

The token is externally issued — `install-secrets.sh` never invents one. It
relays the value from its own environment over ssh stdin, so the secret never
appears in an argv, in shell history, or in this repository:

```bash
export GITHUB_TOKEN=<paste>                 # or source it from a gitignored .env
deploy/prod/install-secrets.sh thor
```

It lands in `~/.culture-nodes/human-inbox.env` on the host, mode `0600`,
read only by the tracker unit.

Beware the obvious typo: the variable must be exactly `GITHUB_TOKEN`. A
misspelled key is silently ignored and the tracker simply behaves as if no
token were set.

Then redeploy so the unit picks it up:

```bash
BRANCH=<your-branch> deploy/prod/deploy.sh thor
```

## Declaring the observable on a task

The tracker only acts on tasks that ask it to. Bind an `observe` input on the
human-actor node, with the PR number coming from wherever the flow produced
it:

```yaml
- id: merge-gate
  kind: agent
  uses: company/human-ops
  input:
    bindings:
      instruction: "Merge PR #{{ .nodes.open-pr.output.number }} when the review is green"
      observe:
        kind: github_pr_merged
        pr: "{{ .nodes.open-pr.output.number }}"
```

Every non-`instruction` input key is preserved verbatim into the bridge's
stored task, which is how the declaration reaches the tracker without any
bridge change. A task with no `observe` block keeps today's behavior exactly.

## Verifying it works

On the bridge host:

```bash
systemctl --user status human-inbox-bridge human-inbox-tracker
journalctl --user -u human-inbox-tracker -n 50 --no-pager
```

End to end, the honest test is the real one: park a human task with an
`observe` declaration on a real PR, merge that PR yourself, and watch the
task complete without you touching the inbox. Then read the ledger — the
completing record should be a `proposed` claim with
`collection_method: github_pr_merged` naming the merge commit, attributed to
the bridge actor, **not** an `observed`-authority record. The tracker is not
a measuring runner and does not claim to be.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Tracker unit absent after deploy | No `GITHUB_TOKEN` installed, on a build where the deploy lane still requires one (see issue #64) |
| Tracker starts, nothing ever auto-completes | The task carries no `observe` block, or the PR is closed-unmerged (never auto-completes by design) |
| `403` with a rate-limit message | Anonymous 60/hr ceiling hit — slow the cadence or move to Path B |
| Task completes but the claim says `human-submission` | It was submitted manually, not observed — the marker is what distinguishes them |
| Control plane trying to call GitHub | A real bug: the credential belongs only to the tracker. `tests/lint/github_isolation_test.go` should have caught it |

## Related

- Issue #54 — the merge IS the submission (the decision this implements)
- Issue #64 — running unauthenticated against public repos, and the
  higher-cadence/private setup this document describes
- `adapters/human-inbox/README.md` — the bridge's own configuration surface
- `examples/pr-upkeep/README.md` — the human-merges rule in a live flow
