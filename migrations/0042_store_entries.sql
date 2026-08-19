-- 0042_store_entries.sql
--
-- The flow store's catalog (issue #192, plan task t7): a store entry is a
-- graph PLUS the evidence that proved it — the workflow's content digest,
-- the proving prod run ids, the deviation records recorded against it, and
-- the actor/runner capability requirements the graph pins. "Every node has
-- a contract. Every result has evidence" applies to the catalog too: an
-- entry with no evidence manifest is not an entry.
--
-- Expand-only (docs/adr/0002-migration-policy.md): one new table, its
-- indexes, nothing dropped, renamed, or tightened. An N-1 binary never
-- reads this table, so it simply has no store entries — the same "no
-- breaker" shape migrations 0020/0024/0033 document.
--
-- WHY THE GRAPH SOURCE IS EMBEDDED, NOT ONLY REFERENCED. The registry's
-- acceptance is "pull a flow proven on thor into a second control plane and
-- publish it without hand-editing digests" (#192). A digest alone is a
-- promise the importing plane cannot redeem — it has no workflow_versions
-- row to resolve it against. So the entry carries the workflow source
-- verbatim (format + bytes), captured at entry creation from the local
-- workflow_versions row it proves, making every entry self-contained:
-- graph + evidence, full fidelity (SCRUM-3 comment 10118's q6 decision).
--
-- WHY CAPABILITY REQUIREMENTS LIVE IN THE MANIFEST, NEVER IN THE GRAPH. A
-- pulled flow pins actor://…@sha256 and runner:// ids that do not exist on
-- the importing plane. The later import step (WP-F, t8) binds those
-- requirements to local registrations WITHOUT touching the graph document —
-- a graph rewrite would change the content digest and break "without
-- hand-editing digests". The evidence_manifest column is where those
-- requirements are declared; the graph bytes stay byte-identical across
-- planes.
--
-- IMMUTABILITY AND COLLISIONS. Rows are insert-only (records are immutable;
-- corrections append — PRD §10). The unique index includes `origin`, which
-- is the collision rule made structural: a locally-authored entry and a
-- pulled entry — even for the same name and the same content — are distinct
-- rows, so a pull can never overwrite or shadow a local addition (#192
-- acceptance (b)). Within one origin, entry_digest (the canonical digest of
-- the entry's own content) makes re-adding identical content idempotent
-- rather than duplicated, the same idempotent-by-digest shape workflow
-- publication uses.

CREATE TABLE store_entries (
    id                  TEXT        PRIMARY KEY,
    namespace_id        TEXT        NOT NULL REFERENCES namespaces (id),

    -- Human-facing flow name (usually the workflow key). Deliberately NOT
    -- unique: two entries may share a name across origins (the coexistence
    -- rule) and even within one origin across revisions of the flow.
    name                TEXT        NOT NULL,

    -- 'local'  = authored on this control plane, evidence checked here;
    -- 'pulled' = ingested verbatim from another registry, evidence carried
    --            as the source plane recorded it.
    origin              TEXT        NOT NULL CHECK (origin IN ('local', 'pulled')),

    -- Where a pulled entry came from. NULL for local entries; required for
    -- pulled ones. A registry address is data on the row, never
    -- configuration baked into code (SCRUM-3 comment 10106's q1 decision).
    source_registry     TEXT,

    -- The graph, self-contained: the workflow's content digest plus the
    -- exact source bytes that digest to it.
    graph_digest        TEXT        NOT NULL,
    graph_source_format TEXT        NOT NULL,
    graph_source        TEXT        NOT NULL,

    -- Full-fidelity evidence manifest (JSON): proving_run_ids,
    -- deviation_records, required_capabilities. Shape is pinned by
    -- internal/store/postgres's EvidenceManifest and the OpenAPI
    -- StoreEvidenceManifest schema.
    evidence_manifest   JSONB       NOT NULL,

    -- Canonical content digest of the entry itself (name + graph +
    -- manifest), computed by internal/contracts. What makes re-adds
    -- idempotent and lets a pulling plane verify integrity end-to-end.
    entry_digest        TEXT        NOT NULL,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT store_entries_pulled_have_source
        CHECK (origin = 'local' OR source_registry IS NOT NULL)
);

-- The collision rule: identity is (namespace, origin, content). Same
-- content in both origins coexists; identical content twice in one origin
-- resolves to the existing row.
CREATE UNIQUE INDEX store_entries_namespace_origin_entry_digest_key
    ON store_entries (namespace_id, origin, entry_digest);

CREATE INDEX store_entries_namespace_id_name_idx
    ON store_entries (namespace_id, name, created_at);
