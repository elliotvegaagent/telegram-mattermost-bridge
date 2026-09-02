package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	app "github.com/elliotvegaagent/telegram-mattermost-bridge/internal/runtime"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: bridge <doctor|init|run|smoke|healthcheck>")
	}
	command := os.Args[1]
	if command == "healthcheck" {
		bind := os.Getenv("HTTP_BIND")
		if bind == "" {
			bind = "0.0.0.0:8080"
		}
		_, port, err := net.SplitHostPort(bind)
		if err != nil {
			return fmt.Errorf("invalid HTTP_BIND: %w", err)
		}
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("health endpoint returned %d", response.StatusCode)
		}
		return nil
	}
	force := false
	confirmed := false
	if command == "init" {
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		flags.BoolVar(&force, "force", false, "reset checkpoints when already initialized")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
	} else if command == "smoke" {
		flags := flag.NewFlagSet("smoke", flag.ContinueOnError)
		flags.BoolVar(&confirmed, "confirm", false, "send visible smoke messages to both platforms")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if !confirmed {
			return fmt.Errorf("smoke sends visible messages; use --confirm")
		}
	} else if command != "doctor" && command != "run" {
		return fmt.Errorf("unknown command %q", command)
	}
	settings, err := config.FromEnv()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	switch strings.ToUpper(settings.LogLevel) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	store, err := storage.Open(settings.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	migrateCtx, migrateCancel := context.WithTimeout(ctx, 30*time.Second)
	defer migrateCancel()
	if err := store.Migrate(migrateCtx); err != nil {
		return err
	}
	switch command {
	case "doctor":
		result, err := app.Doctor(ctx, settings, store, logger)
		if err != nil {
			return err
		}
		return app.PrintJSON(result)
	case "init":
		result, err := app.Initialize(ctx, settings, store, logger, force)
		if err != nil {
			return err
		}
		return app.PrintJSON(result)
	case "smoke":
		result, err := app.Smoke(ctx, settings, store, logger)
		if err != nil {
			return err
		}
		return app.PrintJSON(result)
	default:
		return app.Run(ctx, settings, store, logger)
	}
}
