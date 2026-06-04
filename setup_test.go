package main

import (
	"path/filepath"
	"testing"
)

func TestWriteConfigFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.txt") // also tests dir creation
	if err := writeConfigFile(path, "123:ABC", 6723802240); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("generated config must be loadable: %v", err)
	}
	if cfg.TelegramBotToken != "123:ABC" {
		t.Errorf("token = %q", cfg.TelegramBotToken)
	}
	if len(cfg.AllowedUserIDs) != 1 || cfg.AllowedUserIDs[0] != 6723802240 {
		t.Errorf("ids = %v", cfg.AllowedUserIDs)
	}
	if cfg.ManagerModel != "haiku" || cfg.TimeoutMinutes != 10 || !cfg.ManagerAlways {
		t.Errorf("defaults wrong: %+v", cfg)
	}
}
