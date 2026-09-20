package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Southclaws/opt"
	"github.com/Southclaws/storyden/lib/plugin/rpc"
	"github.com/bwmarrin/discordgo"
)

const (
	historyLimit    = 20              // messages of context to pull, including the trigger
	historyWindow   = 5 * time.Minute // only messages within this window of now are kept
	robotRunTimeout = 2 * time.Minute
)

func (a *PluginApp) handleDiscordMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m == nil || m.Message == nil || m.Author == nil {
		return
	}

	a.Logger.Info("received discord message", slog.String("author_id", m.Author.ID), slog.Int("mentions", len(m.Mentions)))

	if m.Author.ID == s.State.User.ID {
		a.Logger.Debug("ignoring own message")
		return
	}

	cfg, configured := a.currentConfig()
	if !configured {
		a.Logger.Warn("plugin not configured, skipping reply", slog.String("user_id", m.Author.ID))
		return
	}

	if m.GuildID != "" && cfg.TypeSafeAPIKey != "" {
		go a.reactWithWiltedRose(s, m.Message, cfg.TypeSafeAPIKey)
	}

	mentionsBot := discordMessageMentionsBot(m.Message, cfg.DiscordBotID)
	triggerMessage := m.Message
	var referenced *discordgo.Message
	var referenceErr error
	if !mentionsBot {
		if m.Message == nil || m.Message.Type != discordgo.MessageTypeReply {
			a.Logger.Debug("message from target user did not mention bot or reply to bot", slog.String("user_id", m.Author.ID))
			return
		}

		// A direct reply to one of Makie's messages is also an invocation. We
		// resolve the reference before returning so this still works when
		// Discord omits ReferencedMessage from the gateway event.
		referenced, referenceErr = resolveReferencedMessage(s, m.Message)
		if referenceErr != nil {
			a.Logger.Debug("failed to resolve referenced discord message", slog.String("error", referenceErr.Error()), slog.String("channel_id", m.ChannelID), slog.String("message_id", m.ID))
			return
		}
		if !discordMessageAuthoredBy(referenced, cfg.DiscordBotID) {
			a.Logger.Debug("message from target user did not mention bot or reply to bot", slog.String("user_id", m.Author.ID))
			return
		}

		// Without the privileged Message Content intent, Discord may deliver an
		// unmentioned guild reply without its text. Fetch the complete message so
		// a bare reply is still useful to the robot.
		if fetched, err := s.ChannelMessage(m.ChannelID, m.ID); err != nil {
			a.Logger.Debug("failed to fetch complete direct reply", slog.String("error", err.Error()), slog.String("channel_id", m.ChannelID), slog.String("message_id", m.ID))
		} else if fetched != nil {
			triggerMessage = fetched
		}
	}
	if !a.beginChannelRun(m.ChannelID) {
		a.Logger.Info("ignoring invocation while active in another channel", slog.String("channel_id", m.ChannelID), slog.String("user_id", m.Author.ID))
		return
	}
	defer a.finishChannelRun()

	a.Logger.Info("received invocation from target user, will respond", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))

	ctx, cancel := context.WithTimeout(context.Background(), robotRunTimeout)
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
	// newer than everything fetched). buildRobotMessages reads this slice
	// newest-first and emits it in chronological order, so the trigger must be
	// at index 0 to become the final user message.
	if referenced == nil && referenceErr == nil {
		referenced, referenceErr = resolveReferencedMessage(s, m.Message)
	}
	if referenceErr != nil {
		a.Logger.Debug("failed to fetch referenced discord message", slog.String("error", referenceErr.Error()), slog.String("channel_id", m.ChannelID), slog.String("message_id", m.ID))
	} else {
		messages = insertReferencedMessage(messages, referenced)
	}
	messages = append([]*discordgo.Message{triggerMessage}, messages...)

	conversationRun, err := a.prepareConversationRun(ctx, m.ChannelID, m.Timestamp, messages)
	if err != nil {
		a.Logger.Warn("failed to prepare conversation run", slog.String("error", err.Error()), slog.String("channel_id", m.ChannelID), slog.String("message_id", m.ID))
		return
	}
	imageAttachments := discordImageAttachmentsForTrigger(mentionsBot, triggerMessage, referenced, cfg.DiscordBotID)
	robotMessages := a.buildRobotMessagesForRun(ctx, conversationRun, cfg.DiscordBotID, m, imageAttachments)
	if len(robotMessages) == 0 {
		return
	}

	sessionIDLog := "new"
	params := rpc.RPCRequestRobotRunParams{
		Mode:     rpc.RobotRunModeConversation,
		RobotID:  cfg.RobotID,
		Messages: robotMessages,
	}
	if !conversationRun.SessionID.IsNil() {
		sessionIDLog = conversationRun.SessionID.String()
		params.SessionID = opt.New(conversationRun.SessionID)
	}

	a.Logger.Info("calling robot to generate response", slog.String("user_id", m.Author.ID), slog.String("robot_id", cfg.RobotID), slog.String("session_id", sessionIDLog), slog.Bool("new_session", conversationRun.IsNew), slog.Int("message_count", len(robotMessages)))
	stopTyping := a.startDiscordTyping(ctx, s, m.ChannelID)

	run, err := a.Plugin.RunRobot(ctx, params)
	stopTyping()

	sessionID := conversationRun.SessionID
	if run != nil {
		if returnedSessionID, ok := run.SessionID.Get(); ok {
			if conversationRun.IsNew {
				if !a.establishConversationSession(conversationRun, returnedSessionID) {
					a.Logger.Warn("conversation session changed before robot call completed", slog.String("returned_session_id", returnedSessionID.String()), slog.String("channel_id", m.ChannelID))
				}
				sessionID = returnedSessionID
			} else if returnedSessionID != sessionID {
				a.Logger.Error("robot returned an unexpected session ID", slog.String("expected_session_id", sessionID.String()), slog.String("returned_session_id", returnedSessionID.String()))
				return
			}
		}
	}
	if conversationRun.IsNew && sessionID.IsNil() {
		a.failConversationSession(conversationRun)
	}
	if err != nil {
		a.Logger.Error("robot call failed", slog.String("error", err.Error()), slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))
		return
	}
	if sessionID.IsNil() {
		a.Logger.Error("robot response did not contain a session ID", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))
		return
	}
	response, ok := run.FinalText.Get()
	if !ok || strings.TrimSpace(response) == "" {
		a.Logger.Error("robot response did not contain final text", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID))
		return
	}

	a.Logger.Info("robot generated response", slog.String("user_id", m.Author.ID), slog.String("response_length", fmt.Sprintf("%d", len(response))))

	// Post response back to Discord as a reply.
	if err := a.sendConversationReply(s, m.ChannelID, response, m.Reference(), sessionID); err != nil {
		a.Logger.Error("failed to send discord reply", slog.String("error", err.Error()), slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("channel_id", m.ChannelID))
		return
	}

	a.Logger.Info("successfully replied to message", slog.String("user_id", m.Author.ID), slog.String("message_id", m.ID), slog.String("user_message", m.Content), slog.String("bot_response", response))
}

