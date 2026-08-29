package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Southclaws/storyden/app/transports/http/openapi"
	"github.com/Southclaws/storyden/sdk/go/storyden"
	"github.com/bwmarrin/discordgo"
)

//go:embed personality.txt
var personalityTemplateSource string

var personalityTemplate = template.Must(template.New("personality").Parse(personalityTemplateSource))

func init() {
	rand.Seed(time.Now().UnixNano())
}

const (
	initialConfigRetryInterval = 2 * time.Second
	configureTimeout           = 15 * time.Second
	targetUserID               = "285684164613898243"
	botID                      = "1309527755339075634"
	botName                    = "Makeroom"
	robotID                    = "d94r50jara0c2aq6dpj0"
	chatChance                 = 0.05 // 5% chance to reply
	historyLimit               = 15   // messages of context to pull, including the trigger
)

type pluginConfig struct {
	DiscordToken string
}

type pluginApp struct {
	plugin *storyden.Plugin
	logger *slog.Logger

	mu         sync.RWMutex
	config     pluginConfig
	configured bool

	discord *discordgo.Session
}

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

	app := &pluginApp{
		plugin: pl,
		logger: logger,
	}

	pl.OnConfigure(app.handleConfigure)

	go app.syncInitialConfig(ctx)

	if err := pl.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		logger.Error("plugin stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func (a *pluginApp) handleConfigure(ctx context.Context, raw map[string]any) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, configureTimeout)
	defer cancel()

	return a.applyConfig(timeoutCtx, raw, true)
}

func (a *pluginApp) syncInitialConfig(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		raw, err := a.plugin.GetConfig(ctx)
		if err != nil {
			a.logger.Debug("initial configuration not available yet", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(initialConfigRetryInterval):
			}
			continue
		}

		if err := a.applyConfig(ctx, raw, false); err != nil {
			a.logger.Warn("stored configuration is invalid", slog.String("error", err.Error()))
		}
		return
	}
}

func (a *pluginApp) applyConfig(ctx context.Context, raw map[string]any, requireComplete bool) error {
	_ = ctx

	cfg, complete, err := parseConfig(raw)
	if err != nil {
		return err
	}
	if !complete {
		a.setUnconfigured()
		a.logger.Info("plugin is waiting for configuration")
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

	// Reconnect Discord if token changed
	if oldToken != cfg.DiscordToken {
		a.connectDiscord(cfg.DiscordToken)
	}

	a.logger.Info("plugin configuration applied")
	return nil
}

func parseConfig(raw map[string]any) (pluginConfig, bool, error) {
	token, ok := raw["discord_token"].(string)
	if !ok || token == "" {
		return pluginConfig{}, false, nil
	}

	return pluginConfig{
		DiscordToken: token,
	}, true, nil
}

func (a *pluginApp) setUnconfigured() {
	a.mu.Lock()
	a.config = pluginConfig{}
	a.configured = false
	a.mu.Unlock()

	if a.discord != nil {
		a.discord.Close()
		a.discord = nil
	}
}

func (a *pluginApp) currentConfig() (pluginConfig, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.config, a.configured
}

func (a *pluginApp) connectDiscord(token string) {
	if a.discord != nil {
		a.discord.Close()
	}

	a.logger.Info("starting discord connection", slog.String("token_length", fmt.Sprintf("%d", len(token))))

	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		a.logger.Error("failed to create discord session", slog.String("error", err.Error()))
		return
	}

	// Enable required intents
	sess.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages

	a.logger.Info("adding message handler")
	sess.AddHandler(a.handleDiscordMessage)
	sess.AddHandler(a.handleGuildCreate)
	sess.AddHandler(a.handleInteractionCreate)

	a.logger.Info("opening discord session")
	if err := sess.Open(); err != nil {
		a.logger.Error("failed to connect to discord", slog.String("error", err.Error()))
		return
	}

	a.discord = sess
	a.logger.Info("discord connection established")
}

