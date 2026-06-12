package main

import (
	"testing"
)

// --- isContextOverflow ---

func TestIsContextOverflow_MatchesKnownPhrases(t *testing.T) {
	cases := []struct {
		text  string
		match bool
	}{
		{"prompt is too long for the model", true},
		{"context length exceeded the limit", true},
		{"context window is full", true},
		{"some unrelated error", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isContextOverflow(tc.text)
		if got != tc.match {
			t.Errorf("isContextOverflow(%q) = %v, want %v", tc.text, got, tc.match)
		}
	}
}

// --- sendChunked splits output longer than chunkSize ---

func TestSendChunked_LongMessage_SplitsCorrectly(t *testing.T) {
	var sent []string
	sender := &recordSender{sendFn: func(chatID int64, text string) error {
		sent = append(sent, text)
		return nil
	}}

	// 4100-byte message should split into 2 chunks.
	long := make([]byte, 4100)
	for i := range long {
		long[i] = 'a'
	}
	_ = sendChunked(sender, 1, string(long))

	if len(sent) < 2 {
		t.Errorf("expected at least 2 chunks for 4100-byte message, got %d", len(sent))
	}
	total := 0
	for _, chunk := range sent {
		total += len(chunk)
	}
	if total != 4100 {
		t.Errorf("total bytes in chunks = %d, want 4100", total)
	}
}

func TestSendChunked_ShortMessage_SingleSend(t *testing.T) {
	var sent []string
	sender := &recordSender{sendFn: func(_ int64, text string) error {
		sent = append(sent, text)
		return nil
	}}
	_ = sendChunked(sender, 1, "hello")
	if len(sent) != 1 {
		t.Errorf("expected 1 send for short message, got %d", len(sent))
	}
}

func TestSendChunked_EmptyMessage_SendsPlaceholder(t *testing.T) {
	var sent []string
	sender := &recordSender{sendFn: func(_ int64, text string) error {
		sent = append(sent, text)
		return nil
	}}
	_ = sendChunked(sender, 1, "")
	// sendChunked converts empty/whitespace-only input to "(빈 응답)" — one message sent.
	if len(sent) != 1 {
		t.Errorf("expected 1 send (placeholder) for empty message, got %d", len(sent))
	}
	if sent[0] != "(빈 응답)" {
		t.Errorf("expected placeholder text, got %q", sent[0])
	}
}

// recordSender is a minimal MessageSender for testing.
type recordSender struct {
	sendFn func(chatID int64, text string) error
}

func (r *recordSender) Send(chatID int64, text string) error { return r.sendFn(chatID, text) }
func (r *recordSender) Typing(_ int64)                       {}
