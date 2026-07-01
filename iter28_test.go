package main

import (
	"strings"
	"testing"
	"time"
)

// ---- buildContextPrompt (novel cases not in bot_test.go) ----

func TestBuildContextPrompt_NoContext_ReturnsPromptOnly(t *testing.T) {
	got := buildContextPrompt("hello", "", "", "", nil)
	if got != "hello" {
		t.Errorf("no-context prompt = %q, want %q", got, "hello")
	}
}

func TestBuildContextPrompt_LongResponseTruncated(t *testing.T) {
	longResp := strings.Repeat("x", 400)
	hist := []ConversationTurn{{Prompt: "q", Response: longResp}}
	got := buildContextPrompt("req", "", "", "", hist)
	// History responses are truncated to 300 chars; full 400-char response must not appear.
	if strings.Contains(got, longResp) {
		t.Error("long response should be truncated in context prompt")
	}
}

// ---- formatCompletion ----

func TestFormatCompletion_SecondsOnly(t *testing.T) {
	got := formatCompletion(45 * time.Second)
	if !strings.Contains(got, "45초") {
		t.Errorf("formatCompletion(45s) = %q, want '45초'", got)
	}
	if strings.Contains(got, "분") {
		t.Errorf("formatCompletion(45s) should not contain '분', got %q", got)
	}
}

func TestFormatCompletion_MinutesAndSeconds(t *testing.T) {
	got := formatCompletion(2*time.Minute + 30*time.Second)
	if !strings.Contains(got, "2분") {
		t.Errorf("formatCompletion(2m30s) = %q, want '2분'", got)
	}
	if !strings.Contains(got, "30초") {
		t.Errorf("formatCompletion(2m30s) = %q, want '30초'", got)
	}
}

// ---- allCronFields (novel: letters, too few, empty token) ----

func TestAllCronFields_LettersInToken(t *testing.T) {
	if allCronFields([]string{"0", "9", "*", "*", "MON"}) {
		t.Error("alpha token should return false")
	}
}

func TestAllCronFields_TooFew(t *testing.T) {
	if allCronFields([]string{"*", "*", "*", "*"}) {
		t.Error("fewer than 5 tokens should return false")
	}
}

func TestAllCronFields_EmptyToken(t *testing.T) {
	if allCronFields([]string{"*", "*", "*", "*", ""}) {
		t.Error("empty token should return false")
	}
}

// ---- parseFlags4 ----

func TestParseFlags4_MultiWordValue(t *testing.T) {
	v1, _, _, _ := parseFlags4(
		[]string{"--cron", "0", "9", "*", "*", "1-5"},
		"--cron", "--prompt", "--script", "--depends-on",
	)
	if v1 != "0 9 * * 1-5" {
		t.Errorf("parseFlags4 multi-word --cron = %q, want %q", v1, "0 9 * * 1-5")
	}
}

func TestParseFlags4_TwoFlags(t *testing.T) {
	_, v2, _, _ := parseFlags4(
		[]string{"--cron", "*/5 * * * *", "--prompt", "hello world"},
		"--cron", "--prompt", "--script", "--depends-on",
	)
	if v2 != "hello world" {
		t.Errorf("parseFlags4 --prompt = %q, want %q", v2, "hello world")
	}
}

func TestParseFlags4_FlagWithNoValue(t *testing.T) {
	// Flag at end with no following tokens → value stays empty.
	v1, _, _, _ := parseFlags4([]string{"--cron"}, "--cron", "--prompt", "--script", "--depends-on")
	if v1 != "" {
		t.Errorf("flag with no value should be empty, got %q", v1)
	}
}

// ---- parseDependsOn ----

func TestParseDependsOn_CommaSeparated(t *testing.T) {
	got := parseDependsOn("abc,def,ghi")
	if len(got) != 3 || got[0] != "abc" || got[1] != "def" || got[2] != "ghi" {
		t.Errorf("parseDependsOn = %v, want [abc def ghi]", got)
	}
}

func TestParseDependsOn_SpacesAround(t *testing.T) {
	got := parseDependsOn(" abc , def ")
	if len(got) != 2 || got[0] != "abc" || got[1] != "def" {
		t.Errorf("parseDependsOn with spaces = %v, want [abc def]", got)
	}
}

func TestParseDependsOn_EmptyString(t *testing.T) {
	got := parseDependsOn("")
	if len(got) != 0 {
		t.Errorf("parseDependsOn(\"\") = %v, want empty", got)
	}
}

func TestParseDependsOn_TrailingComma(t *testing.T) {
	got := parseDependsOn("a,b,")
	if len(got) != 2 {
		t.Errorf("parseDependsOn trailing comma = %v, want 2 items", got)
	}
}

