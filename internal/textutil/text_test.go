package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

func TestRenderUsesCompactAuthorFormat(t *testing.T) {
	mm := model.Event{Platform: model.Mattermost, Kind: model.Message, AuthorName: "Дмитрий Карнаухов", Text: "все понятно"}
	if got := Render(mm, model.Telegram, "", true); got != "Дмитрий Карнаухов\nвсе понятно" {
		t.Fatalf("unexpected Telegram text: %q", got)
	}
	tg := model.Event{Platform: model.Telegram, Kind: model.Message, AuthorName: "Антон", Text: "Спасибо"}
	if got := Render(tg, model.Mattermost, "", true); got != "Антон: Спасибо" {
		t.Fatalf("unexpected Mattermost text: %q", got)
	}
}

func TestRenderNeutralizesMattermostMentionsAndMarkdown(t *testing.T) {
	event := model.Event{Platform: model.Telegram, Kind: model.Message, AuthorName: "Антон", Text: "@channel @here @пользователь *важно*"}
	got := Render(event, model.Mattermost, "", true)
	for _, mention := range []string{"@channel", "@here", "@пользователь"} {
		if strings.Contains(got, mention) {
			t.Fatalf("mention was not neutralized: %q", got)
		}
	}
	if !strings.Contains(got, "\\*важно\\*") {
		t.Fatalf("markdown was not escaped: %q", got)
	}
}

func TestSplitRespectsRuneLimit(t *testing.T) {
	parts := Split(strings.Repeat("я", 80), 30)
	if len(parts) < 2 {
		t.Fatal("expected multiple parts")
	}
	for _, part := range parts {
		if utf8.RuneCountInString(part) > 30 {
			t.Fatalf("part exceeds limit: %q", part)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	if got := SafeFilename("../bad\nname.txt"); got != ".._bad name.txt" {
		t.Fatalf("unexpected name %q", got)
	}
}
