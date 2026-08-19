# culture-nodes-jira-bridge

An ordinary `kind: agent` actor-protocol bridge with three narrow
capabilities: post a comment on a named Jira issue, perform one
bridge-allowlisted issue transition, or create an issue in a
bridge-allowlisted project. It exposes no generic Jira request surface.

The bridge reads `JIRA_ACCOUNT_EMAIL` and `JIRA_API_TOKEN` directly from its
own process environment. Those values are deliberately not valid JSON config
keys and must never be placed in control-plane or runner configuration.
`JIRA_TRANSITION_PROJECT_PREFIX` and `JIRA_TRANSITION_TARGET` set the transition
custody boundary when the process starts; for example, `SCRUM-` and `Done`.
`JIRA_CREATE_PROJECTS` (or the `create_projects` config list) sets the
creation custody boundary: a comma-separated list of EXACT project keys the
`create_issue` verb may target. It defaults to empty, which refuses every
creation — installing the code widens nothing until a deployment configures
the allowlist.

Invocation input is exactly:

```json
{"verb":"post_comment","issue":"EX-123","comment":"What should this do?","question_id":"q-01"}
```

or:

```json
{"verb":"transition_issue","issue":"SCRUM-17","target":"Done"}
```

or:

```json
{"verb":"create_issue","project":"SCRUM","summary":"Wire the new lane","description":"Optional.","issue_type":"Task"}
```

`description` and `issue_type` are optional (`issue_type` defaults to
`Task`); a `project` outside the configured allowlist is refused by name at
the bridge, before any Jira request is built. `GET /v1/capabilities`
(authenticated like invocations) advertises the verb list and the non-secret
custody configuration behind it — which transition and which creation
projects this deployment actually allows.

`question_id` is optional for ordinary notices and required by a question
round trip. The actor appends a fixed `[culture-nodes:jira-actor ...]` marker
to every comment. The sweep filters that marker as a self-echo even when the
deployed Jira credential belongs to a person; for questions the marker also
carries the identifier copied into the human answer event.

Register it through the ordinary actor registry path, for example:

```sh
deploy/prod/register-actor.sh company/jira-comment http://10.0.0.2:8089 NODES_ACTOR_JIRA_TOKEN
```

The worker-facing bearer token authenticates actor dispatch. It is distinct
from the Jira credential, which remains exclusively in `jira-bridge-jira.env`.

Run the offline suite with `python -m pytest adapters/jira/tests` when pytest
is installed.
