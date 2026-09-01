from __future__ import annotations

import importlib.resources
import json
import sqlite3
import threading
import time
from pathlib import Path

from .models import BridgeEvent, EventKind, MessageLink, OutboxJob, Platform


def now_ms() -> int:
    return int(time.time() * 1000)


class Storage:
    """Small synchronous SQLite seam; operations are short and transaction-scoped."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._connection = sqlite3.connect(path, check_same_thread=False, timeout=10)
        self._connection.row_factory = sqlite3.Row
        self._lock = threading.RLock()
        with self._connection:
            self._connection.execute("PRAGMA journal_mode=WAL")
            self._connection.execute("PRAGMA foreign_keys=ON")
            self._connection.execute("PRAGMA busy_timeout=10000")

    def close(self) -> None:
        with self._lock:
            self._connection.close()

    def migrate(self) -> None:
        migrations = importlib.resources.files("bridge.migrations")
        with self._lock, self._connection:
            self._connection.execute(
                "CREATE TABLE IF NOT EXISTS schema_migrations "
                "(version INTEGER PRIMARY KEY, applied_at_ms INTEGER NOT NULL)"
            )
            applied = {
                int(row[0])
                for row in self._connection.execute("SELECT version FROM schema_migrations")
            }
            for item in sorted(migrations.iterdir(), key=lambda entry: entry.name):
                if not item.name.endswith(".sql"):
                    continue
                version_text = item.name.split("_", 1)[0]
                if not version_text.isdigit():
                    continue
                version = int(version_text)
                if version in applied:
                    continue
                self._connection.executescript(item.read_text(encoding="utf-8"))
                self._connection.execute(
                    "INSERT INTO schema_migrations(version, applied_at_ms) VALUES (?, ?)",
                    (version, now_ms()),
                )

    def initialize(
        self,
        *,
        mm_checkpoint_ms: int,
        tg_offset: int = 0,
        route_ids: tuple[str, ...] = ("default",),
    ) -> None:
        timestamp = now_ms()
        with self._lock, self._connection:
            self._connection.execute(
                "INSERT INTO bridge_meta(key, value) VALUES ('initialized', 'true') "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value"
            )
            for key, value in (
                ("mattermost_checkpoint_ms", str(mm_checkpoint_ms)),
                ("telegram_offset", str(tg_offset)),
                *(
                    (f"mattermost_checkpoint_ms:{route_id}", str(mm_checkpoint_ms))
                    for route_id in route_ids
                ),
            ):
                self._connection.execute(
                    "INSERT INTO sync_state(key, value, updated_at_ms) VALUES (?, ?, ?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at_ms=excluded.updated_at_ms",
                    (key, value, timestamp),
                )

    def is_initialized(self) -> bool:
        with self._lock:
            row = self._connection.execute(
                "SELECT value FROM bridge_meta WHERE key='initialized'"
            ).fetchone()
        return bool(row and row["value"] == "true")

    def get_state(self, key: str, default: str | None = None) -> str | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT value FROM sync_state WHERE key=?", (key,)
            ).fetchone()
        return row["value"] if row else default

    def set_state(self, key: str, value: str) -> None:
        with self._lock, self._connection:
            self._connection.execute(
                "INSERT INTO sync_state(key, value, updated_at_ms) VALUES (?, ?, ?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at_ms=excluded.updated_at_ms",
                (key, value, now_ms()),
            )

    def enqueue(self, event: BridgeEvent) -> bool:
        timestamp = now_ms()
        with self._lock, self._connection:
            cursor = self._connection.execute(
                "INSERT OR IGNORE INTO inbox_events"
                "(event_id, source, kind, source_message_id, payload_json, status, received_at_ms) "
                "VALUES (?, ?, ?, ?, ?, 'accepted', ?)",
                (
                    event.event_id,
                    event.platform.value,
                    event.kind.value,
                    event.message_id,
                    event.to_json(),
                    timestamp,
                ),
            )
            inserted = cursor.rowcount == 1
            if inserted and event.kind is EventKind.REACTION_ADDED and event.reaction:
                self._connection.execute(
                    "INSERT INTO reaction_state"
                    "(source, route_id, source_message_id, actor_id, emoji, created_at_ms) "
                    "VALUES (?, ?, ?, ?, ?, ?) "
                    "ON CONFLICT(source, route_id, source_message_id, actor_id, emoji) "
                    "DO UPDATE SET created_at_ms=excluded.created_at_ms",
                    (
                        event.platform.value,
                        event.route_id,
                        event.message_id,
                        event.author_id,
                        event.reaction,
                        event.created_at_ms or timestamp,
                    ),
                )
            elif inserted and event.kind is EventKind.REACTION_REMOVED and event.reaction:
                self._connection.execute(
                    "DELETE FROM reaction_state WHERE source=? AND route_id=? "
                    "AND source_message_id=? AND actor_id=? AND emoji=?",
                    (
                        event.platform.value,
                        event.route_id,
                        event.message_id,
                        event.author_id,
                        event.reaction,
                    ),
                )
            if inserted and event.kind is not EventKind.IGNORED:
                self._connection.execute(
                    "INSERT INTO outbox_jobs"
                    "(event_id, target, state, next_attempt_at_ms, created_at_ms, updated_at_ms) "
                    "VALUES (?, ?, 'queued', ?, ?, ?)",
                    (event.event_id, event.platform.opposite.value, timestamp, timestamp, timestamp),
                )
            elif inserted:
                self._connection.execute(
                    "UPDATE inbox_events SET status='completed', payload_json=NULL, completed_at_ms=? "
                    "WHERE event_id=?",
                    (timestamp, event.event_id),
                )
            if event.checkpoint_key and event.checkpoint_value is not None:
                self._connection.execute(
                    "INSERT INTO sync_state(key, value, updated_at_ms) VALUES (?, ?, ?) "
                    "ON CONFLICT(key) DO UPDATE SET "
                    "value=CASE WHEN CAST(excluded.value AS INTEGER) > CAST(sync_state.value AS INTEGER) "
                    "THEN excluded.value ELSE sync_state.value END, "
                    "updated_at_ms=excluded.updated_at_ms",
                    (event.checkpoint_key, event.checkpoint_value, timestamp),
                )
        return inserted

    def active_reactions(
        self,
        source: Platform,
        route_id: str,
        source_message_id: str,
    ) -> tuple[str, ...]:
        """Return active distinct reactions, oldest to newest."""
        with self._lock:
            rows = self._connection.execute(
                "SELECT emoji, MAX(created_at_ms) AS latest FROM reaction_state "
                "WHERE source=? AND route_id=? AND source_message_id=? "
                "GROUP BY emoji ORDER BY latest, emoji",
                (source.value, route_id, source_message_id),
            ).fetchall()
        return tuple(str(row["emoji"]) for row in rows)

    def get_event(self, event_id: str) -> BridgeEvent:
        with self._lock:
            row = self._connection.execute(
                "SELECT payload_json FROM inbox_events WHERE event_id=?", (event_id,)
            ).fetchone()
        if not row or not row["payload_json"]:
            raise KeyError(event_id)
        return BridgeEvent.from_json(row["payload_json"])

    def fetch_due_job(self) -> OutboxJob | None:
        timestamp = now_ms()
        with self._lock, self._connection:
            row = self._connection.execute(
                "SELECT id, event_id, target, attempts, progress_json FROM outbox_jobs "
                "WHERE state IN ('queued', 'retry') AND next_attempt_at_ms <= ? "
                "ORDER BY id LIMIT 1",
                (timestamp,),
            ).fetchone()
            if not row:
                return None
            changed = self._connection.execute(
                "UPDATE outbox_jobs SET state='delivering', updated_at_ms=? "
                "WHERE id=? AND state IN ('queued', 'retry')",
                (timestamp, row["id"]),
            )
            if changed.rowcount != 1:
                return None
        return OutboxJob(
            id=row["id"],
            event_id=row["event_id"],
            target=Platform(row["target"]),
            attempts=row["attempts"],
            progress=json.loads(row["progress_json"]),
        )

    def recover_delivering_jobs(self) -> None:
        with self._lock, self._connection:
            self._connection.execute(
                "UPDATE outbox_jobs SET state='retry', next_attempt_at_ms=?, updated_at_ms=? "
                "WHERE state='delivering'",
                (now_ms(), now_ms()),
            )

    def update_progress(self, job_id: int, progress: dict[str, object]) -> None:
        with self._lock, self._connection:
            self._connection.execute(
                "UPDATE outbox_jobs SET progress_json=?, updated_at_ms=? WHERE id=?",
                (json.dumps(progress, separators=(",", ":")), now_ms(), job_id),
            )

    def complete_job(self, job_id: int, event_id: str) -> None:
        timestamp = now_ms()
        with self._lock, self._connection:
            self._connection.execute(
                "UPDATE outbox_jobs SET state='completed', last_error=NULL, updated_at_ms=? WHERE id=?",
                (timestamp, job_id),
            )
            self._connection.execute(
                "UPDATE inbox_events SET status='completed', payload_json=NULL, completed_at_ms=? "
                "WHERE event_id=?",
                (timestamp, event_id),
            )

    def retry_job(self, job: OutboxJob, error: str, delay_seconds: float) -> None:
        timestamp = now_ms()
        next_at = timestamp + max(1000, int(delay_seconds * 1000))
        with self._lock, self._connection:
            self._connection.execute(
                "UPDATE outbox_jobs SET state='retry', attempts=attempts+1, "
                "next_attempt_at_ms=?, last_error=?, updated_at_ms=? WHERE id=?",
                (next_at, error[:500], timestamp, job.id),
            )

    def dead_letter_job(self, job: OutboxJob, error: str) -> None:
        timestamp = now_ms()
        with self._lock, self._connection:
            self._connection.execute(
                "UPDATE outbox_jobs SET state='dead', attempts=attempts+1, last_error=?, updated_at_ms=? "
                "WHERE id=?",
                (error[:500], timestamp, job.id),
            )
            self._connection.execute(
                "UPDATE inbox_events SET status='dead' WHERE event_id=?", (job.event_id,)
            )

    def add_link(self, link: MessageLink) -> None:
        with self._lock, self._connection:
            self._connection.execute(
                "INSERT OR IGNORE INTO message_links"
                "(mm_post_id, tg_chat_id, tg_message_id, mm_root_id, tg_anchor_message_id, "
                "part_index, created_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    link.mm_post_id,
                    link.tg_chat_id,
                    link.tg_message_id,
                    link.mm_root_id,
                    link.tg_anchor_message_id,
                    link.part_index,
                    now_ms(),
                ),
            )

    def find_by_mm(self, post_id: str) -> MessageLink | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT * FROM message_links WHERE mm_post_id=? ORDER BY part_index, id LIMIT 1",
                (post_id,),
            ).fetchone()
        return self._link(row)

    def find_by_mm_root(self, root_id: str) -> MessageLink | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT * FROM message_links WHERE mm_root_id=? ORDER BY id LIMIT 1", (root_id,)
            ).fetchone()
        return self._link(row)

    def find_by_tg(self, chat_id: int, message_id: str) -> MessageLink | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT * FROM message_links WHERE tg_chat_id=? AND tg_message_id=? ORDER BY id LIMIT 1",
                (chat_id, message_id),
            ).fetchone()
        return self._link(row)

    @staticmethod
    def _link(row: sqlite3.Row | None) -> MessageLink | None:
        if not row:
            return None
        return MessageLink(
            mm_post_id=row["mm_post_id"],
            tg_chat_id=row["tg_chat_id"],
            tg_message_id=row["tg_message_id"],
            mm_root_id=row["mm_root_id"],
            tg_anchor_message_id=row["tg_anchor_message_id"],
            part_index=row["part_index"],
        )

    def oldest_pending_age_seconds(self) -> float | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT MIN(created_at_ms) AS oldest FROM outbox_jobs "
                "WHERE state IN ('queued', 'retry', 'delivering')"
            ).fetchone()
        if not row or row["oldest"] is None:
            return None
        return max(0.0, (now_ms() - row["oldest"]) / 1000)

    def counts(self) -> dict[str, int]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT state, COUNT(*) AS count FROM outbox_jobs GROUP BY state"
            ).fetchall()
        return {row["state"]: row["count"] for row in rows}
