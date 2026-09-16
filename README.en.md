# Telegram ↔ Mattermost Bridge in Go

[Русский](README.md) | **English**

A standalone Go implementation of a bidirectional bridge between one or more
Telegram group ↔ private Mattermost channel pairs. It is compatible with the
SQLite schema used by the earlier Python version and can continue from its
offsets, checkpoints, thread links, and reaction state.

```text
Telegram long polling ─┐
                      ├→ Gateway.Accept(event) → SQLite inbox/outbox → delivery worker
Mattermost WebSocket ──┘                         ↕ message/thread/reaction links
```

`Gateway` is a deep module with a single `Accept` entry point. Adapters only
normalize external events and perform delivery; deduplication, retries, the
durable outbox, threads, edits, and reactions stay inside the Gateway.

## Features

- a standalone message starts a new conversation, while a reply preserves the thread;
- author prefixes such as `Anton: ` are italicized on both platforms, while the
  message continues in regular text on the same line;
- text, photos, documents, video, audio, voice messages, animations, and Telegram albums;
- multiple Mattermost attachments are sent sequentially, with at most five files per post;
- safe escaping of Mattermost Markdown and all external `@mentions`;
- silent editing of an already linked text message;
- deleting a Mattermost post deletes every linked message created by the bridge
  in Telegram, without a separate notification (subject to Telegram's 48-hour limit);
- bidirectional reactions for `👍`, `👎`, `❤️`, `🔥`, `🎉`, `✅`, `👀`, and `😂`;
- Telegram author name and avatar through Mattermost post overrides when the server allows it;
- multiple independent channel pairs through `BRIDGE_PAIRS`;
- SQLite WAL mode, durable inbox/outbox, deduplication, and dead letters;
- retries for network errors, `429`, and `5xx`, honoring `Retry-After` and using
  exponential backoff capped at five minutes;
- Mattermost REST catch-up with a five-minute overlap after a WebSocket disconnect;
- JSON logs that exclude tokens and message bodies;
- `/healthz` and `/readyz`; readiness degrades when an adapter is unavailable or
  the oldest queued job is more than ten minutes old.

Telegram lets a bot place only one reaction on a message, so Telegram shows the
most recent active reaction. Mattermost keeps every supported reaction at the
same time; technically, those reactions belong to the bridge bot.

## Configuration

```bash
cp .env.example .env
chmod 600 .env
```

Required variables:

| Variable | Purpose |
|---|---|
| `MM_URL` | Mattermost HTTPS URL |
| `MM_BOT_TOKEN` | Token for a dedicated Mattermost bot account |
| `MM_CHANNEL_ID` | Mattermost channel for a single pair |
| `TG_BOT_TOKEN` | Telegram bot token |
| `TG_CHAT_ID` | Telegram group or supergroup ID for a single pair |
| `DB_PATH` | SQLite path; `/data/bridge.db` in Compose |

For multiple pairs, `BRIDGE_PAIRS` replaces `MM_CHANNEL_ID` and `TG_CHAT_ID`:

```env
BRIDGE_PAIRS=[{"id":"default","tg_chat_id":-1001111111111,"mm_channel_id":"channel_a"},{"id":"customer-b","tg_chat_id":-1002222222222,"mm_channel_id":"channel_b"}]
```

Keep the `default` ID for an existing pair so it continues using its checkpoint.
By default, the Mattermost author's name is forwarded to Telegram. To anonymize
the Mattermost → Telegram direction for one pair, set
`"mm_to_tg_author_mode":"hidden"`; the reverse direction is unchanged:

```env
BRIDGE_PAIRS=[{"id":"default","tg_chat_id":-1001111111111,"mm_channel_id":"channel_a"},{"id":"customer-b","tg_chat_id":-1002222222222,"mm_channel_id":"channel_b","mm_to_tg_author_mode":"hidden"}]
```

Disable Privacy Mode through `@BotFather` and add the bot to the group again, or
make it an administrator. The bot must be an administrator to receive reaction
events. The Mattermost bot account should belong only to the required team and channels.

## Running the bridge

Check tokens, IDs, channel membership, and SQLite write access:

```bash
docker compose build
docker compose run --rm bridge doctor
```

The first initialization of a new database intentionally drops pending Telegram
updates and sets the Mattermost checkpoint to the current time:

```bash
docker compose run --rm bridge init
docker compose up -d
```

Running `init` again is rejected. Use `init --force` only for an intentional
checkpoint reset.

```bash
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
docker compose logs -f bridge
```

After startup, you can send one clearly marked test message in each direction
through the durable pipeline:

```bash
docker compose run --rm bridge smoke --confirm
```

Without Docker:

```bash
go run ./cmd/bridge doctor
go run ./cmd/bridge init
go run ./cmd/bridge run
```

## Migrating from the Python version

The Go and Python implementations use the same migrations 1–2. Never run both
processes at the same time with the same Telegram token or SQLite database.

1. Confirm that `/readyz` on the Python version reports an empty active queue.
2. Stop the Python container and back up `bridge.db` together with its WAL/SHM files.
3. Copy `.env` without printing its secrets.
4. To keep using the previous Docker volume, start with the production override:

   ```bash
   docker compose -f compose.yaml -f compose.production.yaml build
   docker compose -f compose.yaml -f compose.production.yaml run --rm bridge doctor
   docker compose -f compose.yaml -f compose.production.yaml up -d
   ```

Do not run `init` during migration. To roll back, stop the Go Compose project and
start the previous Python Compose project again; the database schema and message
links remain compatible.

## Development and verification

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/bridge
```

Tests use temporary SQLite databases and local fake HTTP/WebSocket endpoints; no
real credentials are required.

## Delivery guarantees

Each event and checkpoint is committed atomically before the Telegram offset or
Mattermost checkpoint advances. After successful delivery, the payload is removed
from SQLite while IDs, status, and message links remain. Delivery is at least once:
a crash in the narrow interval between an external send and recording its result
can theoretically create one duplicate.

Editing file attachments, custom or paid emoji, interactive polls, synchronous
deletion of messages created by other Telegram users, and rebuilding Mattermost
reaction state from history are outside the current version.

## License

This project is distributed under the [MIT License](LICENSE).
