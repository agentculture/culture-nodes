# Jira transition custody

Superseded by [Jira transition allowlist](2026-09-02-jira-transition-allowlist.md).

Decision c19 of `hands-free-scrum-2-pickup` assigns narrow board-state custody to the Jira bridge.

The bridge may transition only issues whose key begins with the project prefix configured by
`JIRA_TRANSITION_PROJECT_PREFIX` (for this plan, `SCRUM-`) and only to the single transition name
configured by `JIRA_TRANSITION_TARGET`. Both values are read from the environment when the bridge
starts. An invocation outside either allowlist boundary is rejected while parsing, before any Jira
request is made.

This custody is bridge-enforced because an actor prompt or workflow document is mutable guidance,
not an authority boundary. Neither can widen board-state custody: changing the project prefix or the
single target requires an operator-controlled bridge configuration change and restart.
