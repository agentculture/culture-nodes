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
