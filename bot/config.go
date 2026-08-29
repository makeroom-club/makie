package bot

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	initialConfigRetryInterval = 2 * time.Second
	configureTimeout           = 15 * time.Second
)

type pluginConfig struct {
	DiscordToken string
	DiscordBotID string
}

func (a *PluginApp) HandleConfigure(ctx context.Context, raw map[string]any) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, configureTimeout)
	defer cancel()

	return a.applyConfig(timeoutCtx, raw, true)
}

func (a *PluginApp) SyncInitialConfig(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		raw, err := a.Plugin.GetConfig(ctx)
		if err != nil {
			a.Logger.Debug("initial configuration not available yet", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(initialConfigRetryInterval):
			}
			continue
		}

		if err := a.applyConfig(ctx, raw, false); err != nil {
			a.Logger.Warn("stored configuration is invalid", slog.String("error", err.Error()))
		}
		return
	}
}

func (a *PluginApp) applyConfig(ctx context.Context, raw map[string]any, requireComplete bool) error {
	_ = ctx

	cfg, complete, err := parseConfig(raw)
	if err != nil {
		return err
	}
	if !complete {
		a.setUnconfigured()
		a.Logger.Info("plugin is waiting for configuration")
		if requireComplete {
			return errors.New("configuration is incomplete")
		}
		return nil
	}

	a.mu.Lock()
	oldToken := a.config.DiscordToken
	a.config = cfg
	a.configured = true
	a.mu.Unlock()

	// Reconnect Discord if token changed.
	if oldToken != cfg.DiscordToken {
		a.connectDiscord(cfg.DiscordToken)
	}

	a.Logger.Info("plugin configuration applied")
	return nil
}

func parseConfig(raw map[string]any) (pluginConfig, bool, error) {
	token, ok := raw["discord_token"].(string)
	if !ok || token == "" {
		return pluginConfig{}, false, nil
	}

	botID, ok := raw["discord_bot_id"].(string)
	if !ok || botID == "" {
		return pluginConfig{}, false, nil
	}

	return pluginConfig{
		DiscordToken: token,
		DiscordBotID: botID,
	}, true, nil
}

func (a *PluginApp) setUnconfigured() {
	a.mu.Lock()
	a.config = pluginConfig{}
	a.configured = false
	a.mu.Unlock()

	if a.discord != nil {
		a.discord.Close()
		a.discord = nil
	}
}

func (a *PluginApp) currentConfig() (pluginConfig, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.config, a.configured
}
