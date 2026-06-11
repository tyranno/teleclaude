package main

import (
	"strings"
	"testing"
	"time"
)

// --- parseOnceDatetime ---

func TestParseOnceDatetime_HHMM_Future(t *testing.T) {
	// Build a time in the future today by using "23:59" which is almost always in the future.
	// If we're past 23:59 it still works because it advances to tomorrow.
	tokens := []string{"23:59", "알림 텍스트"}
	fireAt, consumed, err := parseOnceDatetime(tokens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d, want 1", consumed)
	}
	if fireAt.IsZero() {
		t.Error("fireAt should not be zero")
	}
	// fireAt should be >= now (advanced to tomorrow if already past)
	if fireAt.Before(time.Now()) {
		t.Errorf("fireAt %v is before now", fireAt)
	}
}

func TestParseOnceDatetime_HHMM_PastAutoAdvances(t *testing.T) {
	// "00:01" is almost always in the past today; should advance to tomorrow.
	tokens := []string{"00:01"}
	fireAt, consumed, err := parseOnceDatetime(tokens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d, want 1", consumed)
	}
	// Must be in the future
	if !fireAt.After(time.Now()) {
		t.Errorf("fireAt %v should be in the future", fireAt)
	}
}

func TestParseOnceDatetime_FullDate_Future(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	dateStr := future.Format("2006-01-02")
	tokens := []string{dateStr, "14:30", "메시지"}
	fireAt, consumed, err := parseOnceDatetime(tokens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consumed != 2 {
		t.Errorf("consumed = %d, want 2", consumed)
	}
	if fireAt.Hour() != 14 || fireAt.Minute() != 30 {
		t.Errorf("time = %v, want 14:30", fireAt)
	}
}

func TestParseOnceDatetime_FullDate_Past_Error(t *testing.T) {
	past := time.Now().Add(-48 * time.Hour)
	tokens := []string{past.Format("2006-01-02"), "10:00"}
	_, _, err := parseOnceDatetime(tokens)
	if err == nil {
		t.Error("expected error for past full datetime")
	}
	if !strings.Contains(err.Error(), "과거") {
		t.Errorf("error = %q, want 과거 message", err.Error())
	}
}

func TestParseOnceDatetime_InvalidFormat(t *testing.T) {
	cases := [][]string{
		{},
		{"25:00"},
		{"not-a-date", "10:00"},
		{"2026-13-01", "10:00"},
	}
	for _, tc := range cases {
		if _, _, err := parseOnceDatetime(tc); err == nil {
			t.Errorf("parseOnceDatetime(%v): expected error", tc)
		}
	}
}

// --- allCronFields ---

func TestAllCronFields_Valid(t *testing.T) {
	cases := [][]string{
		{"*", "*", "*", "*", "*"},
		{"0", "9", "*", "*", "1-5"},
		{"*/30", "0", "*", "*", "0"},
		{"0", "0", "1", "1", "*"},
	}
	for _, tokens := range cases {
		if !allCronFields(tokens) {
			t.Errorf("allCronFields(%v) = false, want true", tokens)
		}
	}
}

func TestAllCronFields_Invalid(t *testing.T) {
	cases := [][]string{
		{"*", "*", "*", "*"},          // only 4 fields
		{},                            // empty
		{"abc", "*", "*", "*", "*"},   // invalid char
		{"*", "*", "*", "*", "*", "*"}, // 6 fields (still returns true because checks first 5)
		{"", "*", "*", "*", "*"},      // empty field
	}
	want := []bool{false, false, false, true, false}
	for i, tokens := range cases {
		got := allCronFields(tokens)
		if got != want[i] {
			t.Errorf("allCronFields(%v) = %v, want %v", tokens, got, want[i])
		}
	}
}

// --- isCronExpr ---

func TestIsCronExpr(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"0 9 * * 1-5", true},
		{"*/30 * * * *", true},
		{"@hourly", true},
		{"@every 30m", true},
		{"30m", false},
		{"hourly", false},
		{"매일", false},
		{"0 9 *", false},
	}
	for _, tc := range cases {
		got := isCronExpr(tc.s)
		if got != tc.want {
			t.Errorf("isCronExpr(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// --- buildContextPrompt ---

func TestBuildContextPrompt_NoContext(t *testing.T) {
	// When no context available, returns the prompt as-is.
	prompt := "코드 작성해줘"
	got := buildContextPrompt(prompt, "", "", "", nil)
	if got != prompt {
		t.Errorf("got %q, want %q", got, prompt)
	}
}

func TestBuildContextPrompt_WithGlobalMemory(t *testing.T) {
	got := buildContextPrompt("현재 작업", "", "global mem content", "", nil)
	if !strings.Contains(got, "global mem content") {
		t.Errorf("global memory missing from prompt")
	}
	if !strings.Contains(got, "장기 기억") {
		t.Errorf("global memory header missing")
	}
	if !strings.Contains(got, "현재 작업") {
		t.Errorf("current prompt missing")
	}
}

func TestBuildContextPrompt_WithHistory(t *testing.T) {
	history := []ConversationTurn{
		{Timestamp: time.Now(), Prompt: "이전 질문", Response: "이전 응답"},
	}
	got := buildContextPrompt("새 질문", "", "", "", history)
	if !strings.Contains(got, "이전 질문") {
		t.Errorf("history prompt missing")
	}
	if !strings.Contains(got, "새 질문") {
		t.Errorf("current prompt missing")
	}
}

func TestBuildContextPrompt_WithParentSummary(t *testing.T) {
	got := buildContextPrompt("질문", "부모 요약 내용", "", "", nil)
	if !strings.Contains(got, "부모 요약 내용") {
		t.Errorf("parent summary missing")
	}
	if !strings.Contains(got, "이전 대화 요약") {
		t.Errorf("parent summary header missing")
	}
}

func TestBuildContextPrompt_MemoryNote(t *testing.T) {
	got := buildContextPrompt("작업", "", "mem", "", nil)
	if !strings.Contains(got, ".teleclaude/memory.md") {
		t.Errorf("memory reminder missing from prompt")
	}
}
