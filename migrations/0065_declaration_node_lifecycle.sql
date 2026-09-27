-- 0065_declaration_node_lifecycle.sql
--
-- Task t12 (#328): the node lifecycle -- deadlines, technical-result
-- triggers, orphan-by-upgrade -- needs two facts 0060's declaration_nodes
-- did not yet carry.
--
-- reactor_declaration_id / reactor_declaration_version (c49/h33): when a
-- landing node opens, the currently active declaration (if any) whose
-- start node names it is snapshotted here. That is what lets a later
-- reaction tell a genuinely reactor-less terminal node (nothing was ever
-- meant to consume it, so silence there is normal) apart from a node whose
-- reactor was upgraded or removed out from under it (silence there is an
-- orphan, c49's "closes with a recorded 'orphaned by upgrade vN' reason").
-- Both columns are nullable: NULL means no active declaration reacted to
-- this node name at open time.
ALTER TABLE declaration_nodes ADD COLUMN reactor_declaration_id TEXT;
ALTER TABLE declaration_nodes ADD COLUMN reactor_declaration_version TEXT;

-- deadline_notified_at (h55): the guard that makes node.expired an
-- exactly-once emission. ExpireDue's UPDATE ... WHERE state = 'open' can
-- only ever succeed once per node (the state transition itself is the
-- guard), and this column records when it did, for an operator asking "did
-- this node's deadline actually fire" without having to search
-- signal_events by hand.
ALTER TABLE declaration_nodes ADD COLUMN deadline_notified_at TIMESTAMPTZ;
