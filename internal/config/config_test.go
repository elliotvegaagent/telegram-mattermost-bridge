package config

import "testing"

func TestFromEnvMultipleRoutes(t *testing.T) {
	t.Setenv("MM_URL", "https://mattermost.example.com/")
	t.Setenv("MM_BOT_TOKEN", "mm-secret")
	t.Setenv("TG_BOT_TOKEN", "tg-secret")
	t.Setenv("BRIDGE_PAIRS", `[{"id":"a","tg_chat_id":-1001,"mm_channel_id":"c1"},{"id":"b","tg_chat_id":-1002,"mm_channel_id":"c2","mm_to_tg_author_mode":"hidden"}]`)
	settings, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Routes) != 2 || settings.MMURL != "https://mattermost.example.com" {
		t.Fatalf("settings %#v", settings)
	}
	if settings.Routes[0].MMToTGAuthorMode != MMToTGAuthorName || !settings.Routes[0].IncludeMMToTGAuthor() {
		t.Fatalf("default author mode %#v", settings.Routes[0])
	}
	if settings.Routes[1].MMToTGAuthorMode != MMToTGAuthorHidden || settings.Routes[1].IncludeMMToTGAuthor() {
		t.Fatalf("hidden author mode %#v", settings.Routes[1])
	}
}

func TestFromEnvRejectsDuplicateRoute(t *testing.T) {
	t.Setenv("MM_URL", "https://mattermost.example.com")
	t.Setenv("MM_BOT_TOKEN", "x")
	t.Setenv("TG_BOT_TOKEN", "x")
	t.Setenv("BRIDGE_PAIRS", `[{"id":"a","tg_chat_id":-1,"mm_channel_id":"c1"},{"id":"a","tg_chat_id":-2,"mm_channel_id":"c2"}]`)
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestFromEnvRejectsUnknownAuthorMode(t *testing.T) {
	t.Setenv("MM_URL", "https://mattermost.example.com")
	t.Setenv("MM_BOT_TOKEN", "x")
	t.Setenv("TG_BOT_TOKEN", "x")
	t.Setenv("BRIDGE_PAIRS", `[{"id":"a","tg_chat_id":-1,"mm_channel_id":"c1","mm_to_tg_author_mode":"anonymous"}]`)
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected author mode error")
	}
}
