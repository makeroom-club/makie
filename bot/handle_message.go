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
	historyLimit  = 20              // messages of context to pull, including the trigger
	historyWindow = 5 * time.Minute // only messages within this window of now are kept
)

func (a *PluginApp) handleDiscordMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
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
	if !a.beginChannelRun(m.ChannelID) {
		a.Logger.Info("ignoring mention while active in another channel", slog.String("channel_id", m.ChannelID), slog.String("user_id", m.Author.ID))
		return
	}
	defer a.finishChannelRun()

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
	// newer than everything fetched). buildRobotMessages reads this slice
	// newest-first and emits it in chronological order, so the trigger must be
	// at index 0 to become the final user message.
	messages = append([]*discordgo.Message{m.Message}, messages...)

	conversationRun, err := a.prepareConversationRun(ctx, m.ChannelID, m.Timestamp, messages)
	if err != nil {
		a.Logger.Warn("failed to prepare conversation run", slog.String("error", err.Error()), slog.String("channel_id", m.ChannelID), slog.String("message_id", m.ID))
		return
	}
	if len(conversationRun.Messages) == 0 {
		a.Logger.Debug("ignoring invocation with no new discord messages", slog.String("channel_id", m.ChannelID), slog.String("message_id", m.ID))
		return
	}
	robotMessages := buildRobotMessages(conversationRun.Messages, cfg.DiscordBotID)

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

// buildRobotMessages converts Discord's newest-first history into the
// chronological user/assistant message sequence expected by robot_run.
func buildRobotMessages(messages []*discordgo.Message, botID string) []rpc.RobotRunMessage {
	history := make([]rpc.RobotRunMessage, 0, len(messages))

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		role := rpc.RobotRunMessageRoleUser
		if msg.Author.ID == botID {
			role = rpc.RobotRunMessageRoleAssistant
		}

		content := msg.Content
		if strings.TrimSpace(content) == "" {
			content = "(no text content)"
		}

		history = append(history, rpc.RobotRunMessage{
			Role:    role,
			Content: content,
			Author: opt.NewIf(discordMessageAuthor(msg), func(author string) bool {
				return strings.TrimSpace(author) != ""
			}),
		})
	}

	return history
}
