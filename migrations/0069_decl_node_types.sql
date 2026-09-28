-- 0069_decl_node_types.sql
--
-- Task t38d (#328, owner decision d6): engine-derived node types, so a
-- declaration can start from open nodes by type (`start_from`) instead of
-- from one named start node.
--
-- Both columns describe the action that opened the node, and only the
-- declaration engine writes them, from facts it recorded itself
-- (internal/declengine/nodetypes.go):
--
--   actor_kind  'human' for a human.ask action and 'code' for a code.run
--               action -- the action kind the engine dispatched; otherwise
--               the registration of the actor the firing run's newest
--               attempt ran on: kind 'human', or the registration's
--               metadata.harness when it is one of the closed set below.
--   host        the machine that actor's registration records it runs on
--               (capabilities.preflight.host.hostname, the fact the bridge
--               measured and the registration copied).
--
-- Neither is ever read from an event payload, a declaration body, or any
-- other value an author or agent can write. A fact the engine did not
-- record leaves the column NULL -- never a guess -- and a NULL type
-- matches no typed start_from. Once set, a column is not rewritten.
ALTER TABLE declaration_nodes ADD COLUMN host TEXT
    CHECK (host IS NULL OR host <> '');
ALTER TABLE declaration_nodes ADD COLUMN actor_kind TEXT
    CHECK (actor_kind IS NULL OR actor_kind IN ('claude', 'codex', 'qwen', 'pi', 'colleague', 'human', 'code'));
