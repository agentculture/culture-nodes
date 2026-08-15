# What actually fences the handover, and why it is not a GitHub ruleset (#90, t9)

Status: proposed. Supersedes t9's assumed design, not its intent.

## What t9 assumed

t9 planned the handover as a GitHub push: the agent pushes to
`refs/culture-nodes/RUN-ID`, an org ruleset restricts the worker identity to
that namespace, and `scripts/verify-token-scope.sh` attempts an
out-of-namespace push and requires refusal. Two of its three acceptance
criteria describe that fence:

> a push to any ref outside `refs/culture-nodes/*` is REFUSED by the remote,
> demonstrated by attempting one
>
> `scripts/verify-token-scope.sh` is extended to attempt an out-of-namespace
> push and require refusal — a false positive here is worse than no fence

That design needs an org admin to create the ruleset, and a push credential
living on each agent host.

## What was measured instead

Two facts, both measured this cycle, remove the need for either.

**1. The fleet already has a git transport that needs no credential.**

```console
$ git push ssh://thor/home/thor/git/culture-nodes-agent HEAD:refs/heads/ctb-base
 * [new branch]      HEAD -> ctb-base
$ git ls-remote ssh://orin/home/orin/git/culture-nodes-agent
c407fa8e…  HEAD
7519d740…  refs/heads/main
```

Both directions, no GitHub involved, no token. Every work package this cycle
moved this way. Recorded as deviation d1.

**2. The dispatch sandbox cannot push anywhere at all.**

The codex sandbox has no network egress. Run
`01M039NZ2TZYFG68YZT93A6DC7` found `gh issue list` unable to reach
`api.github.com` and `pypi.org` failing DNS resolution — from a host where
`gh auth status` reports a logged-in account for an ordinary shell user.
Recorded as deviation d6.

The checkout's `origin` is still
`https://github.com/agentculture/culture-nodes`, so a session may *attempt*
`git push origin`. It cannot succeed: there is no route out of the sandbox.

## So the fence is the sandbox, not the remote

Put together, the handover is fenced more tightly than the ruleset would have
fenced it:

| | ruleset design | what ships |
|---|---|---|
| What a session may push to | `refs/culture-nodes/*` on GitHub | nothing — no egress |
| What enforces it | a server-side rule an admin configures | the sandbox the session already runs in |
| What must be provisioned | an org ruleset + a push credential per host | nothing |
| Who can widen it | anyone who can edit the ruleset | anyone who can change the sandbox |

A session commits locally; the operator **pulls** over ssh from outside the
sandbox. Publication stays a separate authority held by the operator, which is
what `preserve.handover_ref`'s own docstring already says it is for: "AGENTS.md
permits creating the commit and the ref, and forbids pushing".

## What this costs, stated plainly

This is a weaker guarantee in one specific way, and it should not be sold as
equivalent.

A ruleset is enforced by the remote and survives a mistake on the agent host.
The sandbox is enforced locally, so anything that restores egress inside a
dispatch — a config change, a codex-cli release that changes the default, a
host running the bridge outside its sandbox — silently removes the fence, with
no error and nothing to notice. `verify-token-scope.sh` was meant to catch a
credential that had quietly widened; nothing here catches a sandbox that has.

The honest statement is therefore: **the handover is unfenced by design and
unreachable in practice.** If a bridge ever runs where a dispatch has network,
the ruleset design comes back, and t9's two criteria come back with it.

The cheap check that would notice: a probe dispatch that attempts
`git push origin HEAD:refs/culture-nodes/probe` and requires failure — the
same shape t9 wanted, aimed at the sandbox rather than at the remote. Not
built here; it needs one billable dispatch per check and belongs with the
capability surface work in #96, which already has to answer "what can this
dispatch actually reach".

## What did ship, and is proven

t9's third criterion — "build packages dispatch workspace-write with the
measured `.git` widening; verification packages stay read-only" — is done and
demonstrated live.

`input.handover` now reaches both the sandbox widening and the ref plumbing,
which nothing had ever set. Run `01M03CD5V0WE9CBSHBM1Y3E9S7`, dispatched with
`--handover`, produced the first commit any codex session has authored in this
fleet:

```console
$ ssh thor 'cd /home/thor/git/culture-nodes-agent && git log --oneline -2'
4028d33 probe: codex commits its own work (t9/#90)
dfce0f2 t9: wire the handover opt-in, so a codex session can commit the work it did
```

Verified by the operator over ssh rather than taken from the session's report.
Verification dispatches continue to run read-only and to receive no widening,
which the same run's sibling dispatches show.
