package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=10000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func nowMS() int64            { return time.Now().UnixMilli() }

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at_ms INTEGER NOT NULL)"); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			continue
		}
		var exists int
		err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version=?", version).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at_ms) VALUES (?, ?)", version, nowMS())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Initialize(ctx context.Context, mmCheckpointMS, tgOffset int64, routeIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO bridge_meta(key,value) VALUES('initialized','true') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); err != nil {
		return err
	}
	states := map[string]string{"mattermost_checkpoint_ms": strconv.FormatInt(mmCheckpointMS, 10), "telegram_offset": strconv.FormatInt(tgOffset, 10)}
	for _, routeID := range routeIDs {
		states["mattermost_checkpoint_ms:"+routeID] = strconv.FormatInt(mmCheckpointMS, 10)
	}
	for key, value := range states {
		if _, err = tx.ExecContext(ctx, "INSERT INTO sync_state(key,value,updated_at_ms) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at_ms=excluded.updated_at_ms", key, value, nowMS()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM bridge_meta WHERE key='initialized'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value == "true", err
}

func (s *Store) GetState(ctx context.Context, key, fallback string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM sync_state WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	return value, err
}

func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO sync_state(key,value,updated_at_ms) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at_ms=excluded.updated_at_ms", key, value, nowMS())
	return err
}

func (s *Store) Enqueue(ctx context.Context, event model.Event) (bool, error) {
	payload, err := event.JSON()
	if err != nil {
		return false, err
	}
	timestamp := nowMS()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO inbox_events(event_id,source,kind,source_message_id,payload_json,status,received_at_ms) VALUES(?,?,?,?,?,'accepted',?)", event.EventID, event.Platform, event.Kind, event.MessageID, payload, timestamp)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	inserted := rows == 1
	if inserted && event.Kind == model.ReactionAdded && event.Reaction != "" {
		created := event.CreatedAtMS
		if created == 0 {
			created = timestamp
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO reaction_state(source,route_id,source_message_id,actor_id,emoji,created_at_ms) VALUES(?,?,?,?,?,?) ON CONFLICT(source,route_id,source_message_id,actor_id,emoji) DO UPDATE SET created_at_ms=excluded.created_at_ms", event.Platform, event.RouteID, event.MessageID, event.AuthorID, event.Reaction, created)
	} else if inserted && event.Kind == model.ReactionRemoved && event.Reaction != "" {
		_, err = tx.ExecContext(ctx, "DELETE FROM reaction_state WHERE source=? AND route_id=? AND source_message_id=? AND actor_id=? AND emoji=?", event.Platform, event.RouteID, event.MessageID, event.AuthorID, event.Reaction)
	}
	if err != nil {
		return false, err
	}
	if inserted && event.Kind != model.Ignored {
		_, err = tx.ExecContext(ctx, "INSERT INTO outbox_jobs(event_id,target,state,next_attempt_at_ms,created_at_ms,updated_at_ms) VALUES(?,?,'queued',?,?,?)", event.EventID, event.Platform.Opposite(), timestamp, timestamp, timestamp)
	} else if inserted {
		_, err = tx.ExecContext(ctx, "UPDATE inbox_events SET status='completed',payload_json=NULL,completed_at_ms=? WHERE event_id=?", timestamp, event.EventID)
	}
	if err != nil {
		return false, err
	}
	if event.CheckpointKey != "" && event.CheckpointValue != nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO sync_state(key,value,updated_at_ms) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=CASE WHEN CAST(excluded.value AS INTEGER)>CAST(sync_state.value AS INTEGER) THEN excluded.value ELSE sync_state.value END,updated_at_ms=excluded.updated_at_ms", event.CheckpointKey, *event.CheckpointValue, timestamp)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return inserted, nil
}

func (s *Store) GetEvent(ctx context.Context, eventID string) (model.Event, error) {
	var payload []byte
	if err := s.db.QueryRowContext(ctx, "SELECT payload_json FROM inbox_events WHERE event_id=?", eventID).Scan(&payload); err != nil {
		return model.Event{}, err
	}
	var event model.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return model.Event{}, err
	}
	return event, nil
}

