-- Declaration execution state. These tables remain independent of the
-- declaration catalog migration so the two workstreams can land separately.
-- The immutable declaration version and component digests are pinned here.
CREATE TABLE declaration_firings (
    id                    TEXT PRIMARY KEY,
    namespace_id          TEXT NOT NULL REFERENCES namespaces (id),
    event_id              TEXT NOT NULL REFERENCES signal_events (id),
    declaration_id        TEXT NOT NULL,
    declaration_version   TEXT NOT NULL,
    trigger_digest        TEXT NOT NULL,
    condition_digest      TEXT NOT NULL,
    action_digest         TEXT NOT NULL,
    lineage_id            TEXT NOT NULL,
    -- A re-mint is a new dispatch attempt on the same logical lineage entry.
    -- Readers count DISTINCT canonical_firing_id, never physical rows.
    canonical_firing_id   TEXT NOT NULL REFERENCES declaration_firings (id),
    remint_of_id          TEXT REFERENCES declaration_firings (id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT declaration_firings_remint_not_self CHECK (remint_of_id IS NULL OR remint_of_id <> id),
    CONSTRAINT declaration_firings_namespace_id_key UNIQUE (namespace_id, id)
);

-- An event delivered again through signal_event_watermarks has the same
-- signal_events.id. It cannot create a second initial firing of a declaration.
CREATE UNIQUE INDEX declaration_firings_event_declaration_key
    ON declaration_firings (namespace_id, event_id, declaration_id)
    WHERE remint_of_id IS NULL;
CREATE UNIQUE INDEX declaration_firings_remint_source_key
    ON declaration_firings (remint_of_id) WHERE remint_of_id IS NOT NULL;
CREATE INDEX declaration_firings_lineage_idx
    ON declaration_firings (namespace_id, lineage_id, created_at, id);

CREATE TABLE declaration_lineage_edges (
    namespace_id       TEXT NOT NULL REFERENCES namespaces (id),
    parent_firing_id   TEXT NOT NULL,
    child_firing_id    TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (parent_firing_id, child_firing_id),
    CONSTRAINT declaration_lineage_no_self_edge CHECK (parent_firing_id <> child_firing_id),
    FOREIGN KEY (namespace_id, parent_firing_id) REFERENCES declaration_firings (namespace_id, id),
    FOREIGN KEY (namespace_id, child_firing_id) REFERENCES declaration_firings (namespace_id, id)
);
CREATE INDEX declaration_lineage_edges_child_idx
    ON declaration_lineage_edges (namespace_id, child_firing_id);

CREATE TABLE declaration_minted_markers (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL,
    firing_id        TEXT NOT NULL,
    artifact_kind    TEXT NOT NULL,
    nonce            TEXT NOT NULL,
    mac              TEXT NOT NULL,
    -- Assigned by the provider after creation; filled from the action result.
    artifact_id      TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, firing_id) REFERENCES declaration_firings (namespace_id, id),
    CONSTRAINT declaration_minted_markers_nonce_key UNIQUE (firing_id, artifact_kind, nonce)
);
CREATE INDEX declaration_minted_markers_artifact_idx
    ON declaration_minted_markers (namespace_id, artifact_kind, artifact_id)
    WHERE artifact_id IS NOT NULL;

CREATE TABLE declaration_nodes (
    id                   TEXT PRIMARY KEY,
    namespace_id         TEXT NOT NULL,
    opening_firing_id    TEXT NOT NULL,
    node_name            TEXT NOT NULL,
    state                TEXT NOT NULL DEFAULT 'open'
                         CHECK (state IN ('open', 'closed', 'frozen')),
    deadline             TIMESTAMPTZ,
    deadline_remaining   INTERVAL,
    closed_reason        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, opening_firing_id) REFERENCES declaration_firings (namespace_id, id)
);
CREATE INDEX declaration_nodes_due_idx
    ON declaration_nodes (namespace_id, deadline, id) WHERE state = 'open' AND deadline IS NOT NULL;

-- Arrival order is assigned at freeze time and survives a switch rollback.
CREATE TABLE declaration_node_frozen_events (
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    node_id          TEXT NOT NULL REFERENCES declaration_nodes (id),
    event_id         TEXT NOT NULL REFERENCES signal_events (id),
    arrival_order    BIGINT NOT NULL CHECK (arrival_order > 0),
    arrived_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, event_id),
    CONSTRAINT declaration_node_frozen_events_order_key UNIQUE (node_id, arrival_order)
);

CREATE TABLE declaration_evaluations (
    id                    TEXT PRIMARY KEY,
    namespace_id          TEXT NOT NULL REFERENCES namespaces (id),
    event_id              TEXT NOT NULL REFERENCES signal_events (id),
    declaration_id        TEXT NOT NULL,
    declaration_version   TEXT NOT NULL,
    outcome               TEXT NOT NULL,
    reason                TEXT NOT NULL,
    firing_id             TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, firing_id) REFERENCES declaration_firings (namespace_id, id)
);
CREATE INDEX declaration_evaluations_event_idx
    ON declaration_evaluations (namespace_id, event_id, declaration_id, created_at);
