# culture-nodes-jira-bridge

An ordinary `kind: agent` actor-protocol bridge with one capability: post a
comment on a named Jira issue. It has no transition operation and exposes no
generic Jira request surface.

The bridge reads `JIRA_ACCOUNT_EMAIL` and `JIRA_API_TOKEN` directly from its
own process environment. Those values are deliberately not valid JSON config
keys and must never be placed in control-plane or runner configuration.

Invocation input is exactly:

```json
{"verb":"post_comment","issue":"EX-123","comment":"The change shipped."}
```

Register it through the ordinary actor registry path, for example:

```sh
deploy/prod/register-actor.sh company/jira-comment http://10.0.0.2:8089 NODES_ACTOR_JIRA_TOKEN
```

The worker-facing bearer token authenticates actor dispatch. It is distinct
from the Jira credential, which remains exclusively in `jira-bridge-jira.env`.

Run the offline suite with `python -m pytest adapters/jira/tests` when pytest
is installed.