func (s *Store) FetchDueJob(ctx context.Context) (*model.OutboxJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var job model.OutboxJob
	var target string
	var progress []byte
	err = tx.QueryRowContext(ctx, "SELECT id,event_id,target,attempts,progress_json FROM outbox_jobs WHERE state IN ('queued','retry') AND next_attempt_at_ms<=? ORDER BY id LIMIT 1", nowMS()).Scan(&job.ID, &job.EventID, &target, &job.Attempts, &progress)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE outbox_jobs SET state='delivering',updated_at_ms=? WHERE id=? AND state IN ('queued','retry')", nowMS(), job.ID)
	if err != nil {
		return nil, err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return nil, nil
	}
	job.Target = model.Platform(target)
	if err := json.Unmarshal(progress, &job.Progress); err != nil {
		return nil, err
	}
	if job.Progress == nil {
		job.Progress = map[string]any{}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) RecoverDelivering(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE outbox_jobs SET state='retry',next_attempt_at_ms=?,updated_at_ms=? WHERE state='delivering'", nowMS(), nowMS())
	return err
}

func (s *Store) UpdateProgress(ctx context.Context, jobID int64, progress map[string]any) error {
	payload, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE outbox_jobs SET progress_json=?,updated_at_ms=? WHERE id=?", payload, nowMS(), jobID)
	return err
}

func (s *Store) CompleteJob(ctx context.Context, jobID int64, eventID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	timestamp := nowMS()
	if _, err = tx.ExecContext(ctx, "UPDATE outbox_jobs SET state='completed',last_error=NULL,updated_at_ms=? WHERE id=?", timestamp, jobID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE inbox_events SET status='completed',payload_json=NULL,completed_at_ms=? WHERE event_id=?", timestamp, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RetryJob(ctx context.Context, job model.OutboxJob, message string, delay time.Duration) error {
	if delay < time.Second {
		delay = time.Second
	}
	_, err := s.db.ExecContext(ctx, "UPDATE outbox_jobs SET state='retry',attempts=attempts+1,next_attempt_at_ms=?,last_error=?,updated_at_ms=? WHERE id=?", nowMS()+delay.Milliseconds(), truncate(message, 500), nowMS(), job.ID)
	return err
}

func (s *Store) DeadLetterJob(ctx context.Context, job model.OutboxJob, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE outbox_jobs SET state='dead',attempts=attempts+1,last_error=?,updated_at_ms=? WHERE id=?", truncate(message, 500), nowMS(), job.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE inbox_events SET status='dead' WHERE event_id=?", job.EventID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddLink(ctx context.Context, link model.MessageLink) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO message_links(mm_post_id,tg_chat_id,tg_message_id,mm_root_id,tg_anchor_message_id,part_index,created_at_ms) VALUES(?,?,?,?,?,?,?)", link.MMPostID, link.TGChatID, link.TGMessageID, link.MMRootID, link.TGAnchorMessageID, link.PartIndex, nowMS())
	return err
}

func (s *Store) FindByMM(ctx context.Context, postID string) (*model.MessageLink, error) {
	return s.findLink(ctx, "SELECT mm_post_id,tg_chat_id,tg_message_id,mm_root_id,tg_anchor_message_id,part_index FROM message_links WHERE mm_post_id=? ORDER BY part_index,id LIMIT 1", postID)
}

func (s *Store) FindByMMRoot(ctx context.Context, rootID string) (*model.MessageLink, error) {
	return s.findLink(ctx, "SELECT mm_post_id,tg_chat_id,tg_message_id,mm_root_id,tg_anchor_message_id,part_index FROM message_links WHERE mm_root_id=? ORDER BY id LIMIT 1", rootID)
}

func (s *Store) FindByTG(ctx context.Context, chatID int64, messageID string) (*model.MessageLink, error) {
	return s.findLink(ctx, "SELECT mm_post_id,tg_chat_id,tg_message_id,mm_root_id,tg_anchor_message_id,part_index FROM message_links WHERE tg_chat_id=? AND tg_message_id=? ORDER BY id LIMIT 1", chatID, messageID)
}

func (s *Store) findLink(ctx context.Context, query string, args ...any) (*model.MessageLink, error) {
	var link model.MessageLink
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&link.MMPostID, &link.TGChatID, &link.TGMessageID, &link.MMRootID, &link.TGAnchorMessageID, &link.PartIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &link, err
}

func (s *Store) ActiveReactions(ctx context.Context, source model.Platform, routeID, messageID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT emoji,MAX(created_at_ms) AS latest FROM reaction_state WHERE source=? AND route_id=? AND source_message_id=? GROUP BY emoji ORDER BY latest,emoji", source, routeID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var emoji string
		var latest int64
		if err := rows.Scan(&emoji, &latest); err != nil {
			return nil, err
		}
		result = append(result, emoji)
	}
	return result, rows.Err()
}

func (s *Store) OldestPendingAge(ctx context.Context) (*float64, error) {
	var created sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MIN(created_at_ms) FROM outbox_jobs WHERE state IN ('queued','retry','delivering')").Scan(&created)
	if err != nil {
		return nil, err
	}
	if !created.Valid {
		return nil, nil
	}
	value := float64(nowMS()-created.Int64) / 1000
	return &value, nil
}

func (s *Store) Counts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT state,COUNT(*) FROM outbox_jobs GROUP BY state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int64{}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		result[state] = count
	}
	return result, rows.Err()
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
