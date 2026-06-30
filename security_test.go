package main

import (
	"testing"
)

// --- RateLimiter ---

func TestRateLimiter_Allow_UnlimitedWhenZero(t *testing.T) {
	r := NewRateLimiter(0)
	for range 1000 {
		if !r.Allow(1) {
			t.Fatal("maxPerMin=0 should always allow")
		}
	}
}

func TestRateLimiter_Allow_BlocksAtLimit(t *testing.T) {
	r := NewRateLimiter(3)
	for i := range 3 {
		if !r.Allow(1) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if r.Allow(1) {
		t.Error("4th request should be blocked")
	}
}

func TestRateLimiter_Allow_PerUserIsolation(t *testing.T) {
	r := NewRateLimiter(2)
	r.Allow(1)
	r.Allow(1) // user 1 at limit
	if !r.Allow(2) {
		t.Error("user 2 should not be affected by user 1's rate limit")
	}
}

func TestRateLimiter_Remaining_Unlimited(t *testing.T) {
	r := NewRateLimiter(0)
	if r.Remaining(1) != -1 {
		t.Error("unlimited should return -1")
	}
}

func TestRateLimiter_Remaining_CountsDown(t *testing.T) {
	r := NewRateLimiter(5)
	if r.Remaining(1) != 5 {
		t.Errorf("initial remaining = %d, want 5", r.Remaining(1))
	}
	r.Allow(1)
	if r.Remaining(1) != 4 {
		t.Errorf("remaining after 1 request = %d, want 4", r.Remaining(1))
	}
}

// --- validateScript ---

func TestValidateScript_DisabledByDefault(t *testing.T) {
	cfg := &Config{AllowScripts: false}
	if err := validateScript(cfg, "echo hello"); err == nil {
		t.Error("expected error when AllowScripts=false")
	}
}

func TestValidateScript_AllowedWhenEnabled(t *testing.T) {
	cfg := &Config{AllowScripts: true}
	if err := validateScript(cfg, "echo hello"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateScript_WhitelistAllow(t *testing.T) {
	cfg := &Config{AllowScripts: true, AllowedScriptCommands: []string{"echo", "curl"}}
	if err := validateScript(cfg, "echo hello world"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateScript_WhitelistBlock(t *testing.T) {
	cfg := &Config{AllowScripts: true, AllowedScriptCommands: []string{"echo"}}
	if err := validateScript(cfg, "rm -rf /"); err == nil {
		t.Error("expected error for non-whitelisted command")
	}
}

func TestValidateScript_EmptyScriptError(t *testing.T) {
	cfg := &Config{AllowScripts: true, AllowedScriptCommands: []string{"echo"}}
	if err := validateScript(cfg, "   "); err == nil {
		t.Error("expected error for empty script")
	}
}

func TestLoadConfig_Security_Defaults(t *testing.T) {
	p := writeTemp(t, "TELEGRAM_BOT_TOKEN=t\nALLOWED_USER_IDS=1\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowScripts {
		t.Error("AllowScripts default should be false")
	}
	if cfg.RateLimitPerMin != 20 {
		t.Errorf("RateLimitPerMin default = %d, want 20", cfg.RateLimitPerMin)
	}
	if len(cfg.AllowedScriptCommands) != 0 {
		t.Errorf("AllowedScriptCommands default should be empty")
	}
}

func TestLoadConfig_Security_Parse(t *testing.T) {
	p := writeTemp(t, "TELEGRAM_BOT_TOKEN=t\nALLOWED_USER_IDS=1\n"+
		"ALLOW_SCRIPTS=true\n"+
		"ALLOWED_SCRIPT_COMMANDS=echo, curl, git\n"+
		"RATE_LIMIT_PER_MIN=5\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowScripts {
		t.Error("AllowScripts should be true")
	}
	if cfg.RateLimitPerMin != 5 {
		t.Errorf("RateLimitPerMin = %d, want 5", cfg.RateLimitPerMin)
	}
	if len(cfg.AllowedScriptCommands) != 3 {
		t.Errorf("AllowedScriptCommands = %v, want 3 items", cfg.AllowedScriptCommands)
	}
}

func TestRateLimiter_Remaining_AfterSetLimit(t *testing.T) {
	r := NewRateLimiter(2)
	_ = r.Allow(9)
	if got := r.Remaining(9); got != 1 {
		t.Errorf("Remaining = %d, want 1", got)
	}
	r.SetLimit(0)
	if got := r.Remaining(9); got != -1 {
		t.Errorf("after SetLimit(0) Remaining = %d, want -1 (unlimited)", got)
	}
}
