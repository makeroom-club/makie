package bot

import (
	"reflect"
	"testing"
	"time"

	"github.com/Southclaws/opt"
	"github.com/Southclaws/storyden/lib/plugin/rpc"
	"github.com/bwmarrin/discordgo"
	"github.com/rs/xid"
)

const testBotID = "1400000000000000001"

func TestBuildRobotMessages(t *testing.T) {
	base := time.Date(2026, 8, 8, 14, 30, 0, 0, time.UTC)
	at := func(offsetSeconds int) time.Time {
		return base.Add(time.Duration(offsetSeconds) * time.Second)
	}
	user := func(id, username, globalName string) *discordgo.User {
		return &discordgo.User{ID: id, Username: username, GlobalName: globalName}
	}

	// Discord returns the triggering message and fetched history newest-first.
	discordMessages := []*discordgo.Message{
		{
			ID:        "5",
			Author:    user("285684164613898243", "southclaws", ""),
			Content:   "<@1400000000000000001> what should I get for his birthday?",
			Timestamp: at(60),
		},
		{
			ID:        "4",
			Author:    user(testBotID, "makie_bot", "Makie"),
			Content:   "Ask him yourself, I'm not writing his biography.",
			Timestamp: at(45),
		},
		{
			ID:        "3",
			Author:    user("534768504943935500", "notif_goblin", "Notification Goblin"),
			Content:   "lol classic",
			Timestamp: at(30),
		},
		{
			ID:        "2",
			Author:    user("218743840243318784", "gremlin", "Gremlin Global"),
			Member:    &discordgo.Member{Nick: "Gremlin Server"},
			Content:   "   ",
			Timestamp: at(15),
		},
	}

	got := buildRobotMessages(discordMessages, testBotID, nil)
	want := []rpc.RobotRunMessage{
		{
			Role:    rpc.RobotRunMessageRoleUser,
			Content: "(no text content)",
			Author:  opt.New("Gremlin Server (@gremlin)"),
		},
		{
			Role:    rpc.RobotRunMessageRoleUser,
			Content: "lol classic",
			Author:  opt.New("Notification Goblin (@notif_goblin)"),
		},
		{
			Role:    rpc.RobotRunMessageRoleAssistant,
			Content: "Ask him yourself, I'm not writing his biography.",
			Author:  opt.New("Makie (@makie_bot)"),
		},
		{
			Role:    rpc.RobotRunMessageRoleUser,
			Content: "<@1400000000000000001> what should I get for his birthday?",
			Author:  opt.New("southclaws (@southclaws)"),
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildRobotMessages() = %#v, want %#v", got, want)
	}
}

func TestBuildRobotMessagesIncludesOrderedUserMedia(t *testing.T) {
	firstID := xid.New()
	secondID := xid.New()
	messages := []*discordgo.Message{
		{
			ID:      "trigger",
			Author:  &discordgo.User{ID: "user"},
			Content: "can you see these?",
		},
		{
			ID:      "assistant",
			Author:  &discordgo.User{ID: testBotID},
			Content: "Earlier response",
		},
	}
	media := map[string][]rpc.RobotRunMedia{
		"trigger": {
			{Type: rpc.RobotRunMediaTypeImage, AssetID: firstID},
			{Type: rpc.RobotRunMediaTypeImage, AssetID: secondID},
		},
		"assistant": {
			{Type: rpc.RobotRunMediaTypeImage, AssetID: xid.New()},
		},
	}

	got := buildRobotMessages(messages, testBotID, media)
	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2", len(got))
	}
	if len(got[0].Media) != 0 {
		t.Fatalf("assistant media = %#v, want none", got[0].Media)
	}
	if want := media["trigger"]; !reflect.DeepEqual(got[1].Media, want) {
		t.Fatalf("user media = %#v, want %#v", got[1].Media, want)
	}
}

func TestDiscordMessageContentMarksReplies(t *testing.T) {
	message := &discordgo.Message{
		Type:    discordgo.MessageTypeReply,
		Content: "<@" + testBotID + "> is this true?",
	}

	want := "[Reply to another Discord message]\n<@" + testBotID + "> is this true?"
	if got := discordMessageContent(message); got != want {
		t.Fatalf("reply content = %q, want %q", got, want)
	}
}

func TestDiscordMessageMentionsBotFallsBackToRawMention(t *testing.T) {
	message := &discordgo.Message{Content: "<@!" + testBotID + "> is this true?"}
	if !discordMessageMentionsBot(message, testBotID) {
		t.Fatal("discordMessageMentionsBot() = false, want true for raw nickname mention")
	}
}

func TestInsertReferencedMessagePreservesNewestFirstOrder(t *testing.T) {
	base := time.Date(2026, 8, 8, 14, 30, 0, 0, time.UTC)
	message := func(id string, offset time.Duration) *discordgo.Message {
		return &discordgo.Message{ID: id, Timestamp: base.Add(offset)}
	}

	history := []*discordgo.Message{
		message("5", 5*time.Minute),
		message("3", 3*time.Minute),
	}
	history = insertReferencedMessage(history, message("4", 4*time.Minute))

	gotIDs := make([]string, len(history))
	for i, msg := range history {
		gotIDs[i] = msg.ID
	}
	if want := []string{"5", "4", "3"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("message order = %v, want %v", gotIDs, want)
	}

	history = insertReferencedMessage(history, message("4", 4*time.Minute))
	if len(history) != 3 {
		t.Fatalf("duplicate referenced message changed history length to %d, want 3", len(history))
	}
}
