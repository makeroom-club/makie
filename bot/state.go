package bot

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/Southclaws/storyden/app/transports/http/openapi"
	"github.com/bwmarrin/discordgo"
)

//go:embed personality.txt
var personalityTemplateSource string

var personalityTemplate = template.Must(template.New("personality").Parse(personalityTemplateSource))

const robotID = "default"

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
	var playbook strings.Builder
	if err := personalityTemplate.Execute(&playbook, struct{ Personality string }{Personality: personality}); err != nil {
		return fmt.Errorf("render personality template: %w", err)
	}
	rendered := playbook.String()

	client, err := a.Plugin.BuildAPIClient(ctx)
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
