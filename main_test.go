package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// TestBuildConversationPromptExample prints an example of the prompt sent to
// the robot for a realistic Discord conversation. Run with:
//
//	go test -run TestBuildConversationPromptExample -v
//
// and read the "=== PROMPT OUTPUT ===" block to eyeball formatting quality.
func TestBuildConversationPromptExample(t *testing.T) {
	base := time.Date(2026, 8, 8, 14, 30, 0, 0, time.UTC)
	at := func(offsetSeconds int) time.Time {
		return base.Add(time.Duration(offsetSeconds) * time.Second)
	}

	user := func(id, username string) *discordgo.User {
		return &discordgo.User{ID: id, Username: username}
	}

	// fetched mimics what s.ChannelMessages(channelID, limit, beforeID, "", "")
	// returns: messages strictly before the trigger, newest-first.
	fetched := []*discordgo.Message{
		{
			ID:        "8",
			Author:    user(botID, botName),
			Content:   "the magnetron agrees with gremlin here",
			Timestamp: at(75),
		},
		{
			ID:        "7",
			Author:    user("218743840243318784", "gremlin"),
			Content:   "never",
			Timestamp: at(60),
		},
		{
			ID:        "6",
			Author:    user("285684164613898243", "southclaws"),
			Content:   "can you two stay on topic for once",
			Timestamp: at(45),
		},
		{
			ID:        "5",
			Author:    user("534768504943935500", "notif_goblin"),
			Content:   "lol classic",
			Timestamp: at(30),
		},
		{
			ID:        "4",
			Author:    user(botID, botName),
			Content:   "Hual remains an unresolved transmission. Ask him yourself, I'm not writing his biography.",
			Timestamp: at(15),
		},
		{
			ID:        "3",
			Author:    user("218743840243318784", "gremlin"),
			Content:   "<@1309527755339075634> thoughts on hual",
			Timestamp: at(0),
		},
	}

	// The trigger is the message that just arrived: newer than everything
	// fetched, so it goes at index 0 — see the comment above the equivalent
	// line in handleDiscordMessage.
	trigger := &discordgo.Message{
		ID:        "9",
		Author:    user("534768504943935500", "notif_goblin"),
		Content:   "<@1309527755339075634> ok but seriously what should I get southclaws for his birthday",
		Timestamp: at(90),
	}
	messages := append([]*discordgo.Message{trigger}, fetched...)

	prompt := buildConversationPrompt(messages)

	fmt.Println("=== PROMPT OUTPUT ===")
	fmt.Println(prompt)
	fmt.Println("=== END PROMPT OUTPUT ===")
}
