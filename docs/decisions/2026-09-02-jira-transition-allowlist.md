# Jira transition allowlist

This decision supersedes the single-target custody described in
[Jira transition custody](2026-08-18-jira-transition-custody.md).

The Jira bridge reads the list form `JIRA_TRANSITION_TARGETS` and
`JIRA_TRANSITION_PROJECT_PREFIX` from its environment. The list is configured,
never defaulted by the bridge. A transition target must be an exact member and
an issue must match the configured prefix; either mismatch is refused while
parsing, before any Jira request.

Per-transition policy belongs to the engine. The bridge holds no policy about
when a permitted transition is appropriate; it enforces only custody through
the configured prefix and exact target membership.

`Pending` is the canonical waiting status.

`Done` is never automatic. A merge raises a human actor node after live
validation evidence is filed. Only that node's `done` outcome plans the
transition to `Done` (decision c32).
