# Interim unrestricted network posture for headspace runs

- Status: proposed
- Culture Nodes issue: agentculture/culture-nodes#50
- Upstream issue: agentculture/headspace-cli#23
- Prior measured version: headspace-cli 0.11.0
- Current published version: 0.11.0 — re-checked from spark at disposition
  time, so the disposition below stands unchanged

Culture Nodes temporarily runs network-dependent sweep operations with
`network: full`, although their declared intent is `network:
egress-allowlist`. The runner rejected the unsupported narrower posture in run
`01KZXACT0E81R8QE8CSWNC3ET2`; widening is explicit rather than silent.

This deviation ends only after headspace-cli publishes an allowlist capability,
Culture Nodes restores the narrower workflow setting, and a conformance test
proves allowed egress succeeds while unlisted egress is refused. Before
issue #50 is closed, rerun:

```bash
curl -fsS https://pypi.org/pypi/headspace-cli/json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["info"]["version"])'
```

If that release exposes a network allowlist flag, #50 changes from cross-repo
tracking to in-repository adoption and conformance work.

The dispatched session could not make that decision — the command failed
because `pypi.org` could not be resolved from inside the codex sandbox, which
has no network egress even though `gh` is installed and authenticated for a
normal shell user on the same host. The operator ran it from spark instead:
0.11.0 is still the latest published release, so #50 remains cross-repo
tracking. That capability gap — a tool present on the host but unusable under
the dispatch posture — is the same failure mode t19 (#96) exists to surface,
observed here for `gh` and network rather than for `uv`.
