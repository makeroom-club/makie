package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"

	"github.com/Southclaws/storyden/sdk/go/storyden"
	"github.com/makeroom-club/makie/bot"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	pl, err := storyden.New(ctx)
	if err != nil {
		logger.Error("failed to initialise plugin", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		if err := pl.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("plugin shutdown returned error", slog.String("error", err.Error()))
		}
	}()

	app := &bot.PluginApp{
		Plugin: pl,
		Logger: logger,
	}

	pl.OnConfigure(app.HandleConfigure)

	go app.SyncInitialConfig(ctx)

	if err := pl.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		logger.Error("plugin stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
