package telegram

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

func testAdapter() *Adapter {
	return New(nil, "token", "https://api.telegram.test", []config.Route{{ID: "default", TGChatID: -100, MMChannelID: "c"}}, nil, nil)
}

func TestNormalizeMessageReplyAndAlbum(t *testing.T) {
	a := testAdapter()
	reply := &Message{MessageID: 5, Text: "old"}
	updates := []Update{{UpdateID: 10, Message: &Message{MessageID: 6, From: &User{ID: 2, FirstName: "Антон", Username: "anton"}, Chat: Chat{ID: -100}, Date: 100, Caption: "Фото", MediaGroupID: "album", ReplyTo: reply, Photo: []File{{FileID: "small", FileSize: intPtr(1)}, {FileID: "large", FileSize: intPtr(2)}}}}, {UpdateID: 11, Message: &Message{MessageID: 7, From: &User{ID: 2, FirstName: "Антон"}, Chat: Chat{ID: -100}, Date: 100, MediaGroupID: "album", Document: &File{FileID: "doc", FileName: "a.pdf", MimeType: "application/pdf"}}}}
	events := a.NormalizeUpdates(updates, 99)
	if len(events) != 1 {
		t.Fatalf("events %#v", events)
	}
	event := events[0]
	if event.Kind != model.Message || event.RouteID != "default" || event.ReplyToID != "5" || event.ReplyQuote != "old" || len(event.MessageIDs) != 2 || len(event.Attachments) != 2 {
		t.Fatalf("event %#v", event)
	}
	if event.Attachments[0].FileID != "large" {
		t.Fatalf("largest photo not selected: %#v", event.Attachments[0])
	}
	if event.CheckpointValue == nil || *event.CheckpointValue != "12" {
		t.Fatalf("checkpoint %#v", event.CheckpointValue)
	}
}

func TestNormalizeIgnoredChatStillAdvancesOffset(t *testing.T) {
	a := testAdapter()
	events := a.NormalizeUpdates([]Update{{UpdateID: 20, Message: &Message{MessageID: 1, From: &User{ID: 2}, Chat: Chat{ID: -200}, Text: "x"}}}, 99)
	if len(events) != 1 || events[0].Kind != model.Ignored || events[0].CheckpointValue == nil || *events[0].CheckpointValue != "21" {
		t.Fatalf("events %#v", events)
	}
}

func TestNormalizeBasicReaction(t *testing.T) {
	a := testAdapter()
	events := a.NormalizeUpdates([]Update{{UpdateID: 30, MessageReaction: &ReactionUpdate{Chat: Chat{ID: -100}, MessageID: 8, User: &User{ID: 2, FirstName: "A"}, Date: 10, New: []ReactionType{{Type: "emoji", Emoji: "🔥"}}}}}, 99)
	if len(events) != 1 || events[0].Kind != model.ReactionAdded || events[0].Reaction != "🔥" {
		t.Fatalf("events %#v", events)
	}
}
func intPtr(v int64) *int64 { return &v }

func TestItalicAuthorEntityUsesTelegramUTF16Offsets(t *testing.T) {
	values := url.Values{}
	addItalicAuthorEntity(values, "(1/2) Иван 🚀\nСообщение", "Иван 🚀")
	var entities []messageEntity
	if err := json.Unmarshal([]byte(values.Get("entities")), &entities); err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Offset != 6 || entities[0].Length != 7 {
		t.Fatalf("entities %#v", entities)
	}
}
