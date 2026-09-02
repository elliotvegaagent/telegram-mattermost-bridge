package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestEnqueueIsDurableAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	checkpoint := "42"
	event := model.Event{EventID: "tg:41:message", Platform: model.Telegram, Kind: model.Message, MessageID: "8", MessageIDs: []string{"8"}, AuthorID: "1", AuthorName: "A", RouteID: "default", Text: "secret body", CheckpointKey: "telegram_offset", CheckpointValue: &checkpoint}
	inserted, err := store.Enqueue(ctx, event)
	if err != nil || !inserted {
		t.Fatalf("first enqueue: %v %v", inserted, err)
	}
	inserted, err = store.Enqueue(ctx, event)
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue: %v %v", inserted, err)
	}
	job, err := store.FetchDueJob(ctx)
	if err != nil || job == nil || job.Target != model.Mattermost {
		t.Fatalf("job: %#v %v", job, err)
	}
	stored, err := store.GetEvent(ctx, event.EventID)
	if err != nil || stored.Text != "secret body" {
		t.Fatalf("event: %#v %v", stored, err)
	}
	if err := store.CompleteJob(ctx, job.ID, event.EventID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetEvent(ctx, event.EventID); err == nil {
		t.Fatal("completed event body must be removed")
	}
	offset, err := store.GetState(ctx, "telegram_offset", "")
	if err != nil || offset != "42" {
		t.Fatalf("offset %q %v", offset, err)
	}
}

func TestMessageLinksAndReactionAggregation(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	link := model.MessageLink{MMPostID: "mm1", TGChatID: -100, TGMessageID: "10", MMRootID: "mm1", TGAnchorMessageID: "10"}
	if err := store.AddLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	if got, err := store.FindByTG(ctx, -100, "10"); err != nil || got == nil || got.MMPostID != "mm1" {
		t.Fatalf("link %#v %v", got, err)
	}
	for i, emoji := range []string{"👍", "🔥"} {
		event := model.Event{EventID: "r" + string(rune('0'+i)), Platform: model.Telegram, Kind: model.ReactionAdded, MessageID: "10", MessageIDs: []string{"10"}, AuthorID: "u" + string(rune('0'+i)), AuthorName: "A", RouteID: "default", Reaction: emoji, CreatedAtMS: int64(i + 1)}
		if _, err := store.Enqueue(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ActiveReactions(ctx, model.Telegram, "default", "10")
	if err != nil || len(got) != 2 || got[1] != "🔥" {
		t.Fatalf("reactions %#v %v", got, err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	store := newStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
