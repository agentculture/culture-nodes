# Jira question round trip

This fixture shows the t11 boundary: the narrow Jira actor posts a marked
question, the run parks without a held lease on `until.signal`, and the
next human comment is emitted by `pr-upkeep/sweep.py` with
`originating_question_id`. The wait output carries that immutable event into
`continue-leg`; its instruction and bindings make this a continuation of the
parked leg, not a new trigger-created unit of work.

The sweep supplies `source_key` and `watermark` to the event API. PostgreSQL
appends the answer event, advances that watermark, and wakes the subscription
in one transaction. `TestAnswerDeliveryRollsBackWatermarkAndEventTogether`
injects a failure between append and watermark write, then retries twice to
pin no-skip and no-re-emit restart behavior.

## The bounded ask/re-ask loop (task t14)

`workflow.yaml` no longer parks once and gives up: a reply that arrives but
does not resolve the question (`continue-leg.needs_clarification`) triggers
one bounded backoff-and-reask cycle (`ask-backoff`, 30 minutes, mirroring
`examples/development-loop`'s `gate-backoff`) before a second unresolved
reply routes to a human `approval` node instead of asking a third time. Two
total asks, mirroring `internal/repair`'s `MaxAttempts: 2` — the workflow's
own header comment records, at length and with file:line citations, exactly
which parts of that bound are engine-enforced today and which are declared
but measured-inert (`continue.while`/`bounds`/`onExhausted` compiles on a
signal-parked loop but the engine never evaluates it there — read the header
before assuming the `continue` block does what its name suggests). It also
settles a risk the build plan carried as unresolved: `until.signal` and
`until.duration` cannot be declared together (the runtime, not the schema,
refuses that), so pure silence — a question nobody ever replies to — still
has no timeout; only a reply that *arrives* and fails to resolve the
question is what this bound counts.

## Claim decisions round-trip through Jira (task t13)

The same ask/wait/resume channel this fixture demonstrates is reused,
unmodified, for a narrower purpose: letting a human decide a `proposed`
ledger record — confirm or reject it — by replying to a Jira comment,
instead of an operator running `scripts/decide-claims.py` by hand. This is
documented here, next to the round-trip mechanics it depends on, rather than
in `adapters/jira/README.md`: the Jira actor and its marker are generic (any
question can carry a `question_id`), and a reader trying to understand *how
a decision gets from a Jira reply into the ledger* needs the round-trip's
own wait/resume/correlation machinery in view at the same time as the
decision format, not a second document to cross-reference.

### The decision-comment format

A record's own id doubles as the round trip's `question_id` — no new
correlation mechanism is invented. The posted comment (built by
`decide_reply.decision_prompt_text`, the single source of truth this section
quotes rather than restates) reads:

```text
A ledger record is awaiting your decision: <record-id>

Reply with exactly one of: `approve <record-id>` or `reject <record-id>`
Any other reply will not be understood as a decision and this question
will be re-asked.

[culture-nodes:jira-actor question_id=<record-id>]
```

The last line is the jira-comment actor's own marker
(`adapters/jira/src/jira_bridge/mapping.py`'s `Comment.marked_text`,
documented in `adapters/jira/README.md`) — not authored by this example. It
is what lets `pr-upkeep/sweep.py`'s `jira_question_id_for_answer` resolve a
later reply back to `<record-id>`.

### The conservative parser

`decide_reply.py` (stdlib-only, a sibling of this directory's
`workflow.yaml`, in the same place `examples/pr-upkeep/sweep.py` lives
beside its own workflow) parses a reply against that format. Only a reply
whose entire, stripped body is EXACTLY `approve <record-id>` or
`reject <record-id>` — for the SAME record id the comment named — parses to
a `Decision`. Anything else — a typo'd verb, a wrong id, extra words, a
second line — parses to `None`, which every caller in the module treats as
a `ReAsk`: nothing is committed, and the same decision comment is meant to
be reposted (the caller's job; this module only decides whether to). See
`tests/test_decide_reply.py` for the exhaustive list of replies that must
NOT parse.

### Wiring the decision into the ledger

A parsed `Decision` is committed through the SAME
`POST /v1alpha1/runs/{id}/reviews` + `POST /v1alpha1/reviews/{id}/commit`
route pair `scripts/decide-claims.py` drives — `decide_reply.commit_decision`
loads that script as a module (`importlib.util.spec_from_file_location`, the
pattern this repo already uses for every hyphenated script under test —
see `tests/test_merge_gate.py`, `tests/test_pr_upkeep_sweep.py`) and calls
its `request()` function directly, so the authentication, JSON encoding, and
HTTP-error reporting live in exactly one place. The committed review's
`rationale` — the same field `decide-claims.py`'s `--why` has written since
task t30 — names the Jira comment the decision was transcribed from, so the
review record is traceable back to the reply that produced it.
