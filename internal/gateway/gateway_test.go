package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/platform"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/storage"
)

type fakeAdapter struct {
	mu        sync.Mutex
	platform  model.Platform
	sends     []model.DeliveryContext
	edits     []string
	deletes   []string
	syncs     []string
	failures  int
	sendErr   error
	deleteErr error
	next      int
}

func (f *fakeAdapter) Platform() model.Platform { return f.platform }
func (f *fakeAdapter) FetchAttachment(context.Context, model.Attachment, int64) (model.MaterializedAttachment, error) {
	return model.MaterializedAttachment{}, nil
}
func (f *fakeAdapter) FetchAuthorAvatar(context.Context, model.Event, int64) (*model.MaterializedAttachment, error) {
	return nil, nil
}
func (f *fakeAdapter) Send(_ context.Context, _ model.Event, c model.DeliveryContext, _ []model.MaterializedAttachment, _ []string, _ int, onSent platform.SentCallback, _ *model.MaterializedAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.next++
	id := fmt.Sprintf("%s-%d", f.platform, f.next)
	f.sends = append(f.sends, c)
	return onSent(model.SentMessage{MessageID: id, RootID: id})
}
func (f *fakeAdapter) SendFailureNotice(context.Context, model.Event, string) error {
	f.mu.Lock()
	f.failures++
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) Edit(_ context.Context, _ model.Event, target string) error {
	f.mu.Lock()
	f.edits = append(f.edits, target)
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) Delete(_ context.Context, _ model.Event, targets []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, targets...)
	return nil
}
func (f *fakeAdapter) AcknowledgeDelivery(context.Context, model.Event) error { return nil }
func (f *fakeAdapter) SyncReactions(_ context.Context, _ model.Event, target string, _ []string) error {
	f.mu.Lock()
	f.syncs = append(f.syncs, target)
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) counts() (int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends), len(f.edits), len(f.syncs), f.failures
}
func (f *fakeAdapter) contextAt(i int) model.DeliveryContext {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends[i]
}
func (f *fakeAdapter) deleteTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func setupGateway(t *testing.T) (*Gateway, *storage.Store, *fakeAdapter, *fakeAdapter, context.CancelFunc) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tg := &fakeAdapter{platform: model.Telegram}
	mm := &fakeAdapter{platform: model.Mattermost}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := New(store, map[model.Platform]platform.Adapter{model.Telegram: tg, model.Mattermost: mm}, []config.Route{{ID: "default", TGChatID: -100, MMChannelID: "c"}}, 1024, logger)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = gw.Run(runCtx) }()
	t.Cleanup(cancel)
	return gw, store, tg, mm, cancel
}

func TestGatewayCorrelatesThreadsEditsReactionsAndDeduplicates(t *testing.T) {
	gw, store, _, mm, _ := setupGateway(t)
	ctx := context.Background()
	root := model.Event{EventID: "tg:1:message", Platform: model.Telegram, Kind: model.Message, MessageID: "10", MessageIDs: []string{"10"}, AuthorID: "u", AuthorName: "Антон", RouteID: "default", Text: "root"}
	inserted, err := gw.Accept(ctx, root)
	if err != nil || !inserted {
		t.Fatalf("accept %v %v", inserted, err)
	}
	waitFor(t, func() bool { s, _, _, _ := mm.counts(); return s == 1 })
	link, err := store.FindByTG(ctx, -100, "10")
	if err != nil || link == nil || link.MMPostID != "mattermost-1" {
		t.Fatalf("link %#v %v", link, err)
	}
	reply := root
	reply.EventID = "tg:2:message"
	reply.MessageID = "11"
	reply.MessageIDs = []string{"11"}
	reply.ReplyToID = "10"
	reply.Text = "reply"
	if inserted, err := gw.Accept(ctx, reply); err != nil || !inserted {
		t.Fatal(err)
	}
	waitFor(t, func() bool { s, _, _, _ := mm.counts(); return s == 2 })
	replyContext := mm.contextAt(1)
	if replyContext.RootID != "mattermost-1" || replyContext.ReplyToID != "mattermost-1" {
		t.Fatalf("reply context %#v", replyContext)
	}
	if inserted, err := gw.Accept(ctx, reply); err != nil || inserted {
		t.Fatalf("duplicate %v %v", inserted, err)
	}
	edit := root
	edit.EventID = "tg:3:edit"
	edit.Kind = model.Edit
	edit.Text = "changed"
	if _, err := gw.Accept(ctx, edit); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, e, _, _ := mm.counts(); return e == 1 })
	if mm.edits[0] != "mattermost-1" {
		t.Fatalf("edit target %q", mm.edits[0])
	}
	reaction := root
	reaction.EventID = "tg:4:reaction_added"
	reaction.Kind = model.ReactionAdded
	reaction.Reaction = "🔥"
	if _, err := gw.Accept(ctx, reaction); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, s, _ := mm.counts(); return s == 1 })
	if mm.syncs[0] != "mattermost-1" {
		t.Fatalf("reaction target %q", mm.syncs[0])
	}
}

