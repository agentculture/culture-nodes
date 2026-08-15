# t17 — the worktree reaper: decisions, and what it would do to spark today

Task t17 is the other half of t16. `workspace.provision()` mints one
detached worktree per writer beneath a permitted root; `reap.py` gives them
back. Everything below is either a decision recorded before the code, or a
measurement taken on spark on 2026-08-15.

Implementation, in `adapters/{claude-code,codex,colleague}/src/*/`
(byte-identical across the three, all-backends rule):

- `reap.py` — decides. Runs git read-only and returns records; nothing in
  it can change anything.
- `reclaim.py` — acts. `secure()`, `execute()`, `sweep()`, and nothing
  else. The split is deliberate: "the plan does not touch your worktrees"
  is checkable by reading which file a function is in, rather than by
  auditing every function in one long module. (It also keeps both halves
  under t4's 1000-line gate — `reap.py` was 1008 before the split.)

Plus the mirrored `tests/test_reap.py` (28 tests each) and
`tests/lint/workspacereaper_test.go`, which guards both halves.

## The refusal the design is built on

Probed, not assumed:

```console
$ git worktree remove <dirty>
fatal: '<dirty>' contains modified or untracked files, use --force to delete it
```

`--force` appears nowhere in `reap.py`. That is enforced on the source text
by `TestTheReaperNeverForces`, because it is not a property a Python test
can protect: a forcing reaper still refuses everything today's reaper
refuses — it just also destroys what it used to leave alone, so every
existing unit test stays green. The guard was mutation-checked (inserting
`"--force"` into the removal command fails it, naming the line).

`TestTheReaperDocstringQuotesTheRefusalItReliesOn` keeps git's own sentence
in the module, so the next reader can see the fact was measured.

## Decision 1 — the gate is positive evidence, never a silent success

A worktree is reapable only when the reaper can NAME a durable object that
outlives the directory. Preserve refs and branches live in the shared
`.git` and survive `git worktree remove` (spec finding s42), so the danger
is confined to work no ref holds.

| Evidence kind | Established by | Measured? |
|---|---|---|
| `branch` | HEAD is contained by the worktree's own branch ref | yes |
| `preserve_ref` | HEAD is contained by a ref under `preserve.py`'s prefix | yes |
| `merged` | HEAD is contained by a configured durable ref | yes |
| `reachable_ref` | HEAD is contained by some other ref | yes |
| `artifact` | a published artifact handle the caller asserts | **no** |

`artifact` carries `measured: false` on the record. The reaper cannot check
an artifact store, so it must not let a handed-in claim read like its own
observation — the PRD's authority model in miniature.

`git for-each-ref --contains <head>` returning nothing is evidence of
absence only when git answered; if git could not answer, that is the
blocker `reachability_unknown`, not "no evidence, therefore refuse for a
different reason". No evidence is ever inferred from a command that
happened not to fail.

## Decision 2 — a dirty worktree is REFUSED, unconditionally

Not "refused unless a preserve ref exists". A preserve commit is built with
`git add -A`, which **respects `.gitignore`** — so a preserve ref proves
nothing about ignored-but-untracked files (a `.env`, a build output, a
local config). Tree-equality against a preserve commit would therefore be a
proof about tracked content only, while the removal destroys everything.
There is no cheap proof that makes forcing safe, so the reaper does not
attempt one.

The cost is real and accepted: worktrees leak exactly where the most work
is in flight (the challenge doc's objection to c52). The mitigation is
visibility, not force — a refusal names the uncommitted paths (bounded to
20, with an exact count) so an operator can act, and the sweep's aggregate
`domain_outcome` goes to `retained` so a workflow can route on it.

An unreadable `git status` is also a refusal. "We could not look" and
"there is nothing there" must never produce the same decision.

## Decision 3 — clean, real commits, on NO branch: preserve first, then reap

This is the dangerous case the task called out, and we mint rather than
refuse.

Refusing is not the neutral option it looks like. Commits reachable only
from a detached worktree HEAD are exactly what `git gc` collects once the
reflog expires, so "leave it alone" loses the same bytes `--force` would,
only later and with nobody watching. Minting is close to free and entirely
non-destructive: `secure()` runs one `git update-ref` on a ref that does
not yet exist — `preserve.py`'s doctrine minus even the commit, since HEAD,
the index and the working tree are never written — and then reads the ref
back, because an `update-ref` that exited 0 is not the same fact as a ref
that exists. The mint is namespaced under the preserve prefix
(`preserve/reaped/<name>-<short sha>`), so rescued work is in the one place
an operator already looks.

Minting is what CONVERTS the case: the worktree is re-planned afterwards
and only reaps because it now has ordinary `preserve_ref` evidence. If the
mint fails, or the ref already exists (refusing to move an existing ref),
the worktree stays. `secure()` is a separate call from `plan()` — a plan
never mutates anything.

## Decision 4 — nested worktrees are refused, and the rule is not configurable

`culture-nodes/.claude/worktrees/web-ux-quick-wins` is clean, on a branch,
and 66 hours idle. It is still refused, because it is nested inside the
repository's own main checkout: whoever is dispatched at the repo root can
already read and write it, so it is not this reaper's to reclaim, and it
may be another writer's live workspace. The remediation is t16's — relocate
it to a sibling root, then reap it there.

The main checkout is an **implicit** containment root, deliberately not a
configured one. spark's four bridges now carry per-lane exact allowlists
that do not name the main checkout at all, so a config-driven check would
have left this hazard unnamed. Nesting is a fact about the filesystem, not
about anyone's configuration.

Ownership otherwise reuses the provisioner's own two lists rather than
inventing a parallel model: `repo_allowlist_prefixes` are the roots this
host mints into (a candidate must be a strict child of one),
`repo_allowlist` entries are other writers' roots.

## Decision 5 — how we know a worktree is idle, and where we cannot

Four signals, in descending strength:

1. **The session registry** (`active_workspaces`). Authoritative for
   sessions THIS bridge started, and nothing else. `None` means "I do not
   know what is running" and defers EVERY candidate; `()` is a positive
   statement that nothing is registered, and is the only way a sweep reaps
   anything. The standalone CLI has no registry, which is why reaping from
   it requires the operator to state it explicitly with
   `--reap-assume-idle`.
2. **git's own `locked` marker** — a positive busy signal git honours too.
3. **A process whose cwd is inside the worktree.** A POSITIVE detector
   only: a hit proves busy, a miss proves nothing, because
   `/proc/<pid>/cwd` is unreadable for other users' processes. On the live
   run below the probe could not read 415 pids; that number is recorded as
   `unreadable_pids` on every decision so the blind spot is visible rather
   than implied.
4. **Age**, default 24h. The weakest signal, and the only one that covers a
   session this host never started.

**We cannot know a worktree is idle**, and the policy is built to say so:
none of the four can see a session on another host sharing the filesystem.
So idleness is only ever a reason to DEFER — a worktree is reaped on
evidence, and idleness merely stops us acting on that evidence too early.
Anything unknown (no registry, no `/proc`, an un-stat-able tree, a
truncated scan) defers.

Two measurements shaped the age probe:

- **Reflogs are excluded.** All three stale worktrees on spark carried a
  `.git/worktrees/<id>/logs/HEAD` written within the same minute, ~18h
  after anyone touched them — `git gc`/`reflog expire` doing background
  maintenance. Counting that as activity would have deferred all three
  forever. Only the files a *user* operation writes count: `index`, `HEAD`,
  `ORIG_HEAD`, `COMMIT_EDITMSG`.
- **The reaper must not touch what it measures.** A plain `git status`
  refreshes and rewrites the worktree's index, so the first version of this
  module made every worktree it inspected look touched-just-now and then
  deferred it. Fixed twice over: the age probe runs *before* the
  cleanliness probe, and the cleanliness probe uses
  `git --no-optional-locks status`. Similarly, the age walk prunes `.git`
  directories — in a main checkout that is shared metadata every *other*
  worktree writes into.

## Decision 6 — failure is a routable domain outcome

Nothing in `reap.py` raises. Every path — git missing, a repo that is not a
repo, a removal git refused — produces a decision record with a
`domain_outcome` of `reclaimed`, `retained` or `deferred`, and leaves the
worktree in place. A sweep's aggregate outcome is the worst of its
decisions.

Removal is opt-in twice over: `plan()` is pure, `execute()` defaults to
`perform=False` and returns the exact argv instead. With `perform=True` the
decision is re-derived from live state first and the removal is abandoned
if the worktree was dirtied or HEAD moved since the plan — a plan is a
snapshot, and a writer can come back to life inside the gap.

## What it would do to spark right now

Read-only run, nothing removed (verified: `git worktree list` still shows
all ten entries afterwards):

```bash
CLAUDE_CODE_BRIDGE_REPO_ALLOWLIST_PREFIXES=/home/spark/git/.worktrees.culture-nodes \
  python3 -m claude_code_bridge \
  --reap-plan /home/spark/git/culture-nodes --reap-assume-idle
```

Aggregate: `retained` — `{refuse: 6, reap: 3, defer: 1}`.

The four the task named:

| Worktree | Decision | Idle | Reason |
|---|---|---|---|
| `.worktrees.culture-nodes/aehl-t13` | **reap** | 51.5h | evidence `branch`: `refs/heads/aehl/t13` contains `674998382c7c`; the branch survives the directory |
| `.worktrees.culture-nodes/aehl-t19` | **reap** | 51.4h | evidence `branch`: `refs/heads/aehl/t19` contains `a5e9495d72e1` |
| `.worktrees.culture-nodes/upkeep-s3516-node-runs` | **reap** | 47.2h | evidence `branch`: `refs/heads/upkeep/s3516-node-runs-invariant-return` contains `55577df52cfd` |
| `culture-nodes/.claude/worktrees/web-ux-quick-wins` | **refuse** | 66.7h | `nested_under_allowlisted_root`: inside `/home/spark/git/culture-nodes`, the repository's own main checkout — plus `outside_permitted_roots`. It is clean and has branch evidence; the refusal is about ownership, not risk |

Each `reap` carries its exact command, e.g.
`git worktree remove /home/spark/git/.worktrees.culture-nodes/aehl-t13`.
**The operator performs the removal.**

The other six are the live batch, and they are the policy working rather
than noise:

| Worktree | Decision | Reason |
|---|---|---|
| `culture-nodes` (main) | refuse | `main_worktree`, `reaper_own_worktree`, `outside_permitted_roots` |
| `owe-intake` | refuse | `reaper_own_worktree` (this lane) + 19 uncommitted paths |
| `owe-developer` | refuse | 12 uncommitted paths; also 9 live pids inside |
| `owe-planner` | refuse | 2 uncommitted paths (`examples/development-loop/`, a new lint test) |
| `owe-x2` | refuse | 19 uncommitted paths; also live pids inside |
| `owe-verifier` | defer | clean and on a branch, but `ORIG_HEAD` written 0.3h ago — inside the 24h floor |

`owe-verifier` is the interesting one: clean, branch evidence present,
nothing wrong with it — and still not reaped, purely because it is warm.
That is the conservative default doing its job. It is also the honest
caveat: had the idle floor been minutes rather than a day, a live lane
would have been a reap candidate on evidence alone, because no signal
available to this host proves a peer lane is working.

## Not built here

- **Artifact-handle evidence is accepted, not verified.** The reaper takes
  a handle from its caller and flags it unmeasured. Verifying it means
  reaching the artifact store from the bridge host, which is t5/t7's
  surface.
- **The follow-up node's workflow wiring.** `plan(..., only=[path])` is the
  single-candidate shape a post-writer node calls, and `sweep()` is the
  age-based orphan sweeper, but neither is declared as a node kind in a
  workflow schema yet.
- **The bridge server does not expose either over HTTP.** Both are reachable
  in-process and through the CLI flags; a route would need the
  authorization question t5 settled for artifacts answered again for
  destructive verbs.
