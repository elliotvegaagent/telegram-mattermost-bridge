package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/gateway"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/health"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/mattermost"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/platform"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/storage"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/telegram"
)

type Parts struct {
	Telegram   *telegram.Adapter
	Mattermost *mattermost.Adapter
	Health     *health.State
}

func NewParts(settings config.Settings, logger *slog.Logger) Parts {
	state := health.New()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second}}
	return Parts{Telegram: telegram.New(client, settings.TGBotToken, settings.TGAPIBase, settings.Routes, state.Callback("telegram"), logger.With("adapter", "telegram")), Mattermost: mattermost.New(client, settings.MMURL, settings.MMBotToken, settings.Routes, state.Callback("mattermost"), logger.With("adapter", "mattermost")), Health: state}
}

func Doctor(ctx context.Context, settings config.Settings, store *storage.Store, logger *slog.Logger) (map[string]any, error) {
	parts := NewParts(settings, logger)
	tgMe, err := parts.Telegram.GetMe(ctx)
	if err != nil {
		return nil, err
	}
	mmMe, err := parts.Mattermost.GetMe(ctx)
	if err != nil {
		return nil, err
	}
	pairs := []map[string]any{}
	for _, route := range settings.Routes {
		chat, err := parts.Telegram.GetChat(ctx, route.ID)
		if err != nil {
			return nil, err
		}
		member, err := parts.Telegram.GetChatMember(ctx, route.ID, tgMe.ID)
		if err != nil {
			return nil, err
		}
		channel, err := parts.Mattermost.GetChannel(ctx, route.ID)
		if err != nil {
			return nil, err
		}
		if _, err := parts.Mattermost.GetChannelMember(ctx, route.ID, mmMe.ID); err != nil {
			return nil, err
		}
		pairs = append(pairs, map[string]any{"id": route.ID, "telegram": map[string]any{"chat_id": chat.ID, "chat_title": firstNonEmpty(chat.Title, chat.Username), "membership": member["status"]}, "mattermost": map[string]any{"channel_id": channel.ID, "channel": firstNonEmpty(channel.DisplayName, channel.Name)}})
	}
	if err := store.SetState(ctx, "doctor_last_run_ms", strconv.FormatInt(time.Now().UnixMilli(), 10)); err != nil {
		return nil, err
	}
	initialized, err := store.IsInitialized(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok", "telegram_bot": firstNonEmpty(tgMe.Username, strconv.FormatInt(tgMe.ID, 10)), "mattermost_bot": firstNonEmpty(mmMe.Username, mmMe.ID), "pairs": pairs, "database": settings.DBPath, "initialized": initialized, "implementation": "go"}, nil
}

func Initialize(ctx context.Context, settings config.Settings, store *storage.Store, logger *slog.Logger, force bool) (map[string]any, error) {
	initialized, err := store.IsInitialized(ctx)
	if err != nil {
		return nil, err
	}
	if initialized && !force {
		return nil, fmt.Errorf("bridge is already initialized; use --force to reset checkpoints")
	}
	result, err := Doctor(ctx, settings, store, logger)
	if err != nil {
		return nil, err
	}
	parts := NewParts(settings, logger)
	if err := parts.Telegram.DropPendingUpdates(ctx); err != nil {
		return nil, err
	}
	routeIDs := make([]string, len(settings.Routes))
	for i, route := range settings.Routes {
		routeIDs[i] = route.ID
	}
	if err := store.Initialize(ctx, time.Now().UnixMilli(), 0, routeIDs); err != nil {
		return nil, err
	}
	result["initialized"] = true
	result["history_policy"] = "pending Telegram updates dropped; Mattermost starts now"
	return result, nil
}

func Smoke(ctx context.Context, settings config.Settings, store *storage.Store, logger *slog.Logger) (map[string]any, error) {
	initialized, err := store.IsInitialized(ctx)
	if err != nil {
		return nil, err
	}
	if !initialized {
		return nil, fmt.Errorf("bridge is not initialized")
	}
	parts := NewParts(settings, logger)
	gw, err := gateway.New(store, map[model.Platform]platform.Adapter{model.Telegram: parts.Telegram, model.Mattermost: parts.Mattermost}, settings.Routes, settings.MaxAttachmentBytes, logger.With("module", "gateway"))
	if err != nil {
		return nil, err
	}
	timestamp := time.Now().UnixMilli()
	eventIDs := []string{}
	for _, route := range settings.Routes {
		events := []model.Event{
			{EventID: fmt.Sprintf("smoke:mm:%s:%d", route.ID, timestamp), Platform: model.Mattermost, Kind: model.Message, MessageID: fmt.Sprintf("smoke-mm-%d", timestamp), MessageIDs: []string{fmt.Sprintf("smoke-mm-%d", timestamp)}, AuthorID: "smoke", AuthorName: "Проверка Go bridge", RouteID: route.ID, Text: "✅ Go bridge: Mattermost → Telegram", CreatedAtMS: timestamp},
			{EventID: fmt.Sprintf("smoke:tg:%s:%d", route.ID, timestamp), Platform: model.Telegram, Kind: model.Message, MessageID: fmt.Sprintf("smoke-tg-%d", timestamp), MessageIDs: []string{fmt.Sprintf("smoke-tg-%d", timestamp)}, AuthorID: "smoke", AuthorName: "Проверка Go bridge", RouteID: route.ID, Text: "✅ Go bridge: Telegram → Mattermost", CreatedAtMS: timestamp},
		}
		for _, event := range events {
			if _, err := gw.Accept(ctx, event); err != nil {
				return nil, err
			}
			eventIDs = append(eventIDs, event.EventID)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- gw.Run(runCtx) }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-runDone:
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("smoke worker stopped before delivery")
		case <-deadline.C:
			return nil, fmt.Errorf("smoke delivery timed out")
		case <-ticker.C:
			counts, err := store.Counts(ctx)
			if err != nil {
				return nil, err
			}
			if counts["queued"] == 0 && counts["retry"] == 0 && counts["delivering"] == 0 {
				cancel()
				return map[string]any{"status": "ok", "implementation": "go", "delivered_events": eventIDs}, nil
			}
		}
	}
}

