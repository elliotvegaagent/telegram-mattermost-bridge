package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Route struct {
	ID          string `json:"id"`
	TGChatID    int64  `json:"tg_chat_id"`
	MMChannelID string `json:"mm_channel_id"`
}

type Settings struct {
	MMURL              string
	MMBotToken         string
	TGBotToken         string
	Routes             []Route
	DBPath             string
	HTTPBind           string
	LogLevel           string
	MaxAttachmentBytes int64
	TGAPIBase          string
}

func required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func FromEnv() (Settings, error) {
	mmURL, err := required("MM_URL")
	if err != nil {
		return Settings{}, err
	}
	mmToken, err := required("MM_BOT_TOKEN")
	if err != nil {
		return Settings{}, err
	}
	tgToken, err := required("TG_BOT_TOKEN")
	if err != nil {
		return Settings{}, err
	}

	bind := strings.TrimSpace(envOr("HTTP_BIND", "0.0.0.0:8080"))
	_, portText, err := net.SplitHostPort(bind)
	if err != nil {
		return Settings{}, fmt.Errorf("HTTP_BIND must have the form host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Settings{}, fmt.Errorf("HTTP_BIND contains an invalid port")
	}

	maxBytes, err := strconv.ParseInt(envOr("MAX_ATTACHMENT_BYTES", "26214400"), 10, 64)
	if err != nil || maxBytes <= 0 {
		return Settings{}, fmt.Errorf("MAX_ATTACHMENT_BYTES must be positive integer")
	}
	routes, err := routesFromEnv()
	if err != nil {
		return Settings{}, err
	}

	return Settings{
		MMURL: strings.TrimRight(mmURL, "/"), MMBotToken: mmToken, TGBotToken: tgToken,
		Routes: routes, DBPath: filepath.Clean(envOr("DB_PATH", "/data/bridge.db")),
		HTTPBind: bind, LogLevel: strings.ToUpper(envOr("LOG_LEVEL", "INFO")),
		MaxAttachmentBytes: maxBytes,
		TGAPIBase:          strings.TrimRight(envOr("TG_API_BASE", "https://api.telegram.org"), "/"),
	}, nil
}

func routesFromEnv() ([]Route, error) {
	var routes []Route
	if raw := strings.TrimSpace(os.Getenv("BRIDGE_PAIRS")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &routes); err != nil || len(routes) == 0 {
			return nil, fmt.Errorf("BRIDGE_PAIRS must be a non-empty valid JSON array")
		}
	} else {
		chatText, err := required("TG_CHAT_ID")
		if err != nil {
			return nil, err
		}
		chatID, err := strconv.ParseInt(chatText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TG_CHAT_ID must be an integer")
		}
		channelID, err := required("MM_CHANNEL_ID")
		if err != nil {
			return nil, err
		}
		routes = []Route{{ID: "default", TGChatID: chatID, MMChannelID: channelID}}
	}
	ids, chats, channels := map[string]bool{}, map[int64]bool{}, map[string]bool{}
	for i, route := range routes {
		route.ID = strings.TrimSpace(route.ID)
		route.MMChannelID = strings.TrimSpace(route.MMChannelID)
		if route.ID == "" || route.MMChannelID == "" || route.TGChatID == 0 {
			return nil, fmt.Errorf("BRIDGE_PAIRS[%d] requires id, tg_chat_id and mm_channel_id", i)
		}
		if ids[route.ID] || chats[route.TGChatID] || channels[route.MMChannelID] {
			return nil, fmt.Errorf("BRIDGE_PAIRS contains duplicate id, Telegram chat or Mattermost channel")
		}
		ids[route.ID], chats[route.TGChatID], channels[route.MMChannelID] = true, true, true
		routes[i] = route
	}
	return routes, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
