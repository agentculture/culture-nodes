# own-the-work-end-to-end — run state

**Purpose.** A resumable record of the in-flight fan-out for the eleven-issue
batch behind [#87]. Written at a deliberate compaction point so the work can
continue after a context refresh without anything living only in an operator's
head.

**Note the irony, deliberately.** Success signal 5 of this very batch is *"this
cycle's `-STATE.md` would not have needed to exist."* This file exists because
the capability that would remove the need — a run that explains itself — is
precisely what is being built and has not landed. Its existence is evidence for
the batch, not against it. When t25/t26 run, that signal is answered by
comparing what a reader could reconstruct from the runs alone against what is
written here.

---

## 1. Where the artifacts are

| Artifact | Location |
|---|---|
| Spec (converged) | `docs/specs/2026-08-14-own-the-work-end-to-end.md` — 75 claims, 54 honesty conditions, 49 scope entries |
| Plan (converged) | `docs/plans/2026-08-15-own-the-work-end-to-end.md` — 26 tasks, 8 waves, 100 coverage targets |
| Frame / plan / deviation state | `.devague/{frames,plans,deliveries}/own-the-work-end-to-end.json` |
| Integration branch | `owe/batch` — pushed to origin |
| `main` | untouched at `7519d74`; **main is PR-protected** (`Protect main` ruleset targets `~DEFAULT_BRANCH`, requires PR, blocks deletion + non-fast-forward) |

Read plan state with `devague plan show`, deviations with `devague deviate --list`.

## 2. What is merged

**Waves 0 and most of 1**, on `owe/batch`. The TDD gate ran before and after
every merge, on spark — never on the agent host that built it.

| Task | Issue | Built by | Run |
|---|---|---|---|
| t1 | #72 | codex-thor | `01M022G18240VX3NX6NRT8HJCF` |
| t2 | #84 | codex-thor | same |
| t3 | #83 | codex-orin | `01M023VRBAZWF9S3BK3JVZX2E3` |
| t15 | #77 (partial) | codex-orin | same |
| t8 | #80 (partial) | codex-thor | `01M023WD51EZDCNJSNW69MW43Y` |
| t4 | — | codex-thor | `01M026D8P5V97PH0YRZ3MZ1YB4` |
| t5 | #79 | codex-orin | `01M026V7GE2NFXAEXWKAHBFQFY` |
| t9 | #82 | codex-thor | `01M027SM6QM7TF1764S52B7NG5` |
| t14 | #77 | codex-orin | `01M027V2QRAWW21MF8S2CFQ9QT` |
| t19 | — | codex-orin | same |

Operator-lane commits: `ea1ce0c` (#91 resolution + `AGENTS.md` + credential
lint), `0c573fc` (q5 decision + `DefaultTokenTTL` docstring), plus the plan
coverage map.

**Known partials, deliberately left open:**

- **t8** — the continuation declaration compiles and its CEL evaluates;
  scheduling is **not** wired into attempt completion. Separately, `node.state`
  is hardcoded so the `while` predicate is always true ([#95]).
- **t15** — colleague and notify emit explicit model sentinels; human-inbox has
  no preflight capability surface at all, which the run flagged itself.

## 3. What is NOT done

16 of 26 tasks remain. **No live test (t25), no summary (t26), no `/cicd`, no
PR, no `/version-bump`.**

**In flight right now:**

| Pkg | Task | Actor | Run |
|---|---|---|---|
| P9 | t7 — artifact retention vs ledger immutability | codex-thor | `01M028PJEW3K0AXAVF5B25E6YS` |
| P10 | t16 workspace provisioner, t18 handover-gate design | codex-orin | `01M028R3ZMCRDZAA9341WNN3FZ` |

**`t6` is `proposed`, not confirmed** — rewriting its instruction with [#91]'s
measurements flipped a confirmed task back. It is now the plan's **only**
convergence gap: t6 covers c3, h48 and c69, so `devague plan confirm t6` closes
four reported gaps at once.

## 4. The credential — settled, do not re-litigate

`GITHUB_TOKEN_WORKER` in `.env` is **correct and sufficient**: fine-grained,
`Contents: Read and write` on all `agentculture` repos, active at the org, no
pending approval. Verified end to end:

```bash
TOKEN_ENV=GITHUB_TOKEN_WORKER REPO=agentculture/culture-nodes REQUIRE=write \
  PROBE_PUSH=/home/spark/git/culture-nodes scripts/verify-token-scope.sh
# -> push_dryrun: allowed, can_push: True, within_policy: True
```

**The trap that cost two misdiagnoses:** `gh` installs a *URL-scoped* credential
helper in `~/.gitconfig`
(`credential.https://github.com.helper = !/usr/bin/gh auth git-credential`), and
a configured helper **outranks `GIT_ASKPASS`**. Pushes silently used gh's `gho_`
OAuth token instead. **Every push must disable both helper forms:**

```bash
git -c credential.helper= -c credential.https://github.com.helper= \
  push https://x-access-token@github.com/agentculture/culture-nodes.git <branch>
# with GIT_ASKPASS pointing at a script that echoes $GITHUB_TOKEN_WORKER
```

`git config --get credential.helper` does **not** show URL-scoped helpers; use
`git config --get-regexp 'credential.*helper'`.

## 5. The sandbox — settled by measurement, [#91] closed

Under codex `--sandbox workspace-write` the **worktree is writable and `.git` is
read-only**. That is a codex carve-out (codex-cli 0.147.0), not a kernel
restriction. Adding one scoped entry lifts it:

```bash
codex exec --sandbox workspace-write \
  -c 'sandbox_workspace_write={writable_roots=["<checkout>/.git"]}'
```

Measured on thor: plain `workspace-write` → `Read-only file system`; with the
widening → `GIT_WRITABLE`, and a full write-tree/commit-tree/update-ref produced
commit `df7d974` at `refs/culture-nodes/probe`. So the `git_ref` carrier
(q9/c70) needs **neither** `danger-full-access` **nor** to be dropped. Build the
widening as opt-in per dispatch — a package that hands over no ref gets no
`.git` write. Recorded as deviation `d6`.

**Policy, decided by the repo owner:** `AGENTS.md` now permits a handover commit
and a ref under `refs/culture-nodes/<run-id>`. Push stays forbidden and nothing
may be committed onto a branch, so an agent's output is unreachable from any
branch until the operator or control plane moves it.

## 6. Fleet facts (measured, do not re-derive)

| Fact | Value |
|---|---|
| Control plane | `http://192.168.1.146:18080` |
| `company/codex-thor` | `http://192.168.1.146:8086`, checkout `/home/thor/git/culture-nodes-agent` |
| `company/codex-orin` | `http://192.168.1.138:8086`, checkout `/home/orin/git/culture-nodes-agent` |
| codex on the agent hosts | `~/.local/bin/codex`, codex-cli 0.147.0 — **not on the default `ssh` PATH** |
| Go on the agent hosts | **absent** — an agent there cannot run `go test`, so every Go claim needs the operator's gate |
| Allowlist | **exactly one path per codex bridge**, exact-match — hence one package per actor at a time (deviation d1) |
| Harvest command | `ssh <host> 'cd <checkout> && git add -N . >/dev/null 2>&1; git diff HEAD --binary'` |
| Checkout refresh | `git reset --hard HEAD && git clean -qfd && git fetch -q origin owe/batch && git checkout -q -B owe/batch origin/owe/batch` — required before **every** dispatch ([#93]) |
| `nodes-op.sh assign` | **watches by default and blocks**; background it with `nohup … &` |
| `nodes-op.sh ledger` | **truncates the claim** ([#92]); read the full text with `curl "$API/v1alpha1/runs/<id>/ledger"` piped through `python3 -c "…x['data']['statement']…"` |

## 7. Deviations recorded

| id | what |
|---|---|
| d1 | Serialize to one package per codex actor — one allowlisted checkout each |
| d2 | Refresh both agent checkouts; they were two commits behind |
| d3 | Commit and push spec + plan so briefs cite them rather than inlining |
| d4 | Reconcile decision c26 against the committed unit tests |
| d4* | Sandbox posture: `workspace-write` leaves `.git` read-only — **superseded by d6** |
| d5 | Self-contained briefs to unblock wave 0 without answering #91 |
| d6 | #91 resolved by measurement: keep `workspace-write`, widen `.git` per dispatch, permit local refs |
| d7 | Split t4: the lint lands with the file splits, not before. t8 lands its declaration and stays open |
| d8 | q5 settled on per-artifact read capabilities; the spec's TTL premise falsified |

`devague deviate --list` is authoritative for ids.

## 8. t25 needs a deploy first — measured, not assumed

The production stack on thor is **15 hours old**, predating this whole batch.
Probed live:

```bash
curl -o /dev/null -w '%{http_code}' -X POST \
  http://192.168.1.146:18080/v1alpha1/attempts/att_probe/artifacts
# -> 405   (401 would mean the route is deployed and rejecting the token)
```

So none of t5's artifact route, t9's deadline cancel, or t8's continuation is
running in production. **t25 cannot be a live test of this batch until
`owe/batch` is deployed to thor.**

Two consequences worth planning for:

- The deploy will itself exercise **t19**, which rewrote how `deploy.sh`
  derives the sweep source from the revision it ships. That is dogfooding, and
  it is also the first thing to check if the deploy misbehaves.
- The API service now `depends_on: minio: service_healthy` and constructs an
  S3-backed artifact router at startup (t5). A MinIO that is not healthy will
  now stop the API from starting, where previously it would not have. Both
  `prod-minio-1` and `prod-postgres-1` are currently healthy.

## 9. Next actions, in order

1. **`devague plan confirm t6`** — user-only, and it is now the plan's ONLY
   convergence gap: t6 covers c3, h48 and c69, so confirming it closes four
   reported gaps at once.
2. Harvest P9 (t7) and P10 (t16, t18) → apply to a worktree under
   `../.worktrees.culture-nodes/` → **run the full suite on spark** (the agent
   hosts have no Go, no npm, no working `uv run`) → merge → `git worktree remove`.
3. t6 itself, once confirmed — the two-carrier handoff, with #91's
   measurements already in its brief.
4. Waves 2–4 per `devague plan waves`.
5. t24 self-test → **deploy `owe/batch` to thor** (see §8) → t25 live test →
   t26 summary → `/cicd` → PR against `main`.

**Every PR bumps the version** (`/version-bump`) — the `version-check` job blocks
merge otherwise. Not yet done for this batch.

## 10. The merge gate is not a formality

Every build package so far has arrived with at least one failure the run could
not have seen, because **the agent hosts have no Go toolchain** and their
sandbox blocks socket creation for the Python loopback tests. Three of four
gate failures in this batch were tests the agent wrote and never executed.

Specifics worth remembering, because they will recur:

- A stale sibling test encoding the old semantics, missed while its neighbours
  were updated (codex capability tests, t3).
- An instruction satisfied in letter but not intent — `probes` kept injectable
  while being made to decide nothing, with module globals monkeypatched instead
  (t3; fixed by adding an injectable `capability_probe`).
- A fixture spliced at the wrong indentation, producing nine diagnostics none of
  which were about the feature (t8).
- A declaration validated in one direction only: `onExhausted` excused from
  needing a contract declaration, but never required to be *routed* (t8; fixed
  with `graph.continuation_exhausted_unrouted`).

## 11. Issues opened this cycle

- [#88] widen SonarCloud beyond `culture_nodes`, measure a baseline, ratchet
- [#89] run scope → think → challenge through Culture Nodes as a workflow
- [#90] worker push credential — **carries a correction comment**; its original
  root-cause analysis was wrong
- [#91] `.git` read-only under `workspace-write` — **closed**, resolved by
  measurement, see §5
- [#92] the operator surface truncates ledger claims, hiding the qualifying half
- [#93] every dispatch needs the operator to hand-prepare the agent checkout
- [#94] the capability surface reports `writable_paths` but not whether `.git`
  is writable

[#87]: https://github.com/agentculture/culture-nodes/issues/87
[#88]: https://github.com/agentculture/culture-nodes/issues/88
[#89]: https://github.com/agentculture/culture-nodes/issues/89
[#90]: https://github.com/agentculture/culture-nodes/issues/90
[#91]: https://github.com/agentculture/culture-nodes/issues/91
[#92]: https://github.com/agentculture/culture-nodes/issues/92
[#93]: https://github.com/agentculture/culture-nodes/issues/93
[#94]: https://github.com/agentculture/culture-nodes/issues/94
