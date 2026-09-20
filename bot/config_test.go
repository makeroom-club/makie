package bot

import "testing"

func TestParseConfigRobotID(t *testing.T) {
	tests := []struct {
		name        string
		robotID     any
		wantRobotID string
	}{
		{name: "default", wantRobotID: "denbot"},
		{name: "empty", robotID: "", wantRobotID: "denbot"},
		{name: "configured", robotID: "moderator", wantRobotID: "moderator"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := map[string]any{
				"discord_token":  "token",
				"discord_bot_id": "1400000000000000001",
			}
			if tt.robotID != nil {
				raw["robot_id"] = tt.robotID
			}

			got, complete, err := parseConfig(raw)
			if err != nil {
				t.Fatalf("parseConfig() error = %v", err)
			}
			if !complete {
				t.Fatal("parseConfig() complete = false, want true")
			}
			if got.RobotID != tt.wantRobotID {
				t.Fatalf("parseConfig() RobotID = %q, want %q", got.RobotID, tt.wantRobotID)
			}
		})
	}
}

func TestParseConfigTypeSafeAPIKey(t *testing.T) {
	for _, key := range []string{"", "  ", "  typesafe-secret  "} {
		cfg, complete, err := parseConfig(map[string]any{"discord_token": "token", "discord_bot_id": "bot", "typesafe_api_key": key})
		if err != nil || !complete {
			t.Fatalf("optional key broke config: %v, %v", complete, err)
		}
		want := ""
		if key == "  typesafe-secret  " {
			want = "typesafe-secret"
		}
		if cfg.TypeSafeAPIKey != want {
			t.Fatalf("key = %q, want %q", cfg.TypeSafeAPIKey, want)
		}
	}
}
