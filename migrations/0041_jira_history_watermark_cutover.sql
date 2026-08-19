-- Expand-only cutover state for Jira's history-position emitter. Jira's
-- current changelog/comment head is not present in PostgreSQL, so deployment
-- creates one pending adoption marker per issue previously seen through the
-- legacy status/comment cursors.
CREATE TABLE jira_history_watermark_cutovers (
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    issue_source_key TEXT NOT NULL,
    watermark        JSONB,
    adopted_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, issue_source_key),
    CONSTRAINT jira_history_watermark_cutovers_adoption_pair CHECK (
        (watermark IS NULL) = (adopted_at IS NULL)
    ) NOT VALID
);

INSERT INTO jira_history_watermark_cutovers (namespace_id, issue_source_key)
SELECT DISTINCT namespace_id, regexp_replace(source_key, ':(status|comment)$', '')
FROM signal_event_watermarks
WHERE source_key ~ '^jira:[^:]+:[^:]+:(status|comment)$'
ON CONFLICT DO NOTHING;
