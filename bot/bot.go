package bot

import (
	"log/slog"
	"sync"

	"github.com/Southclaws/storyden/sdk/go/storyden"
	"github.com/bwmarrin/discordgo"
)

type PluginApp struct {
	Plugin *storyden.Plugin
	Logger *slog.Logger

	mu         sync.RWMutex
	config     pluginConfig
	configured bool

	discord *discordgo.Session
}
