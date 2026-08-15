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

**t1 + t2** (package P1, built by `codex-thor` run `01M022G18240VX3NX6NRT8HJCF`),
merged to `owe/batch` at `a6c9301`. TDD gate passed before and after: `go build`,
`go vet`, full `go test ./...`, adapter compile.

- t1 — `EnvironmentFile` seam on every claude and codex bridge unit;
  `install-secrets.sh` relays `GITHUB_TOKEN_WORKER` without fabricating it, host
  derived from the actor registration; early `.github/workflows/**` scope refusal.
- t2 — human-inbox lane adopts `culture-nodes-*` naming with the JSON config the
  running bridge actually reads; the deploy now stops, disables **and removes**
  the legacy unit files.

## 3. What is NOT done

24 of 26 tasks. Waves 1–7 untouched. **No live test (t25), no summary (t26), no
`/cicd`, no PR.**

Remaining wave 0: **t3, t15** (package P2, adapters) and **t4, t8** (package P3,
platform) — both dispatched twice and blocked both times, see §5.

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

## 5. The blocker to solve first

Both round-2 packages failed identically, after their write probes passed:

```text
git fetch origin owe/batch  ->  .git/FETCH_HEAD: Read-only file system
```

Under codex `--sandbox workspace-write` the **worktree is writable but `.git` is
read-only**. So an agent can edit files but cannot `fetch`, `commit`, or create a
ref.

Two consequences, the second far larger than the first:

1. **Operationally**, packages need `--sandbox danger-full-access` (what the
   agent checkout's own `AGENTS.md` recommends), or briefs must avoid git
   entirely and be self-contained.
2. **For the spec**, the `git_ref` handover carrier decided in q9/c70 requires
   the producing agent to create a commit and a ref — exactly what this sandbox
   forbids. The two-carrier decision currently rests on either raising the
   sandbox (weakening confinement) or having the operator create the ref, which
   reintroduces the human-as-transport the batch exists to remove. **This is an
   open decision, recorded as a `risky` deviation.**

## 6. Fleet facts (measured, do not re-derive)

| Fact | Value |
|---|---|
| Control plane | `http://192.168.1.146:18080` |
| `company/codex-thor` | `http://192.168.1.146:8086`, checkout `/home/thor/git/culture-nodes-agent` |
| `company/codex-orin` | `http://192.168.1.138:8086`, checkout `/home/orin/git/culture-nodes-agent` |
| Both checkouts | clean, at `7519d74` |
| Allowlist | **exactly one path per codex bridge**, exact-match — hence one package per actor at a time (deviation d1) |
| Agent commit policy | `AGENTS.md:22` — *"Never `git commit` or `git push` from a session"*; the operator harvests the diff |
| Harvest command | `ssh <host> 'cd <checkout> && git add -N . >/dev/null 2>&1; git diff HEAD --binary'` |

**`AGENTS.md:22` contradicts the `git_ref` carrier.** t6 requires an agent to
produce a commit; the policy forbids it. Unreconciled.

## 7. Deviations recorded

| id | what |
|---|---|
| d1 | Serialize to one package per codex actor — one allowlisted checkout each |
| d2 | Refresh both agent checkouts; they were two commits behind |
| d3 | Commit and push spec + plan so briefs cite them rather than inlining — **done** |
| d4 | Reconcile decision c26 against the committed unit tests (narrowed an over-broad `Environment=` ban to its real intent) |
| d4* | Sandbox posture: `workspace-write` leaves `.git` read-only (see §5), classified `risky` |

*Two records display as `d4`; read `devague deviate --list` for the authoritative ids.*

## 8. Next actions, in order

1. **Decide the sandbox question (§5).** It gates everything downstream and
   changes what t6 can be.
2. Re-dispatch P2 (t3, t15 → `codex-orin`) and P3 (t4, t8 → `codex-thor`) with
   the chosen sandbox. One package per actor.
3. Harvest → apply to a `owe/<pkg>` worktree under
   `../.worktrees.culture-nodes/` → TDD gate → merge into `owe/batch` →
   `git worktree remove`.
4. Continue waves 1–7 per `devague plan waves`.
5. t24 self-test → t25 live test → t26 summary → `/cicd` → PR against `main`.

**Every PR bumps the version** (`/version-bump`) — the `version-check` job blocks
merge otherwise. Not yet done for this batch.

## 9. Issues opened this cycle

- [#88] widen SonarCloud beyond `culture_nodes`, measure a baseline, ratchet
- [#89] run scope → think → challenge through Culture Nodes as a workflow
- [#90] worker push credential: permission, delivery seam, verification —
  **carries a correction comment**; its original root-cause analysis was wrong

Queued, not yet filed: the sandbox/`git_ref` contradiction (§5), and the
`AGENTS.md`-versus-`git_ref` contradiction (§6).

[#87]: https://github.com/agentculture/culture-nodes/issues/87
[#88]: https://github.com/agentculture/culture-nodes/issues/88
[#89]: https://github.com/agentculture/culture-nodes/issues/89
[#90]: https://github.com/agentculture/culture-nodes/issues/90
