package main

import (
	"strings"
	"testing"
)

func TestChunkText_Short(t *testing.T) {
	got := chunkText("hello", 4096)
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("got %v", got)
	}
}

func TestChunkText_HardSplit(t *testing.T) {
	s := strings.Repeat("a", 5000) // no newlines → hard cut
	got := chunkText(s, 4096)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(got))
	}
	if len([]rune(got[0])) != 4096 {
		t.Errorf("first chunk len = %d, want 4096", len([]rune(got[0])))
	}
	if got[0]+got[1] != s {
		t.Error("chunks do not reassemble to original")
	}
}

func TestChunkText_NewlinePreference(t *testing.T) {
	// 3000 'a', newline, 3000 'b' → split should occur at the newline (> max/2).
	s := strings.Repeat("a", 3000) + "\n" + strings.Repeat("b", 3000)
	got := chunkText(s, 4096)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(got))
	}
	if !strings.HasSuffix(got[0], "\n") {
		t.Error("first chunk should end at the newline boundary")
	}
	if strings.Contains(got[1], "a") {
		t.Error("second chunk should only contain the 'b' run")
	}
}

func TestChunkText_Multibyte(t *testing.T) {
	s := strings.Repeat("가", 5000) // Korean runes; must not split mid-rune
	got := chunkText(s, 4096)
	joined := strings.Join(got, "")
	if joined != s {
		t.Error("multibyte chunks corrupted on reassembly")
	}
}

func TestRoutingHeader(t *testing.T) {
	if h := routingHeader("myapp", "로그인", false); !strings.Contains(h, "myapp") || !strings.Contains(h, "이어가기") {
		t.Errorf("header = %q", h)
	}
	if h := routingHeader("myapp", "로그인", true); !strings.Contains(h, "새 대화") {
		t.Errorf("header = %q", h)
	}
}

// fakeSender records sent messages for sendChunked tests.
type fakeSender struct{ sent []string }

func (f *fakeSender) Send(_ int64, text string) error { f.sent = append(f.sent, text); return nil }
func (f *fakeSender) Typing(_ int64)                  {}

func TestSendChunked_EmptyBecomesPlaceholder(t *testing.T) {
	f := &fakeSender{}
	if err := sendChunked(f, 1, "   "); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "(빈 응답)" {
		t.Errorf("sent = %v", f.sent)
	}
}

func TestSendChunked_Multi(t *testing.T) {
	f := &fakeSender{}
	s := strings.Repeat("a", 5000)
	if err := sendChunked(f, 1, s); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 {
		t.Errorf("expected 2 messages, got %d", len(f.sent))
	}
}

func TestParseFlags_MultiTokenValues(t *testing.T) {
	tokens := strings.Fields("--cron 0 9 * * 1-5 --prompt 안녕하세요 세상 --script echo ok")
	cron, prompt, script := parseFlags(tokens, "--cron", "--prompt", "--script")
	if cron != "0 9 * * 1-5" {
		t.Errorf("cron = %q, want %q", cron, "0 9 * * 1-5")
	}
	if prompt != "안녕하세요 세상" {
		t.Errorf("prompt = %q, want %q", prompt, "안녕하세요 세상")
	}
	if script != "echo ok" {
		t.Errorf("script = %q, want %q", script, "echo ok")
	}
}

func TestParseFlags_SingleToken(t *testing.T) {
	tokens := strings.Fields("--prompt hello")
	_, prompt, _ := parseFlags(tokens, "--cron", "--prompt", "--script")
	if prompt != "hello" {
		t.Errorf("prompt = %q, want %q", prompt, "hello")
	}
}

func TestParseFlags_Empty(t *testing.T) {
	cron, prompt, script := parseFlags(nil, "--cron", "--prompt", "--script")
	if cron != "" || prompt != "" || script != "" {
		t.Errorf("expected all empty, got cron=%q prompt=%q script=%q", cron, prompt, script)
	}
}

func TestParseFlags_MissingValue(t *testing.T) {
	// --cron at end with no value → should be ""
	tokens := strings.Fields("--cron")
	cron, _, _ := parseFlags(tokens, "--cron", "--prompt", "--script")
	if cron != "" {
		t.Errorf("cron = %q, want empty", cron)
	}
}
