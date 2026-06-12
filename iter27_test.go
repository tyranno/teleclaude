package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// --- ParseSchedule ---

func TestParseSchedule_Aliases(t *testing.T) {
	cases := []struct {
		in      string
		wantDur time.Duration
	}{
		{"hourly", time.Hour},
		{"daily", 24 * time.Hour},
		{"weekly", 7 * 24 * time.Hour},
		{"매시간", time.Hour},
		{"매일", 24 * time.Hour},
		{"매주", 7 * 24 * time.Hour},
		// Case-insensitive
		{"HOURLY", time.Hour},
		{"Daily", 24 * time.Hour},
	}
	for _, tc := range cases {
		dur, _, err := ParseSchedule(tc.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q) error: %v", tc.in, err)
			continue
		}
		if dur != tc.wantDur {
			t.Errorf("ParseSchedule(%q) = %v, want %v", tc.in, dur, tc.wantDur)
		}
	}
}

func TestParseSchedule_Units(t *testing.T) {
	cases := []struct {
		in      string
		wantDur time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"3d", 3 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		dur, _, err := ParseSchedule(tc.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q) error: %v", tc.in, err)
			continue
		}
		if dur != tc.wantDur {
			t.Errorf("ParseSchedule(%q) = %v, want %v", tc.in, dur, tc.wantDur)
		}
	}
}

func TestParseSchedule_InvalidInputs(t *testing.T) {
	cases := []string{"0m", "abc", "30", "x", ""}
	for _, in := range cases {
		_, _, err := ParseSchedule(in)
		if err == nil {
			t.Errorf("ParseSchedule(%q) should return error", in)
		}
	}
}

// --- durationToCron ---

func TestDurationToCron_Boundaries(t *testing.T) {
	cases := []struct {
		dur  time.Duration
		want string
	}{
		{30 * time.Second, "* * * * *"},  // sub-minute
		{time.Minute, "* * * * *"},        // exactly 1m
		{30 * time.Minute, "*/30 * * * *"},
		{time.Hour, "0 * * * *"},
		{6 * time.Hour, "0 */6 * * *"},
		{24 * time.Hour, "0 0 * * *"},
		{7 * 24 * time.Hour, "0 0 * * 0"},
	}
	for _, tc := range cases {
		got := durationToCron(tc.dur)
		if got != tc.want {
			t.Errorf("durationToCron(%v) = %q, want %q", tc.dur, got, tc.want)
		}
	}
}

// --- parseOnceDatetime ---

func TestParseOnceDatetime_HHMMInFuture(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	token := future.Format("15:04")
	fireAt, consumed, err := parseOnceDatetime([]string{token, "message"})
	if err != nil {
		t.Fatalf("parseOnceDatetime(%q) error: %v", token, err)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d, want 1", consumed)
	}
	if fireAt.Hour() != future.Hour() || fireAt.Minute() != future.Minute() {
		t.Errorf("fireAt time mismatch: got %v, want hour=%d min=%d", fireAt, future.Hour(), future.Minute())
	}
}

func TestParseOnceDatetime_HHMMPastTimeAdvances24h(t *testing.T) {
	// A past time should be advanced to tomorrow.
	past := time.Now().Add(-2 * time.Hour)
	token := past.Format("15:04")
	fireAt, consumed, err := parseOnceDatetime([]string{token, "message"})
	if err != nil {
		t.Fatalf("parseOnceDatetime(%q) error: %v", token, err)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d, want 1", consumed)
	}
	if !fireAt.After(time.Now()) {
		t.Error("past HH:MM time should be advanced to tomorrow (after now)")
	}
}

func TestParseOnceDatetime_FullDatetime(t *testing.T) {
	future := time.Now().Add(25 * time.Hour).Truncate(time.Minute)
	dateTok := future.Format("2006-01-02")
	timeTok := future.Format("15:04")
	fireAt, consumed, err := parseOnceDatetime([]string{dateTok, timeTok, "msg"})
	if err != nil {
		t.Fatalf("parseOnceDatetime(%q %q) error: %v", dateTok, timeTok, err)
	}
	if consumed != 2 {
		t.Errorf("consumed = %d, want 2", consumed)
	}
	diff := fireAt.Sub(future)
	if diff < -time.Minute || diff > time.Minute {
		t.Errorf("fireAt %v ≠ expected %v", fireAt, future)
	}
}

func TestParseOnceDatetime_PastFullDatetime_Error(t *testing.T) {
	_, _, err := parseOnceDatetime([]string{"2000-01-01", "12:00", "msg"})
	if err == nil {
		t.Error("past YYYY-MM-DD HH:MM should return error")
	}
	if !strings.Contains(err.Error(), "과거") {
		t.Errorf("error should mention 과거 (past), got: %v", err)
	}
}

func TestParseOnceDatetime_InvalidFormat_Error(t *testing.T) {
	_, _, err := parseOnceDatetime([]string{"not-a-date"})
	if err == nil {
		t.Error("invalid token should return error")
	}
}

func TestParseOnceDatetime_EmptyTokens_Error(t *testing.T) {
	_, _, err := parseOnceDatetime([]string{})
	if err == nil {
		t.Error("empty token list should return error")
	}
}

// --- writeConfigFile ---

func TestWriteConfigFile_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.txt"
	if err := writeConfigFile(path, "testtoken:123", 42); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, "TELEGRAM_BOT_TOKEN=testtoken:123") {
		t.Errorf("expected token in config, got: %s", content)
	}
	if !strings.Contains(content, "ALLOWED_USER_IDS=42") {
		t.Errorf("expected userID in config, got: %s", content)
	}
}
