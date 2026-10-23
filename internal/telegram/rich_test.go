package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/textutil"
)

func TestRichMessageFromJSON(t *testing.T) {
	const raw = `{"update_id":51,"message":{"message_id":15,"chat":{"id":-100},"from":{"id":2,"first_name":"A"},"rich_message":{"blocks":[
		{"type":"paragraph","text":["Добрый день. ",{"type":"bold","text":"Пример"}]},
		{"type":"heading","size":2,"text":"Раздел"},
		{"type":"list","items":[{"label":"•","blocks":[{"type":"paragraph","text":[{"type":"code","text":"sample_value"},": число"]},{"type":"list","items":[{"label":"1.","blocks":[{"type":"paragraph","text":"Вложенный пункт"}]}]}]}]},
		{"type":"pre","language":"json","text":"{\"value\":null}"},
		{"type":"paragraph","text":{"type":"url","text":"Документация","url":"https://example.com/api"}},
		{"type":"paragraph","text":"@channel @all @here @someone"}
	]}}}`
	var update Update
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatal(err)
	}
	event := testAdapter().NormalizeUpdates([]Update{update}, 99)[0]
	if event.Kind != model.Message {
		t.Fatalf("rich message dropped: kind=%s", event.Kind)
	}
	for _, part := range []string{"Добрый день. Пример", "Раздел", "• sample_value: число", "  1. Вложенный пункт", "{\"value\":null}", "Документация (https://example.com/api)"} {
		if !strings.Contains(event.Text, part) {
			t.Errorf("missing %q in %q", part, event.Text)
		}
	}
	rendered := textutil.Render(event, model.Mattermost, "", true)
	for _, mention := range []string{"@channel", "@all", "@here", "@someone"} {
		if strings.Contains(rendered, mention) {
			t.Fatalf("unsafe mention %s", mention)
		}
	}
	update.Message.ReplyTo = update.Message
	update.EditedMessage, update.Message = update.Message, nil
	event = testAdapter().NormalizeUpdates([]Update{update}, 99)[0]
	if event.Kind != model.Edit || event.ReplyToID != "15" || !strings.Contains(event.ReplyQuote, "Пример") {
		t.Fatalf("rich edit/reply lost: %#v", event)
	}
}

func TestUnsupportedContentDoesNotDisappear(t *testing.T) {
	for _, tc := range []struct{ body, contentType, reason string }{
		{`"rich_message":{"blocks":[{"type":"paragraph","text":"visible"},{"type":"future_secret_type","secret":"never log this"}]}`, "rich_message", "unsupported_rich_message"},
		{`"rich_message":{"blocks":[{"type":"photo","photo":[]}]}`, "rich_message", "unsupported_rich_message"},
		{`"rich_message":{}`, "rich_message", "unsupported_rich_message"},
		{`"rich_message":42`, "rich_message", "invalid_rich_message"},
		{`"location":{"latitude":1,"longitude":2}`, "location", "unsupported_content"},
		{`"new_secret_type":{"value":"never log this"}`, "unknown", "unsupported_content"},
	} {
		var u Update
		if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":2,"chat":{"id":-100},"from":{"id":2},`+tc.body+`}}`), &u); err != nil {
			t.Fatal(err)
		}
		e := testAdapter().NormalizeUpdates([]Update{u}, 99)[0]
		if e.Kind != model.Unsupported || e.ContentType != tc.contentType || e.DiagnosticReason != tc.reason || e.Text != "" || *e.CheckpointValue != "2" {
			t.Fatalf("bad diagnostic: %#v", e)
		}
		u.Message.Chat.ID = -200
		if e := testAdapter().NormalizeUpdates([]Update{u}, 99)[0]; e.Kind != model.Ignored {
			t.Fatal("foreign chat processed")
		}
		u.Message.Chat.ID, u.Message.From.ID = -100, 99
		if e := testAdapter().NormalizeUpdates([]Update{u}, 99)[0]; e.Kind != model.Ignored {
			t.Fatal("self message processed")
		}
	}
	var service Update
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":2,"chat":{"id":-100},"pinned_message":{"message_id":1,"text":"hello"}}}`), &service); err != nil {
		t.Fatal(err)
	}
	if e := testAdapter().NormalizeUpdates([]Update{service}, 99)[0]; e.Kind != model.Ignored {
		t.Fatal("service message treated as unsupported content")
	}
}

func TestRichMessageHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,"chat":{"id":-100},"from":{"id":2},"rich_message":{"blocks":[{"type":"paragraph","text":"hello"}]}}}]}`)
	}))
	defer server.Close()
	a := New(server.Client(), "test-token", server.URL, []config.Route{{ID: "default", TGChatID: -100, MMChannelID: "c"}}, nil, nil)
	updates, err := a.GetUpdates(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	e := a.NormalizeUpdates(updates, 99)[0]
	if e.Kind != model.Message || e.Text != "hello" {
		t.Fatalf("HTTP rich message lost: %#v", e)
	}
}

func TestRichMessageDepthLimit(t *testing.T) {
	var text any = "end"
	for i := 0; i < 100; i++ {
		text = map[string]any{"type": "bold", "text": text}
	}
	raw, _ := json.Marshal(map[string]any{"blocks": []any{map[string]any{"type": "paragraph", "text": text}}})
	if _, err := richMessageText(raw); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
}
