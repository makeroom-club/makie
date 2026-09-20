package bot

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const (
	initialConfigRetryInterval = 2 * time.Second
	configureTimeout           = 15 * time.Second
	defaultRobotID             = "denbot"
)

type pluginConfig struct {
	DiscordToken   string
	DiscordBotID   string
	RobotID        string
	TypeSafeAPIKey string
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
	oldBotID := a.config.DiscordBotID
	oldRobotID := a.config.RobotID
	oldReactionsEnabled := a.config.TypeSafeAPIKey != ""
	a.config = cfg
	a.configured = true
	a.mu.Unlock()
	if oldBotID != cfg.DiscordBotID || oldRobotID != cfg.RobotID {
		a.resetConversationSession()
	}

	// Message Content is privileged, so only request it when reactions are enabled.
	if oldToken != cfg.DiscordToken || oldReactionsEnabled != (cfg.TypeSafeAPIKey != "") {
		a.connectDiscord(cfg.DiscordToken, cfg.TypeSafeAPIKey != "")
	}

	a.Logger.Info("plugin configuration applied", slog.Bool("wilted_rose_enabled", cfg.TypeSafeAPIKey != ""))
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
	robotID, _ := raw["robot_id"].(string)
	robotID = strings.TrimSpace(robotID)
	if robotID == "" {
		robotID = defaultRobotID
	}

	apiKey, _ := raw["typesafe_api_key"].(string)

	return pluginConfig{
		TypeSafeAPIKey: strings.TrimSpace(apiKey),
		DiscordToken:   token,
		DiscordBotID:   botID,
		RobotID:        robotID,
	}, true, nil
}

func (a *PluginApp) setUnconfigured() {
	a.mu.Lock()
	a.config = pluginConfig{}
	a.configured = false
	a.mu.Unlock()
	a.resetConversationSession()

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
