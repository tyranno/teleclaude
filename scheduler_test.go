package main

import (
	"testing"
	"time"
)

func TestParseSchedule_English(t *testing.T) {
	cases := []struct {
		in      string
		wantDur time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"hourly", time.Hour},
		{"daily", 24 * time.Hour},
		{"weekly", 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		dur, _, err := ParseSchedule(tc.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if dur != tc.wantDur {
			t.Errorf("ParseSchedule(%q) = %v, want %v", tc.in, dur, tc.wantDur)
		}
	}
}

func TestParseSchedule_Korean(t *testing.T) {
	cases := []struct {
		in      string
		wantDur time.Duration
	}{
		{"매시간", time.Hour},
		{"매일", 24 * time.Hour},
		{"매주", 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		dur, _, err := ParseSchedule(tc.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if dur != tc.wantDur {
			t.Errorf("ParseSchedule(%q) = %v, want %v", tc.in, dur, tc.wantDur)
		}
	}
}

func TestParseSchedule_Invalid(t *testing.T) {
	invalid := []string{"", "abc", "0m", "-1h", "5x", "매달"}
	for _, tc := range invalid {
		if _, _, err := ParseSchedule(tc); err == nil {
			t.Errorf("ParseSchedule(%q): expected error, got nil", tc)
		}
	}
}

func TestDetectBackendSwitchIntent(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		// Should detect
		{"codex로 전환해줘", "codex"},
		{"claude로 바꿔줘", "claude"},
		{"codex backend로 switch해줘", "codex"},
		{"codex 써줘", "codex"},
		{"claude 사용해", "claude"},
		// Should NOT detect (no switch verb)
		{"codex 프로젝트 백엔드 코드 작성해줘", ""},
		{"voice-chat-server의 backend api에 codex 주석 추가", ""},
		{"이 코드 claude api로 작성되어 있어", ""},
		// Neither codex nor claude mentioned
		{"전환해줘", ""},
	}
	for _, tc := range cases {
		got := detectBackendSwitchIntent(tc.text)
		if got != tc.want {
			t.Errorf("detectBackendSwitchIntent(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestDurationToCron(t *testing.T) {
	cases := []struct {
		dur  time.Duration
		want string
	}{
		{time.Minute, "* * * * *"},
		{30 * time.Minute, "*/30 * * * *"},
		{time.Hour, "0 * * * *"},
		{2 * time.Hour, "0 */2 * * *"},
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
