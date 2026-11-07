package bot

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestInstagramEmbedLinks(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          []string
	}{
		{
			name:    "post with query and fragment",
			content: "look https://instagram.com/p/ABC_123/?img_index=2&igsh=abc%3D#photo",
			want:    []string{"https://kkinstagram.com/p/ABC_123/?img_index=2&igsh=abc%3D#photo"},
		},
		{
			name:    "www reel and HTTP",
			content: "http://www.Instagram.com/reel/AbC/",
			want:    []string{"http://kkinstagram.com/reel/AbC/"},
		},
		{
			name:    "multiple links and duplicate",
			content: "https://instagram.com/p/one/ https://example.com/ https://www.instagram.com/p/two/ https://instagram.com/p/one/",
			want:    []string{"https://kkinstagram.com/p/one/", "https://kkinstagram.com/p/two/"},
		},
		{
			name:    "Markdown and punctuation",
			content: "[post](https://instagram.com/p/one/), <https://www.instagram.com/reel/two/>.",
			want:    []string{"https://kkinstagram.com/p/one/", "https://kkinstagram.com/reel/two/"},
		},
		{
			name:    "other hosts and already fixed links",
			content: "https://kkinstagram.com/p/one/ https://notinstagram.com/p/one/ https://instagram.com.example.org/p/one/ https://instagram.com@example.org/p/one/ https://example.org/instagram.com/p/one/ https://x.com/user/status/123",
		},
		{
			name:    "invalid URL",
			content: "https://instagram.com/p/%zz",
		},
		{
			name:    "plain text",
			content: "talking about instagram.com",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := instagramEmbedLinks(tt.content); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("instagramEmbedLinks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstagramLinkReplyWithoutMentionOrReactions(t *testing.T) {
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	session.State.User = &discordgo.User{ID: "makie", Bot: true}
	var replies []discordgo.MessageSend
	session.Client = &http.Client{Transport: reactionTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v9/channels/channel/messages" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var reply discordgo.MessageSend
		if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, reply)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"sent"}`)), Header: make(http.Header)}, nil
	})}
	app := &PluginApp{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		configured:      true,
		config:          pluginConfig{DiscordBotID: "makie"},
		activeChannelID: "another-channel",
		activeRuns:      1,
	}
	message := &discordgo.Message{
		ID:        "original",
		ChannelID: "channel",
		GuildID:   "guild",
		Author:    &discordgo.User{ID: "someone"},
		Content:   "https://www.instagram.com/reel/AbC/?igsh=123",
	}
	app.handleDiscordMessage(session, &discordgo.MessageCreate{Message: message})
	if len(replies) != 1 {
		t.Fatalf("reply count = %d, want 1", len(replies))
	}
	reply := replies[0]
	if reply.Content != "https://kkinstagram.com/reel/AbC/?igsh=123" {
		t.Fatalf("reply content = %q", reply.Content)
	}
	if reply.Reference == nil || reply.Reference.MessageID != "original" {
		t.Fatalf("reply reference = %+v", reply.Reference)
	}
	if reply.AllowedMentions == nil || reply.AllowedMentions.RepliedUser || len(reply.AllowedMentions.Parse) != 0 {
		t.Fatalf("unexpected mentions: %+v", reply.AllowedMentions)
	}
	if message.Content != "https://www.instagram.com/reel/AbC/?igsh=123" {
		t.Fatal("original message content changed")
	}

	for _, author := range []*discordgo.User{
		{ID: "makie"},
		{ID: "other-bot", Bot: true},
	} {
		message.Author = author
		app.handleDiscordMessage(session, &discordgo.MessageCreate{Message: message})
	}
	message.Author = &discordgo.User{ID: "webhook"}
	message.WebhookID = "webhook"
	app.handleDiscordMessage(session, &discordgo.MessageCreate{Message: message})
	if len(replies) != 1 {
		t.Fatalf("bot or webhook message triggered extra replies: %d", len(replies))
	}
}
