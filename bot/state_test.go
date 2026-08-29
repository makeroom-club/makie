package bot

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/xid"
)

func TestChannelFocusAllowsOnlyTheActiveChannel(t *testing.T) {
	app := &PluginApp{}

	if !app.beginChannelRun("channel-a") {
		t.Fatal("first run in channel-a was rejected")
	}
	if !app.beginChannelRun("channel-a") {
		t.Fatal("concurrent run in channel-a was rejected")
	}
	if app.beginChannelRun("channel-b") {
		t.Fatal("run in channel-b was accepted while channel-a was active")
	}

	app.finishChannelRun()
	if app.beginChannelRun("channel-b") {
		t.Fatal("run in channel-b was accepted while another channel-a run remained active")
	}

	app.finishChannelRun()
	if !app.beginChannelRun("channel-b") {
		t.Fatal("run in channel-b was rejected after channel-a became idle")
	}
	app.finishChannelRun()
}

func TestConversationSessionContinuesWithOnlyUnseenMessages(t *testing.T) {
	app := &PluginApp{}
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	message := func(id string, at time.Time) *discordgo.Message {
		return &discordgo.Message{ID: id, Timestamp: at}
	}

	first, err := app.prepareConversationRun(context.Background(), "channel-a", base, []*discordgo.Message{
		message("3", base),
		message("2", base.Add(-time.Minute)),
		message("1", base.Add(-2*time.Minute)),
	})
	if err != nil {
		t.Fatalf("prepare first conversation run: %v", err)
	}
	if !first.IsNew {
		t.Fatal("first run did not create a session")
	}
	if !first.SessionID.IsNil() {
		t.Fatalf("first run generated session ID %s; Storyden should generate it", first.SessionID)
	}
	storydenSessionID := xid.New()
	if !app.establishConversationSession(first, storydenSessionID) {
		t.Fatal("failed to store the session ID returned by Storyden")
	}

	// This reply already exists as the assistant response in Storyden, so its
	// Discord copy must not be imported on the next run.
	app.recordConversationMessage(storydenSessionID, message("4", base.Add(time.Second)))

	second, err := app.prepareConversationRun(context.Background(), "channel-a", base.Add(2*time.Minute), []*discordgo.Message{
		message("6", base.Add(2*time.Minute)),
		message("5", base.Add(time.Minute)),
		message("4", base.Add(time.Second)),
		message("3", base),
		message("2", base.Add(-time.Minute)),
	})
	if err != nil {
		t.Fatalf("prepare second conversation run: %v", err)
	}
	if second.IsNew {
		t.Fatal("second run unexpectedly created a new session")
	}
	if second.SessionID != storydenSessionID {
		t.Fatalf("second run session ID = %s, want %s", second.SessionID, storydenSessionID)
	}

	gotIDs := make([]string, len(second.Messages))
	for i, msg := range second.Messages {
		gotIDs[i] = msg.ID
	}
	wantIDs := []string{"6", "5"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("second run message IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestConversationSessionExpiresAfterHistoryWindow(t *testing.T) {
	app := &PluginApp{}
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	message := func(id string, at time.Time) *discordgo.Message {
		return &discordgo.Message{ID: id, Timestamp: at}
	}

	first, err := app.prepareConversationRun(context.Background(), "channel-a", base, []*discordgo.Message{message("1", base)})
	if err != nil {
		t.Fatalf("prepare first conversation run: %v", err)
	}
	firstSessionID := xid.New()
	if !app.establishConversationSession(first, firstSessionID) {
		t.Fatal("failed to store the first session ID returned by Storyden")
	}

	expiredAt := base.Add(historyWindow + time.Second)
	second, err := app.prepareConversationRun(context.Background(), "channel-a", expiredAt, []*discordgo.Message{
		message("3", expiredAt),
		message("2", expiredAt.Add(-time.Minute)),
	})
	if err != nil {
		t.Fatalf("prepare expired conversation run: %v", err)
	}

	if !second.IsNew {
		t.Fatal("run after the history window did not create a new session")
	}
	if !second.SessionID.IsNil() {
		t.Fatalf("expired run generated session ID %s; Storyden should generate it", second.SessionID)
	}
	if len(second.Messages) != 2 {
		t.Fatalf("new session imported %d messages, want 2", len(second.Messages))
	}
}

func TestConversationContinuationWaitsForStorydenSessionID(t *testing.T) {
	app := &PluginApp{}
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	first, err := app.prepareConversationRun(context.Background(), "channel-a", base, []*discordgo.Message{{ID: "1", Timestamp: base}})
	if err != nil {
		t.Fatalf("prepare first conversation run: %v", err)
	}

	type result struct {
		run conversationRun
		err error
	}
	waiting := make(chan result, 1)
	go func() {
		run, err := app.prepareConversationRun(context.Background(), "channel-a", base.Add(time.Minute), []*discordgo.Message{{ID: "2", Timestamp: base.Add(time.Minute)}})
		waiting <- result{run: run, err: err}
	}()

	select {
	case result := <-waiting:
		t.Fatalf("continuation returned before Storyden supplied the session ID: %+v", result)
	case <-time.After(25 * time.Millisecond):
	}

	storydenSessionID := xid.New()
	if !app.establishConversationSession(first, storydenSessionID) {
		t.Fatal("failed to store the session ID returned by Storyden")
	}

	select {
	case result := <-waiting:
		if result.err != nil {
			t.Fatalf("prepare continuation: %v", result.err)
		}
		if result.run.IsNew {
			t.Fatal("continuation unexpectedly created a new session")
		}
		if result.run.SessionID != storydenSessionID {
			t.Fatalf("continuation session ID = %s, want %s", result.run.SessionID, storydenSessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("continuation remained blocked after Storyden supplied the session ID")
	}
}
