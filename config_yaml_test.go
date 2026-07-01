package main

import "testing"

func TestYAMLRoundTrip(t *testing.T) {
	c := &Config{
		TelegramBotToken:    "123:ABC",
		AllowedUserIDs:      []int64{111, 222},
		AllowedUsernames:    []string{"alice"},
		ManagerModel:        "haiku",
		WorkerModel:         "sonnet",
		ManagerAlways:       false,
		ClaudePath:          "",
		ClaudeOauthToken:    "sk-ant-oat01-X",
		DefaultBackend:      "claude",
		CodexModel:          "o4-mini",
		TimeoutMinutes:      10,
		MaxWorkers:          3,
		RateLimitPerMin:     20,
		AllowScripts:        false,
		ScreenControl:       true,
		ScreenPresetsFile:   "",
		ScreenElevated:      true,
		ConversationTTLDays: 45,
	}
	b, err := marshalConfigYAML(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalConfigYAML(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.TelegramBotToken != c.TelegramBotToken ||
		len(got.AllowedUserIDs) != 2 || got.AllowedUserIDs[1] != 222 ||
		got.WorkerModel != "sonnet" || got.ManagerAlways != false ||
		got.ClaudeOauthToken != "sk-ant-oat01-X" || got.DefaultBackend != "claude" ||
		got.MaxWorkers != 3 || got.RateLimitPerMin != 20 || got.ScreenControl != true ||
		got.ScreenElevated != true || got.ConversationTTLDays != 45 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestYAMLDefaults(t *testing.T) {
	// Minimal YAML → defaults applied, validate passes.
	y := []byte("telegram:\n  bot_token: t\n  allowed_user_ids: [1]\n")
	got, err := unmarshalConfigYAML(y)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManagerModel != "haiku" || got.TimeoutMinutes != 10 || got.MaxWorkers != 3 ||
		got.RateLimitPerMin != 20 || got.ManagerAlways != true || got.ConversationTTLDays != 30 {
		t.Errorf("defaults wrong: %+v", got)
	}
}
