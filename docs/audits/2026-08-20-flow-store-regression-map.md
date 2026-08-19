# Flow-store live-failure regression map

Task t12 for cycle issue #203. These tests pin the live failures cited by spec claim c12 and its s14 observation; related work is tracked in #193 and #197.

| Failure | Spec claim | Test file::test name | What it pins |
|---|---|---|---|
| The 10106 skip | c12 | `tests/test_pr_upkeep_sweep_jira.py::TestJiraHistoryReplay::test_two_comment_reply_replays_two_facts_with_cumulative_history_watermarks` | Two comments arriving inside one poll window emit as two ordered facts with advancing history watermarks. |
| Unheard board acts | c12 | `tests/test_pr_upkeep_sweep_jira.py::TestJiraEventNames::test_comment_consumer_trigger_exactly_matches_sweep_comment_event_name`; `internal/engine/commentconsumer_test.go::TestBareHumanCommentFactMintsConsumerRunInOneDelivery` | The committed consumer subscribes to the emitter's exact event name, and one bare human comment delivery mints a run without polling. |
| Cutover replay | c12 | `internal/store/postgres/signal_test.go::TestJiraHistoryCutoverAdoptsHeadWithoutReplayingRecordedHistory` | Recorded prod-shaped legacy watermark rows adopt the current history head and suppress pre-cutover transition/comment replay. |
| s14 marker-substring suppression | c12 / s14 | `tests/test_pr_upkeep_sweep_jira.py::TestJiraSelfEcho::test_human_quoting_actor_marker_is_not_silenced_when_bot_identity_is_known` | A non-bot-authored comment quoting the literal protocol marker still emits when bot account identity is configured. |