func Run(ctx context.Context, settings config.Settings, store *storage.Store, logger *slog.Logger) error {
	initialized, err := store.IsInitialized(ctx)
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("bridge is not initialized; run `bridge init` first")
	}
	parts := NewParts(settings, logger)
	tgMe, err := parts.Telegram.GetMe(ctx)
	if err != nil {
		return err
	}
	mmMe, err := parts.Mattermost.GetMe(ctx)
	if err != nil {
		return err
	}
	adapters := map[model.Platform]platform.Adapter{model.Telegram: parts.Telegram, model.Mattermost: parts.Mattermost}
	gw, err := gateway.New(store, adapters, settings.Routes, settings.MaxAttachmentBytes, logger.With("module", "gateway"))
	if err != nil {
		return err
	}
	offsetText, err := store.GetState(ctx, "telegram_offset", "0")
	if err != nil {
		return err
	}
	offset, err := strconv.ParseInt(offsetText, 10, 64)
	if err != nil {
		return err
	}
	startup := time.Now().UnixMilli()
	legacyText, err := store.GetState(ctx, "mattermost_checkpoint_ms", strconv.FormatInt(startup, 10))
	if err != nil {
		return err
	}
	legacy, _ := strconv.ParseInt(legacyText, 10, 64)
	checkpoints := map[string]int64{}
	for _, route := range settings.Routes {
		key := "mattermost_checkpoint_ms:" + route.ID
		value, err := store.GetState(ctx, key, "")
		if err != nil {
			return err
		}
		if value == "" {
			checkpoint := startup
			if route.ID == "default" {
				checkpoint = legacy
			}
			if err := store.SetState(ctx, key, strconv.FormatInt(checkpoint, 10)); err != nil {
				return err
			}
			checkpoints[route.ID] = checkpoint
		} else {
			checkpoint, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil {
				return parseErr
			}
			checkpoints[route.ID] = checkpoint
		}
	}
	logger.Info("bridge_started", "implementation", "go", "routes", len(settings.Routes))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, 4)
	go func() { errors <- gw.Run(runCtx) }()
	go func() { errors <- parts.Telegram.RunPolling(runCtx, offset, tgMe.ID, gw.Accept) }()
	go func() { errors <- parts.Mattermost.RunWebSocket(runCtx, checkpoints, mmMe.ID, gw.Accept) }()
	go func() { errors <- health.RunServer(runCtx, settings.HTTPBind, health.Handler(parts.Health, store)) }()
	select {
	case <-ctx.Done():
		cancel()
		return nil
	case err := <-errors:
		cancel()
		return err
	}
}

func PrintJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
