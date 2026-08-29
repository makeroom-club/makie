package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	botName               = "Makie"
	typingRefreshInterval = 8 * time.Second
)

func (a *PluginApp) startDiscordTyping(ctx context.Context, session *discordgo.Session, channelID string) context.CancelFunc {
	typingCtx, cancel := context.WithCancel(ctx)

	if err := session.ChannelTyping(channelID); err != nil {
		a.Logger.Warn("failed to start discord typing indicator", slog.String("error", err.Error()), slog.String("channel_id", channelID))
	}

	go func() {
		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				if err := session.ChannelTyping(channelID); err != nil {
					a.Logger.Warn("failed to refresh discord typing indicator", slog.String("error", err.Error()), slog.String("channel_id", channelID))
				}
			}
		}
	}()

	return cancel
}

func discordMessageAuthor(message *discordgo.Message) string {
	if message.Author == nil {
		return ""
	}

	username := strings.TrimSpace(message.Author.Username)
	displayName := ""
	if message.Member != nil {
		displayName = strings.TrimSpace(message.Member.Nick)
	}
	if displayName == "" {
		displayName = strings.TrimSpace(message.Author.GlobalName)
	}
	if displayName == "" {
		displayName = username
	}

	if username == "" {
		return displayName
	}
	return fmt.Sprintf("%s (@%s)", displayName, username)
}

func (a *PluginApp) connectDiscord(token string) {
	if a.discord != nil {
		a.discord.Close()
	}

	a.Logger.Info("starting discord connection", slog.String("token_length", fmt.Sprintf("%d", len(token))))

	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		a.Logger.Error("failed to create discord session", slog.String("error", err.Error()))
		return
	}

	// Enable required intents.
	sess.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages

	a.Logger.Info("adding message handler")
	sess.AddHandler(a.handleDiscordMessage)
	sess.AddHandler(a.handleGuildCreate)
	sess.AddHandler(a.handleInteractionCreate)

	a.Logger.Info("opening discord session")
	if err := sess.Open(); err != nil {
		a.Logger.Error("failed to connect to discord", slog.String("error", err.Error()))
		return
	}

	a.discord = sess
	a.Logger.Info("discord connection established")
}

// personalityCommandPermission restricts /personality to members who can
// manage the server, since the command rewrites the robot's entire system
// prompt and shouldn't be open to anyone who can type in the channel.
var personalityCommandPermission int64 = discordgo.PermissionManageGuild

var personalityCommand = &discordgo.ApplicationCommand{
	Name:                     "personality",
	Description:              fmt.Sprintf("Change %s's personality", botName),
	DefaultMemberPermissions: &personalityCommandPermission,
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "prompt",
			Description: "Description of the personality/behaviour to adopt",
			Required:    true,
		},
	},
}

// handleGuildCreate registers the slash commands in every guild the bot is
// a member of. It fires both for guilds already joined at startup and for
// guilds joined afterwards, and ApplicationCommandCreate upserts by name, so
// it's safe to call on every occurrence without tracking registration state.
func (a *PluginApp) handleGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	if _, err := s.ApplicationCommandCreate(s.State.User.ID, g.ID, personalityCommand); err != nil {
		a.Logger.Error("failed to register slash commands", slog.String("error", err.Error()), slog.String("guild_id", g.ID))
		return
	}

	a.Logger.Info("registered slash commands", slog.String("guild_id", g.ID))
}

func (a *PluginApp) handleInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	data := i.ApplicationCommandData()
	if data.Name != personalityCommand.Name {
		return
	}

	a.handlePersonalityCommand(s, i, data)
}