// ---- extFromMIME ----

func TestExtFromMIME_Known(t *testing.T) {
	cases := []struct{ mime, ext string }{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"application/pdf", ".pdf"},
		{"text/plain", ".txt"},
		{"application/zip", ".zip"},
	}
	for _, tc := range cases {
		got := extFromMIME(tc.mime)
		if got != tc.ext {
			t.Errorf("extFromMIME(%q) = %q, want %q", tc.mime, got, tc.ext)
		}
	}
}

func TestExtFromMIME_Unknown(t *testing.T) {
	got := extFromMIME("application/octet-stream")
	if got != ".bin" {
		t.Errorf("extFromMIME(unknown) = %q, want .bin", got)
	}
}

func TestExtFromMIME_CaseInsensitive(t *testing.T) {
	got := extFromMIME("IMAGE/JPEG")
	if got != ".jpg" {
		t.Errorf("extFromMIME(IMAGE/JPEG) = %q, want .jpg", got)
	}
}

// ---- removeInt64 ----

func TestRemoveInt64_Present(t *testing.T) {
	got := removeInt64([]int64{1, 2, 3}, 2)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("removeInt64 = %v, want [1 3]", got)
	}
}

func TestRemoveInt64_NotPresent(t *testing.T) {
	got := removeInt64([]int64{1, 3}, 99)
	if len(got) != 2 {
		t.Errorf("removeInt64(not present) = %v, want [1 3]", got)
	}
}

func TestRemoveInt64_EmptySlice(t *testing.T) {
	got := removeInt64(nil, 42)
	if len(got) != 0 {
		t.Errorf("removeInt64(nil) should return empty, got %v", got)
	}
}

// ---- detectBackendSwitchIntent ----

func TestDetectBackendSwitchIntent_Codex(t *testing.T) {
	got := detectBackendSwitchIntent("codex로 전환해줘")
	if got != "codex" {
		t.Errorf("detectBackendSwitchIntent = %q, want codex", got)
	}
}

func TestDetectBackendSwitchIntent_Claude(t *testing.T) {
	got := detectBackendSwitchIntent("claude로 바꿔줘")
	if got != "claude" {
		t.Errorf("detectBackendSwitchIntent = %q, want claude", got)
	}
}

func TestDetectBackendSwitchIntent_NoVerb(t *testing.T) {
	// Mentioning codex without a switch verb → no switch intent.
	got := detectBackendSwitchIntent("codex가 뭐야?")
	if got != "" {
		t.Errorf("detectBackendSwitchIntent(no verb) = %q, want empty", got)
	}
}

func TestDetectBackendSwitchIntent_VerbWithoutBackend(t *testing.T) {
	// Has a verb but no backend name → no intent.
	got := detectBackendSwitchIntent("뭔가를 전환해줘")
	if got != "" {
		t.Errorf("detectBackendSwitchIntent(verb only) = %q, want empty", got)
	}
}

// ---- isContextOverflow ----

func TestIsContextOverflow_PromptTooLong(t *testing.T) {
	if !isContextOverflow("Error: prompt is too long for this model") {
		t.Error("'prompt is too long' should match")
	}
}

func TestIsContextOverflow_ContextLengthExceeded(t *testing.T) {
	if !isContextOverflow("Context length exceeded maximum") {
		t.Error("'context length exceeded' should match")
	}
}

func TestIsContextOverflow_ContextWindow(t *testing.T) {
	if !isContextOverflow("This request exceeds the context window") {
		t.Error("'context window' should match")
	}
}

func TestIsContextOverflow_UnrelatedError(t *testing.T) {
	if isContextOverflow("connection refused") {
		t.Error("unrelated error should not match")
	}
}

// ---- isSessionNotFound ----

func TestIsSessionNotFound_NoConversation(t *testing.T) {
	if !isSessionNotFound("Error: No conversation found with session ID: abc-123") {
		t.Error("'No conversation found' should match")
	}
}

func TestIsSessionNotFound_SessionNotFound(t *testing.T) {
	if !isSessionNotFound("session not found") {
		t.Error("'session not found' should match")
	}
}

func TestIsSessionNotFound_Unrelated(t *testing.T) {
	if isSessionNotFound("prompt is too long") {
		t.Error("unrelated (overflow) error should not match")
	}
	if isSessionNotFound("connection refused") {
		t.Error("unrelated error should not match")
	}
}

func TestIsContextOverflow_CaseInsensitive(t *testing.T) {
	if !isContextOverflow("PROMPT IS TOO LONG") {
		t.Error("case-insensitive match should work")
	}
}