func discordMessageMentionsBot(message *discordgo.Message, botID string) bool {
	if message == nil {
		return false
	}

	for _, mention := range message.Mentions {
		if mention != nil && mention.ID == botID {
			return true
		}
	}

	// A bot mention is normally decoded into Mentions. Keep the raw-token
	// fallback because Discord may omit the parsed mention data when a gateway
	// payload is partially redacted by message-content intent settings.
	return strings.Contains(message.Content, "<@"+botID+">") ||
		strings.Contains(message.Content, "<@!"+botID+">")
}

func discordMessageAuthoredBy(message *discordgo.Message, authorID string) bool {
	return message != nil && message.Author != nil && message.Author.ID == authorID
}

// resolveReferencedMessage returns the message that triggered a Discord reply.
// Discord normally includes it in the gateway event, but can omit it when the
// referenced message cannot be resolved. Fetching it by ID lets the robot see
// the context even when the original message is outside the recent history
// window.
func resolveReferencedMessage(session *discordgo.Session, message *discordgo.Message) (*discordgo.Message, error) {
	if message == nil {
		return nil, nil
	}
	if message.ReferencedMessage != nil && message.ReferencedMessage.Author != nil && message.ReferencedMessage.Author.ID != "" {
		return message.ReferencedMessage, nil
	}
	if message.MessageReference == nil || message.MessageReference.MessageID == "" {
		return nil, nil
	}

	channelID := message.MessageReference.ChannelID
	if channelID == "" {
		channelID = message.ChannelID
	}
	return session.ChannelMessage(channelID, message.MessageReference.MessageID)
}

// insertReferencedMessage keeps messages in Discord's newest-first order and
// avoids adding the referenced message twice when it is already in the recent
// channel history.
func insertReferencedMessage(messages []*discordgo.Message, referenced *discordgo.Message) []*discordgo.Message {
	if referenced == nil {
		return messages
	}
	if referenced.ID != "" {
		for _, message := range messages {
			if message != nil && message.ID == referenced.ID {
				return messages
			}
		}
	}

	insertAt := len(messages)
	for i, message := range messages {
		if message != nil && referenced.Timestamp.After(message.Timestamp) {
			insertAt = i
			break
		}
	}

	messages = append(messages, nil)
	copy(messages[insertAt+1:], messages[insertAt:])
	messages[insertAt] = referenced
	return messages
}

// buildRobotMessages converts Discord's newest-first history into the
// chronological user/assistant message sequence expected by robot_run.
func buildRobotMessages(messages []*discordgo.Message, botID string, mediaByMessageID map[string][]rpc.RobotRunMedia) []rpc.RobotRunMessage {
	history := make([]rpc.RobotRunMessage, 0, len(messages))

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		role := rpc.RobotRunMessageRoleUser
		if msg.Author.ID == botID {
			role = rpc.RobotRunMessageRoleAssistant
		}

		robotMessage := rpc.RobotRunMessage{
			Role:    role,
			Content: discordMessageContent(msg),
			Author: opt.NewIf(discordMessageAuthor(msg), func(author string) bool {
				return strings.TrimSpace(author) != ""
			}),
		}
		if role == rpc.RobotRunMessageRoleUser {
			robotMessage.Media = mediaByMessageID[msg.ID]
		}

		history = append(history, robotMessage)
	}

	return history
}

// discordMessageContent formats the text sent to robot_run while preserving
// the fact that a message is a Discord reply.
func discordMessageContent(message *discordgo.Message) string {
	if message == nil {
		return "(no text content)"
	}

	content := strings.TrimSpace(message.Content)
	if message.Type == discordgo.MessageTypeReply {
		if content == "" {
			content = "(no text content)"
		}
		content = "[Reply to another Discord message]\n" + content
	}
	if content == "" {
		return "(no text content)"
	}
	return content
}
