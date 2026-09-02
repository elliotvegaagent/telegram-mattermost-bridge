package config

import "testing"

func TestFromEnvMultipleRoutes(t *testing.T) {
	t.Setenv("MM_URL", "https://mattermost.example.com/")
	t.Setenv("MM_BOT_TOKEN", "mm-secret")
	t.Setenv("TG_BOT_TOKEN", "tg-secret")
	t.Setenv("BRIDGE_PAIRS", `[{"id":"a","tg_chat_id":-1001,"mm_channel_id":"c1"},{"id":"b","tg_chat_id":-1002,"mm_channel_id":"c2"}]`)
	settings, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Routes) != 2 || settings.MMURL != "https://mattermost.example.com" {
		t.Fatalf("settings %#v", settings)
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