func TestGatewayDeadLettersPermanentFailureAndNotifiesSource(t *testing.T) {
	gw, store, tg, mm, _ := setupGateway(t)
	mm.sendErr = model.Permanent("forbidden")
	ctx := context.Background()
	event := model.Event{EventID: "tg:9:message", Platform: model.Telegram, Kind: model.Message, MessageID: "90", MessageIDs: []string{"90"}, AuthorID: "u", AuthorName: "A", RouteID: "default", Text: "x"}
	if _, err := gw.Accept(ctx, event); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, _, f := tg.counts(); return f == 1 })
	counts, err := store.Counts(ctx)
	if err != nil || counts["dead"] != 1 {
		t.Fatalf("counts %#v %v", counts, err)
	}
}

func TestGatewayDoesNotSendDeletionNotification(t *testing.T) {
	gw, store, tg, _, _ := setupGateway(t)
	ctx := context.Background()
	if err := store.AddLink(ctx, model.MessageLink{MMPostID: "mm1", TGChatID: -100, TGMessageID: "10", MMRootID: "mm1", TGAnchorMessageID: "10"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddLink(ctx, model.MessageLink{MMPostID: "mm1", TGChatID: -100, TGMessageID: "11", MMRootID: "mm1", TGAnchorMessageID: "10", PartIndex: 1}); err != nil {
		t.Fatal(err)
	}
	event := model.Event{EventID: "mm:1:delete", Platform: model.Mattermost, Kind: model.Delete, MessageID: "mm1", MessageIDs: []string{"mm1"}, AuthorID: "u", AuthorName: "A", RouteID: "default"}
	if _, err := gw.Accept(ctx, event); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		counts, err := store.Counts(ctx)
		return err == nil && counts["completed"] == 1
	})
	sends, _, _, _ := tg.counts()
	if sends != 0 {
		t.Fatalf("delete produced %d Telegram notification messages", sends)
	}
	deleted := tg.deleteTargets()
	if len(deleted) != 2 || deleted[0] != "10" || deleted[1] != "11" {
		t.Fatalf("deleted targets %#v", deleted)
	}
}

func TestGatewayDoesNotNotifyWhenLinkedDeleteIsRejected(t *testing.T) {
	gw, store, tg, mm, _ := setupGateway(t)
	tg.deleteErr = model.Permanent("cannot delete")
	ctx := context.Background()
	if err := store.AddLink(ctx, model.MessageLink{MMPostID: "mm1", TGChatID: -100, TGMessageID: "10", MMRootID: "mm1", TGAnchorMessageID: "10"}); err != nil {
		t.Fatal(err)
	}
	event := model.Event{EventID: "mm:2:delete", Platform: model.Mattermost, Kind: model.Delete, MessageID: "mm1", MessageIDs: []string{"mm1"}, RouteID: "default"}
	if _, err := gw.Accept(ctx, event); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		counts, err := store.Counts(ctx)
		return err == nil && counts["dead"] == 1
	})
	_, _, _, failures := mm.counts()
	if failures != 0 {
		t.Fatalf("delete failure produced %d user notifications", failures)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}
