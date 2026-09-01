CREATE TABLE IF NOT EXISTS reaction_state (
    source TEXT NOT NULL,
    route_id TEXT NOT NULL,
    source_message_id TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    emoji TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY(source, route_id, source_message_id, actor_id, emoji)
);

CREATE INDEX IF NOT EXISTS reaction_state_message_idx
    ON reaction_state(source, route_id, source_message_id, created_at_ms);
