package bot

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/Southclaws/storyden/app/transports/http/openapi"
	"github.com/bwmarrin/discordgo"
	"github.com/rs/xid"
)

//go:embed personality.txt
var personalityTemplateSource string

var personalityTemplate = template.Must(template.New("personality").Parse(personalityTemplateSource))

type conversationSessionState struct {
	ID            xid.ID
	ChannelID     string
	LastMessageAt time.Time
	SeenMessages  map[string]time.Time
	Ready         chan struct{}
}

type conversationRun struct {
	SessionID xid.ID
	Messages  []*discordgo.Message
	IsNew     bool
	ready     chan struct{}
}

func (a *PluginApp) resetConversationSession() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	a.closeConversationReadyLocked()
	a.conversation = conversationSessionState{}
}

func (a *PluginApp) closeConversationReadyLocked() {
	if a.conversation.Ready != nil {
		close(a.conversation.Ready)
		a.conversation.Ready = nil
	}
}

func (a *PluginApp) beginChannelRun(channelID string) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if a.activeRuns > 0 && a.activeChannelID != channelID {
		return false
	}

	a.activeChannelID = channelID
	a.activeRuns++
	return true
}

func (a *PluginApp) finishChannelRun() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if a.activeRuns == 0 {
		return
	}

	a.activeRuns--
	if a.activeRuns == 0 {
		a.activeChannelID = ""
	}
}

func (a *PluginApp) prepareConversationRun(ctx context.Context, channelID string, triggerAt time.Time, messages []*discordgo.Message) (conversationRun, error) {
	if triggerAt.IsZero() {
		triggerAt = time.Now()
	}

	for {
		a.stateMu.Lock()

		isNew := a.conversation.ChannelID == "" ||
			a.conversation.ChannelID != channelID ||
			triggerAt.After(a.conversation.LastMessageAt.Add(historyWindow))
		if isNew {
			a.closeConversationReadyLocked()
			a.conversation = conversationSessionState{
				ChannelID:     channelID,
				LastMessageAt: triggerAt,
				SeenMessages:  make(map[string]time.Time, len(messages)),
				Ready:         make(chan struct{}),
			}
		} else if a.conversation.ID.IsNil() {
			ready := a.conversation.Ready
			a.stateMu.Unlock()

			select {
			case <-ctx.Done():
				return conversationRun{}, ctx.Err()
			case <-ready:
				continue
			}
		}

		cutoff := triggerAt.Add(-historyWindow)
		for messageID, seenAt := range a.conversation.SeenMessages {
			if seenAt.Before(cutoff) {
				delete(a.conversation.SeenMessages, messageID)
			}
		}

		unseen := make([]*discordgo.Message, 0, len(messages))
		for _, message := range messages {
			if message.ID != "" {
				if _, seen := a.conversation.SeenMessages[message.ID]; seen {
					continue
				}
			}

			unseen = append(unseen, message)
			if message.ID != "" {
				seenAt := message.Timestamp
				if seenAt.IsZero() {
					seenAt = triggerAt
				}
				a.conversation.SeenMessages[message.ID] = seenAt
			}
		}

		if triggerAt.After(a.conversation.LastMessageAt) {
			a.conversation.LastMessageAt = triggerAt
		}

		run := conversationRun{
			SessionID: a.conversation.ID,
			Messages:  unseen,
			IsNew:     isNew,
			ready:     a.conversation.Ready,
		}
		a.stateMu.Unlock()
		return run, nil
	}
}

func (a *PluginApp) establishConversationSession(run conversationRun, sessionID xid.ID) bool {
	if !run.IsNew || run.ready == nil || sessionID.IsNil() {
		return false
	}

	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if a.conversation.Ready != run.ready || !a.conversation.ID.IsNil() {
		return false
	}

	a.conversation.ID = sessionID
	a.closeConversationReadyLocked()
	return true
}

func (a *PluginApp) failConversationSession(run conversationRun) {
	if !run.IsNew || run.ready == nil {
		return
	}

	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if a.conversation.Ready != run.ready {
		return
	}

	a.closeConversationReadyLocked()
	a.conversation = conversationSessionState{}
}

func (a *PluginApp) recordConversationMessage(sessionID xid.ID, message *discordgo.Message) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.recordConversationMessageLocked(sessionID, message)
}

func (a *PluginApp) recordConversationMessageLocked(sessionID xid.ID, message *discordgo.Message) {
	if message == nil || message.ID == "" {
		return
	}
	if a.conversation.ID != sessionID {
		return
	}

	seenAt := message.Timestamp
	if seenAt.IsZero() {
		seenAt = time.Now()
	}
	a.conversation.SeenMessages[message.ID] = seenAt
	if seenAt.After(a.conversation.LastMessageAt) {
		a.conversation.LastMessageAt = seenAt
	}
}

func (a *PluginApp) sendConversationReply(session *discordgo.Session, channelID, response string, reference *discordgo.MessageReference, sessionID xid.ID) error {
	// Keep reply creation and deduplication atomic. A concurrent invocation may
	// already be fetching channel history, but it cannot reserve that history
	// until this Discord message ID has been marked as already persisted in the
	// Storyden session.
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	sentMessage, err := session.ChannelMessageSendReply(channelID, response, reference)
	if err != nil {
		return err
	}
	a.recordConversationMessageLocked(sessionID, sentMessage)
	return nil
}

func (a *PluginApp) handlePersonalityCommand(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	if len(data.Options) == 0 {
		a.Logger.Warn("personality command invoked without a prompt option")
		return
	}
	personality := data.Options[0].StringValue()

	// Playbook updates hit the Storyden API, which can take longer than
	// Discord's 3 second initial-response window, so acknowledge immediately
	// and edit the response once the update completes.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		a.Logger.Error("failed to acknowledge personality command", slog.String("error", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.updatePersonality(ctx, personality); err != nil {
		a.Logger.Error("failed to update robot personality", slog.String("error", err.Error()))
		content := fmt.Sprintf("Couldn't update personality: %s", err.Error())
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			a.Logger.Error("failed to edit personality command response", slog.String("error", err.Error()))
		}
		return
	}

	invokerID := ""
	if i.Member != nil && i.Member.User != nil {
		invokerID = i.Member.User.ID
	}
	a.Logger.Info("updated robot personality", slog.String("user_id", invokerID))
	content := "Personality updated."
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		a.Logger.Error("failed to edit personality command response", slog.String("error", err.Error()))
	}
}

func (a *PluginApp) updatePersonality(ctx context.Context, personality string) error {
	cfg, configured := a.currentConfig()
	if !configured {
		return errors.New("configuration is incomplete")
	}

	var playbook strings.Builder
	if err := personalityTemplate.Execute(&playbook, struct{ Personality string }{Personality: personality}); err != nil {
		return fmt.Errorf("render personality template: %w", err)
	}
	rendered := playbook.String()

	client, err := a.Plugin.BuildAPIClient(ctx)
	if err != nil {
		return fmt.Errorf("build api client: %w", err)
	}

	resp, err := client.RobotUpdateWithResponse(ctx, cfg.RobotID, openapi.RobotMutableProps{
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
