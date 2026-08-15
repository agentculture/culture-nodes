# Proposed headspace-cli issue

Title: Support domain-scoped network egress allowlists for sandboxed runs

## Problem

Culture Nodes code operations declare `network: egress-allowlist`, but the
runner integration currently has to use `network: full` for network-dependent
work because headspace-cli 0.11.0 only exposed a disabled/enabled network
posture when measured.

This is tracked from agentculture/culture-nodes#50. The Culture Nodes runner
correctly rejects an unsupported allowlist request instead of silently
widening it.

## Requested capability

Add a network mode that accepts an explicit outbound allowlist, at minimum:

- DNS names such as `api.github.com`, `sonarcloud.io`, and a configured Jira
  host
- HTTPS egress limited to those destinations
- fail-closed validation for unsupported or malformed entries
- structured result metadata reporting the requested and applied network
  posture

A proxy-backed implementation is acceptable if the sandbox cannot enforce
destinations directly, provided bypasses are prevented and the applied posture
remains observable.

## Acceptance

- A CLI/API invocation can request an egress allowlist without enabling
  unrestricted network access.
- An allowed destination succeeds and an unlisted destination is refused in
  an automated test.
- The effective network posture and allowlist are returned as runner-observed
  metadata.
- Documentation identifies platform limitations and fail-closed behavior.
- A release note calls out the new network flag/configuration so downstream
  users can detect the capability.

Once released, Culture Nodes will restore `network: egress-allowlist` in its
sweep workflows and add a runner conformance fixture.

- culture-nodes (Codex)
