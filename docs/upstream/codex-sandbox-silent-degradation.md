# Draft: upstream issue for OpenAI Codex CLI

Not filed. Written for the operator to file (or discard) — it reports a
behavior of the Codex CLI, not of this repo. Local evidence was gathered on
`spark`, `thor`, and `orin` while running the economy-discord-graphs batch.

Related: this repo's issue #63 (the provisioning gap on thor/orin).

---

## Title

`--sandbox workspace-write` silently degrades to no sandbox when bubblewrap
cannot create a user namespace

## Environment

- Codex CLI v0.147.0
- Ubuntu 24.04, aarch64 (also reproduced on two other hosts of the same image)
- `kernel.apparmor_restrict_unprivileged_userns = 1` (Ubuntu 24.04 default)
- bubblewrap present at `/usr/bin/bwrap`

## What happens

Ubuntu 24.04 ships with an AppArmor policy that forbids unprivileged
processes from creating user namespaces unless a profile grants it. Under
that default, bubblewrap cannot start:

```console
$ sysctl -n kernel.apparmor_restrict_unprivileged_userns
1
$ bwrap --ro-bind / / --unshare-user true
bwrap: setting up uid map: Permission denied
```

Running Codex with an explicitly requested sandbox on such a host prints a
warning and then **proceeds anyway, unsandboxed**:

```console
$ codex exec --sandbox workspace-write "…"
…
sandbox: workspace-write [workdir, /tmp, $TMPDIR]
…
warning: Codex's Linux sandbox uses bubblewrap and needs access to create user namespaces.
```

The session header still reports `sandbox: workspace-write`. The run then has
whatever access the invoking user has.

## Why this is a bug rather than a rough edge

Three things compound:

1. **The requested guarantee is not delivered, and the header says it is.**
   `--sandbox workspace-write` is an explicit request for confinement. After
   the warning scrolls past — and in `exec` mode it scrolls past immediately,
   above the model's own output — the only durable signal is a header line
   that asserts the sandbox is active. An operator reading the log later has
   no way to tell a confined run from an unconfined one.

2. **It degrades open rather than closed.** For a flag whose entire purpose is
   restriction, the safe failure is to refuse to start. Automation that
   opts into a sandbox is precisely the automation that should not run
   without one. A caller who genuinely wants no sandbox already has
   `--dangerously-bypass-approvals-and-sandbox`, and that flag's name does
   the honest work of saying so.

3. **It is inconsistent across invocation paths.** On the same OS image we
   see a hard failure in one integration and warn-and-continue in another, so
   callers cannot rely on either behavior.

The practical shape of the problem: a supervising process may reasonably
decline `--dangerously-bypass-approvals-and-sandbox` and permit
`--sandbox workspace-write`, believing it has chosen the safer option — and
on these hosts the two are equivalent at runtime, with only the second
claiming otherwise.

## Suggested resolution

Any one of these would close it; the first is the one we would want:

- **Exit non-zero when a sandbox was explicitly requested and cannot be
  established.** Suggest the bypass flag in the error for callers who
  knowingly want it.
- Failing that, make the degradation impossible to miss: reflect it in the
  session header (`sandbox: none (requested workspace-write — bubblewrap
  unavailable)`) rather than only in a warning line, and in any machine
  -readable session metadata.
- Name the remediation in the warning. Today it states the requirement but
  not the fix. On Ubuntu 24.04 the options are an AppArmor profile for
  `bwrap`, or `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`
  (with its own trade-off, which is worth stating).
- Consider detecting the Ubuntu 24.04 default specifically — it is common
  enough that a targeted message would resolve most reports of this class.

## Willing to help

Happy to test a patch on the affected hosts.