func (a *pluginApp) handleDiscordMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	a.logger.Info("received ANY discord message", slog.String("author_id", m.Author.ID), slog.String("bot_id", s.State.User.ID), slog.Int("mentions", len(m.Mentions)))

	// Ignore bot messages
	if m.Author.ID == s.State.User.ID {
		a.logger.Debug("ignoring own message")
		return
	}

	// Check if message is from target user or mentions the bot
	isMentioned := false
	for _, mention := range m.Mentions {
		a.logger.Info("found mention", slog.String("mention_id", mention.ID), slog.String("bot_id", botID), slog.Bool("is_bot", mention.ID == botID))
		if mention.ID == botID {
			isMentioned = true
			break
		}
	}

	isFromTarget := m.Author.ID == targetUserID
	shouldRespond := false

	// Always respond to mentions
	if isMentioned {
		shouldRespond = true
		a.logger.Info("received mention of bot, will respond", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))
	} else if isFromTarget {
		// 5% chance to respond to target user
		shouldRespond = rand.Float64() < chatChance
		if shouldRespond {
			a.logger.Info("received message from target user", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("content", m.Content))
		} else {
			a.logger.Debug("skipped reply due to probability", slog.String("user_id", m.Author.ID))
			return
		}
	} else {
		a.logger.Debug("message not from target user or mentioning bot", slog.String("user_id", m.Author.ID), slog.Bool("from_target", isFromTarget), slog.Bool("mentioned", isMentioned))
		return
	}

	a.logger.Info("proceeding with response", slog.Bool("should_respond", shouldRespond), slog.String("user_id", m.Author.ID))

	if !shouldRespond {
		return
	}

	_, configured := a.currentConfig()
	if !configured {
		a.logger.Warn("plugin not configured, skipping reply", slog.String("user_id", m.Author.ID))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch recent messages for context. historyLimit-1 because the triggering
	// message itself is appended explicitly below.
	messages, err := s.ChannelMessages(m.ChannelID, historyLimit-1, m.ID, "", "")
	if err != nil {
		a.logger.Warn("failed to fetch message history", slog.String("error", err.Error()), slog.String("channel_id", m.ChannelID))
		messages = []*discordgo.Message{}
	}

	// ChannelMessages with a before-ID returns messages before the triggering
	// message, newest-first, so add the trigger explicitly at the front (it's
	// newer than everything fetched). buildConversationPrompt reads this slice
	// newest-first and prints it in reverse, so the trigger must be at index 0
	// to end up last in the rendered prompt. Without this, the robot only sees
	// the preceding conversation and can answer the wrong message.
	messages = append([]*discordgo.Message{m.Message}, messages...)

	prompt := buildConversationPrompt(messages)

	a.logger.Info("calling robot to generate response", slog.String("user_id", m.Author.ID), slog.String("robot_id", robotID))

	// Call robot to generate response
	response, err := a.plugin.RunRobot(ctx, robotID, prompt)
	if err != nil {
		a.logger.Error("robot call failed", slog.String("error", err.Error()), slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))
		return
	}

	a.logger.Info("robot generated response", slog.String("user_id", m.Author.ID), slog.String("response_length", fmt.Sprintf("%d", len(response))))

	// Post response back to Discord as a reply
	_, err = s.ChannelMessageSendReply(m.ChannelID, response, m.Reference())
	if err != nil {
		a.logger.Error("failed to send discord reply", slog.String("error", err.Error()), slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("channel_id", m.ChannelID))
		return
	}

	a.logger.Info("successfully replied to message", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("user_message", m.Content), slog.String("bot_response", response))
}

