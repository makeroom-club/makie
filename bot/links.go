package bot

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

var messageURLPattern = regexp.MustCompile("(?i)https?://[^\\s<>\"`]+")

func instagramEmbedLinks(content string) []string {
	var links []string
	seen := make(map[string]bool)
	for _, match := range messageURLPattern.FindAllString(content, -1) {
		// Exclude surrounding prose and Markdown link punctuation.
		link, err := url.Parse(strings.TrimRight(match, ".,;:!?)]}'"))
		if err != nil || link.User != nil {
			continue
		}
		switch strings.ToLower(link.Host) {
		case "instagram.com", "www.instagram.com":
			link.Host = "kkinstagram.com"
		default:
			continue
		}
		fixed := link.String()
		if !seen[fixed] {
			links = append(links, fixed)
			seen[fixed] = true
		}
	}
	return links
}

func (a *PluginApp) replyWithInstagramLinks(session *discordgo.Session, message *discordgo.Message) {
	links := instagramEmbedLinks(message.Content)
	if len(links) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Send links separately so a message containing several links still fits
	// Discord's message length limit after expanding the domain names.
	for _, link := range links {
		_, err := session.ChannelMessageSendComplex(message.ChannelID, &discordgo.MessageSend{
			Content:         link,
			Reference:       message.SoftReference(),
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}, discordgo.WithContext(ctx))
		if err != nil {
			a.Logger.Warn("failed to send Instagram embed link", slog.String("error", err.Error()), slog.String("channel_id", message.ChannelID), slog.String("message_id", message.ID))
			return
		}
	}
}
