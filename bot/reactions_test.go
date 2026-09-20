package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type reactionTransport func(*http.Request) (*http.Response, error)

func (f reactionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWiltedRoseEvaluation(t *testing.T) {
	for _, tt := range []struct {
		name, body    string
		status        int
		want, wantErr bool
	}{
		{"yes", `{"answers":{"wilted_rose":{"type":"noul","noul":0.99}}}`, 200, true, false},
		{"threshold", `{"answers":{"wilted_rose":{"type":"noul","noul":0.85}}}`, 200, true, false},
		{"uncertain", `{"answers":{"wilted_rose":{"type":"noul","noul":0.84}}}`, 200, false, false},
		{"no", `{"answers":{"wilted_rose":{"type":"noul","noul":0}}}`, 200, false, false},
		{"missing answer", `{"answers":{}}`, 200, false, true},
		{"missing probability", `{"answers":{"wilted_rose":{"type":"noul"}}}`, 200, false, true},
		{"null probability", `{"answers":{"wilted_rose":{"type":"noul","noul":null}}}`, 200, false, true},
		{"wrong type", `{"answers":{"wilted_rose":{"type":"score","noul":1}}}`, 200, false, true},
		{"out of range", `{"answers":{"wilted_rose":{"type":"noul","noul":1.2}}}`, 200, false, true},
		{"malformed", `{`, 200, false, true},
		{"unauthorized", `secret echoed here`, 401, false, true},
		{"rate limited", `{}`, 429, false, true},
		{"unavailable", `{}`, 529, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: reactionTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.URL.String() != "https://api.typesafe.ai/v1/systemone" {
					t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("missing authentication or JSON header")
				}
				var body struct {
					Model     string            `json:"model"`
					State     map[string]string `json:"state"`
					Questions map[string]struct {
						Type, Instructions string
						Criteria           map[string]string
					} `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Model != "jev-latest" || body.State["message"] != "three hours on the wrong branch" || body.State["replying_to"] != "how is the fix going?" {
					t.Errorf("wrong request: %+v", body)
				}
				question := body.Questions["wilted_rose"]
				if len(body.Questions) != 1 || question.Type != "noul" || !strings.Contains(question.Instructions, "Should I react with a 🥀 to this message?") || question.Criteria["true"] == "" || question.Criteria["false"] == "" {
					t.Errorf("wrong question: %+v", question)
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			})}
			got, err := shouldReactWithWiltedRose(context.Background(), client, "test-key", &discordgo.Message{Content: "three hours on the wrong branch", ReferencedMessage: &discordgo.Message{Content: "how is the fix going?"}})
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("got %v, %v; want %v, error=%v", got, err, tt.want, tt.wantErr)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want one without retry storm", calls)
			}
			if err != nil && strings.Contains(err.Error(), "secret echoed") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}

func TestWiltedRoseUnmentionedGuildMessage(t *testing.T) {
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	evaluated := make(chan struct{}, 1)
	http.DefaultClient = &http.Client{Transport: reactionTransport(func(r *http.Request) (*http.Response, error) {
		evaluated <- struct{}{}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"answers":{"wilted_rose":{"type":"noul","noul":0.99}}}`)), Header: make(http.Header)}, nil
	})}
	reacted := make(chan string, 1)
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	session.State.User = &discordgo.User{ID: "makie"}
	session.Client = &http.Client{Transport: reactionTransport(func(r *http.Request) (*http.Response, error) {
		reacted <- r.Method + " " + r.URL.Path
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	app := &PluginApp{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), configured: true, config: pluginConfig{DiscordBotID: "makie", TypeSafeAPIKey: "key"}, activeChannelID: "another-channel", activeRuns: 1}
	app.handleDiscordMessage(session, &discordgo.MessageCreate{Message: &discordgo.Message{ID: "message", ChannelID: "channel", GuildID: "guild", Author: &discordgo.User{ID: "someone"}, Content: "spent 3 hours debugging the wrong branch"}})
	select {
	case got := <-reacted:
		if got != "PUT /api/v9/channels/channel/messages/message/reactions/🥀/@me" {
			t.Fatalf("unexpected reaction: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unmentioned message did not get evaluated and reacted to")
	}
	<-evaluated
	// These must never start an evaluation.
	for _, tt := range []struct{ name, guild, author, key string }{
		{"DM", "", "someone", "key"},
		{"own message", "guild", "makie", "key"},
		{"disabled", "guild", "someone", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app.config.TypeSafeAPIKey = tt.key
			app.handleDiscordMessage(session, &discordgo.MessageCreate{Message: &discordgo.Message{ID: "ignored", ChannelID: "channel", GuildID: tt.guild, Author: &discordgo.User{ID: tt.author}, Content: "oh no"}})
		})
	}
	select {
	case <-evaluated:
		t.Fatal("excluded message was evaluated")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestWiltedRoseCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: reactionTransport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if react, err := shouldReactWithWiltedRose(ctx, client, "key", &discordgo.Message{}); react || err == nil {
		t.Fatalf("cancelled evaluation = %v, %v", react, err)
	}
}
