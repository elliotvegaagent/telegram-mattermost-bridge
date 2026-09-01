CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS bridge_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_state (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS inbox_events (
    event_id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    kind TEXT NOT NULL,
    source_message_id TEXT NOT NULL,
    payload_json TEXT,
    status TEXT NOT NULL DEFAULT 'accepted',
    received_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER
);

CREATE TABLE IF NOT EXISTS outbox_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE REFERENCES inbox_events(event_id),
    target TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at_ms INTEGER NOT NULL,
    progress_json TEXT NOT NULL DEFAULT '{}',
    last_error TEXT,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS outbox_due_idx
    ON outbox_jobs(state, next_attempt_at_ms, id);

CREATE TABLE IF NOT EXISTS message_links (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    mm_post_id TEXT NOT NULL,
    tg_chat_id INTEGER NOT NULL,
    tg_message_id TEXT NOT NULL,
    mm_root_id TEXT NOT NULL,
    tg_anchor_message_id TEXT NOT NULL,
    part_index INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    UNIQUE(mm_post_id, tg_chat_id, tg_message_id)
);

CREATE INDEX IF NOT EXISTS links_mm_idx ON message_links(mm_post_id);
CREATE INDEX IF NOT EXISTS links_tg_idx ON message_links(tg_chat_id, tg_message_id);
CREATE INDEX IF NOT EXISTS links_root_idx ON message_links(mm_root_id);
