package bot

import (
	"reflect"
	"testing"
	"time"

	"github.com/Southclaws/opt"
	"github.com/Southclaws/storyden/lib/plugin/rpc"
	"github.com/bwmarrin/discordgo"
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

	got := buildRobotMessages(discordMessages, testBotID)
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
