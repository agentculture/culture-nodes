# Connection-principle audit

Status: **proposed current-state claim**. The aspirational t25 acceptance
criteria are false today: participant addresses are stored in configuration,
database rows, and dispatch records. The transport inversion has not yet made
the repository satisfy the principle.

## Stored and exposed participant addresses

| Site | What the command found | Classification |
|---|---|---|
| `migrations/0001_namespaces_and_identity.sql:46` | `nl -ba migrations/0001_namespaces_and_identity.sql \| sed -n '38,54p'` shows nullable `actors.endpoint_ref`. Actor registrations store the participant URL here. | **The inversion removes this.** Plan t24 contracts the actor address column after bridges dial in. |
| `migrations/0011_runner_invocations.sql:63-66` | `nl -ba migrations/0011_runner_invocations.sql \| sed -n '57,70p'` shows required `runner_invocations.endpoint`, explicitly retained to answer where the invocation runs. Each dispatch record therefore snapshots a participant address. | **The inversion removes this.** Plan t24 explicitly removes the invocation endpoint after t23 changes the transport. |
| `web/src/api/types.ts:417` | `nl -ba web/src/api/types.ts \| sed -n '411,421p'` shows `Actor.endpoint_ref?: string`, exposing the stored address in the web API type. | **The inversion removes this.** It must leave the public actor representation when the backing actor column contracts; it is not an independent store. |
| `.claude/skills/nodes-operator/scripts/nodes-op.sh:22` | `nl -ba .claude/skills/nodes-operator/scripts/nodes-op.sh \| sed -n '19,23p'` shows the default control-plane URL `http://192.168.1.146:18080`. | **This remains, for a distinct reason.** It addresses the control plane used by the operator, not a bridge participant. The bridge dial-in inversion does not eliminate clients' need to locate the API. The file is vendored and must not be edited here. |
| `.claude/skills/nodes-operator/scripts/nodes-op.sh:335` | `nl -ba .claude/skills/nodes-operator/scripts/nodes-op.sh \| sed -n '332,336p'` shows the operator surface selecting every actor's `endpoint_ref` from Postgres. | **The inversion removes this dependency.** Once t24 removes the column, this registry view must stop reading participant addresses. |
| `deploy/prod/register-actor.sh:37` | `rg -n 'https?://([0-9]{1,3}\\.){3}[0-9]{1,3}:[0-9]+' deploy .claude/skills/nodes-operator/scripts` finds the example participant URL `http://192.168.1.5:17070`. | **The inversion removes this participant-address input.** Dial-in registration must identify/authenticate a bridge without configuring where the control plane should dial it. |

The database sites above are the authoritative stores. Code that reads or
copies those fields is dependency surface rather than another storage site:
`rg -n 'endpoint_ref|runnerInvocationColumns' internal/store internal/worker
internal/api` identifies the registration insert and registry resolution for
`actors.endpoint_ref`, plus the runner-invocation persistence path that copies
`endpoint` into its row. Those readers/writers must change with t23/t24, but
this audit does not edit them.

## Deployment-config result

`rg -n --glob '*.yml' --glob '*.yaml' --glob '*.json' --glob '*.template'
--glob '*.service' --glob '*.env*' --glob '*.sh'
'https?://([0-9]{1,3}\\.){3}[0-9]{1,3}:[0-9]+' deploy
.claude/skills/nodes-operator/scripts` returns two literal LAN URLs:

- `.claude/skills/nodes-operator/scripts/nodes-op.sh:22`, the control-plane
  default, remains because it is not a participant address.
- `deploy/prod/register-actor.sh:37`, a bridge endpoint example, is scheduled
  for removal by the inversion.

Therefore the aspirational criterion that deployment grep return only LAN
bridge endpoints scheduled for removal is also false: the result includes the
control-plane API address, which remains. Loopback bind/test URLs and wildcard
listen addresses are not stored participant locations and are outside this
principle.

## Conclusion

`rg -n 'endpoint_ref|endpoint[[:space:]]+TEXT NOT NULL'
migrations/0001_namespaces_and_identity.sql
migrations/0011_runner_invocations.sql` demonstrates that the schema still
stores participant locations. This audit consequently records the connection
principle as **not satisfied**. T23/t24 are scheduled to remove the two stores
and their API/operator dependencies; the operator-to-control-plane address
remains by design.