// buildConversationPrompt renders a Discord message history into an
// XML-tagged prompt for the robot. XML-style markers give the model an
// unambiguous way to tell one message apart from the next even when content
// spans multiple lines or itself contains dashes/colons, and the bot's own
// past messages are flagged with is_you="true" so it knows what it already
// said. The instruction to answer only the last message keeps the model from
// latching onto an earlier question that happens to be easier to answer.
func buildConversationPrompt(messages []*discordgo.Message) string {
	var sb strings.Builder

	fmt.Fprintf(&sb,
		"You are %s, a Discord bot with user ID %s. The <discord_history> block below contains a snippet of recent messages from a Discord channel, oldest first. Each message is wrapped in a <message> tag with author, author_id, and time attributes. Messages you previously sent have is_you=\"true\".\n\n",
		botName, botID,
	)

	sb.WriteString("<discord_history>\n")
	// Reverse to chronological order (oldest first); ChannelMessages returns
	// newest first.
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		isYou := msg.Author.ID == botID
		content := msg.Content
		if content == "" {
			content = "(no text content)"
		}

		fmt.Fprintf(&sb,
			"  <message author=%q author_id=%q time=%q is_you=%q>\n    %s\n  </message>\n",
			msg.Author.Username, msg.Author.ID, msg.Timestamp.Format("15:04:05"), fmt.Sprintf("%t", isYou), content,
		)
	}
	sb.WriteString("</discord_history>\n\n")

	sb.WriteString("Reply naturally and directly to the LAST <message> in <discord_history>. Do not answer or continue any earlier message.")

	return sb.String()
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
func (a *pluginApp) handleGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	if _, err := s.ApplicationCommandCreate(s.State.User.ID, g.ID, personalityCommand); err != nil {
		a.logger.Error("failed to register slash commands", slog.String("error", err.Error()), slog.String("guild_id", g.ID))
		return
	}

	a.logger.Info("registered slash commands", slog.String("guild_id", g.ID))
}

func (a *pluginApp) handleInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	data := i.ApplicationCommandData()
	if data.Name != personalityCommand.Name {
		return
	}

	a.handlePersonalityCommand(s, i, data)
}

func (a *pluginApp) handlePersonalityCommand(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	if len(data.Options) == 0 {
		a.logger.Warn("personality command invoked without a prompt option")
		return
	}
	personality := data.Options[0].StringValue()

	// Playbook updates hit the Storyden API, which can take longer than
	// Discord's 3 second initial-response window, so acknowledge immediately
	// and edit the response once the update completes.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		a.logger.Error("failed to acknowledge personality command", slog.String("error", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.updatePersonality(ctx, personality); err != nil {
		a.logger.Error("failed to update robot personality", slog.String("error", err.Error()))
		content := fmt.Sprintf("Couldn't update personality: %s", err.Error())
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			a.logger.Error("failed to edit personality command response", slog.String("error", err.Error()))
		}
		return
	}

	invokerID := ""
	if i.Member != nil && i.Member.User != nil {
		invokerID = i.Member.User.ID
	}
	a.logger.Info("updated robot personality", slog.String("user_id", invokerID))
	content := "Personality updated."
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		a.logger.Error("failed to edit personality command response", slog.String("error", err.Error()))
	}
}

func (a *pluginApp) updatePersonality(ctx context.Context, personality string) error {
	var playbook strings.Builder
	if err := personalityTemplate.Execute(&playbook, struct{ Personality string }{Personality: personality}); err != nil {
		return fmt.Errorf("render personality template: %w", err)
	}
	rendered := playbook.String()

	client, err := a.plugin.BuildAPIClient(ctx)
	if err != nil {
		return fmt.Errorf("build api client: %w", err)
	}

	resp, err := client.RobotUpdateWithResponse(ctx, robotID, openapi.RobotMutableProps{
		Playbook: &rendered,
	})
	if err != nil {
		return fmt.Errorf("update robot: %w", err)
	}
	if resp.StatusCode() != 200 {
		return fmt.Errorf("update robot: unexpected status %s", resp.Status())
	}

	return nil
}
