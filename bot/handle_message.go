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
	historyLimit  = 20              // messages of context to pull, including the trigger
	historyWindow = 5 * time.Minute // only messages within this window of now are kept
)

func (a *PluginApp) handleDiscordMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	a.Logger.Info("received ANY discord message", slog.String("author_id", m.Author.ID), slog.String("bot_id", s.State.User.ID), slog.Int("mentions", len(m.Mentions)))

	// Ignore bot messages.
	if m.Author.ID == s.State.User.ID {
		a.Logger.Debug("ignoring own message")
		return
	}

	cfg, configured := a.currentConfig()
	if !configured {
		a.Logger.Warn("plugin not configured, skipping reply", slog.String("user_id", m.Author.ID))
		return
	}

	isMentioned := false
	for _, mention := range m.Mentions {
		if mention.ID == cfg.DiscordBotID {
			isMentioned = true
			break
		}
	}
	if !isMentioned {
		a.Logger.Debug("message from target user did not mention bot", slog.String("user_id", m.Author.ID))
		return
	}

	a.Logger.Info("received mention from target user, will respond", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch recent messages for context. historyLimit-1 because the triggering
	// message itself is appended explicitly below.
	messages, err := s.ChannelMessages(m.ChannelID, historyLimit-1, m.ID, "", "")
	if err != nil {
		a.Logger.Warn("failed to fetch message history", slog.String("error", err.Error()), slog.String("channel_id", m.ChannelID))
		messages = []*discordgo.Message{}
	}

	// ChannelMessages returns messages newest-first, so once we hit one older
	// than the window cutoff every remaining message is also older; truncate
	// from there instead of keeping the full historyLimit.
	cutoff := time.Now().Add(-historyWindow)
	for i, msg := range messages {
		if msg.Timestamp.Before(cutoff) {
			messages = messages[:i]
			break
		}
	}

	// ChannelMessages with a before-ID returns messages before the triggering
	// message, newest-first, so add the trigger explicitly at the front (it's
	// newer than everything fetched). buildConversationPrompt reads this slice
	// newest-first and prints it in reverse, so the trigger must be at index 0
	// to end up last in the rendered prompt. Without this, the robot only sees
	// the preceding conversation and can answer the wrong message.
	messages = append([]*discordgo.Message{m.Message}, messages...)

	prompt := buildConversationPrompt(messages, cfg.DiscordBotID, botName)

	a.Logger.Info("calling robot to generate response", slog.String("user_id", m.Author.ID), slog.String("robot_id", robotID))

	// Call robot to generate response.
	response, err := a.Plugin.RunRobot(ctx, robotID, prompt)
	if err != nil {
		a.Logger.Error("robot call failed", slog.String("error", err.Error()), slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))
		return
	}

	a.Logger.Info("robot generated response", slog.String("user_id", m.Author.ID), slog.String("response_length", fmt.Sprintf("%d", len(response))))

	// Post response back to Discord as a reply.
	_, err = s.ChannelMessageSendReply(m.ChannelID, response, m.Reference())
	if err != nil {
		a.Logger.Error("failed to send discord reply", slog.String("error", err.Error()), slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("channel_id", m.ChannelID))
		return
	}

	a.Logger.Info("successfully replied to message", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("user_message", m.Content), slog.String("bot_response", response))
}

// buildConversationPrompt renders a Discord message history into an
// XML-tagged prompt for the robot. XML-style markers give the model an
// unambiguous way to tell one message apart from the next even when content
// spans multiple lines or itself contains dashes/colons, and the bot's own
// past messages are flagged with is_you="true" so it knows what it already
// said. The instruction to answer only the last message keeps the model from
// latching onto an earlier question that happens to be easier to answer.
func buildConversationPrompt(messages []*discordgo.Message, botID, botName string) string {
	var sb strings.Builder

	fmt.Fprintf(
		&sb,
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

		fmt.Fprintf(
			&sb,
			"  <message author=%q author_id=%q time=%q is_you=%q>\n    %s\n  </message>\n",
			msg.Author.Username, msg.Author.ID, msg.Timestamp.Format("15:04:05"), fmt.Sprintf("%t", isYou), content,
		)
	}
	sb.WriteString("</discord_history>\n\n")

	sb.WriteString("Reply naturally and directly to the LAST <message> in <discord_history>. Do not answer or continue any earlier message.")

	return sb.String()
}
