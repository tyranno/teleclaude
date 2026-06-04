package main

import (
	"crypto/rand"
	"fmt"
)

// newUUID returns a random RFC-4122 v4 UUID (no external dependency).
// Design Ref: §4.2 — used as claude --session-id per conversation.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is catastrophic; fall back to a fixed-shape value.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// estimateTokens returns a rough token count estimate (word-based heuristic).
// English: ~1.3 tokens/word; Korean: ~1.5 tokens/word. Conservative estimate.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Simple heuristic: count whitespace-separated tokens, multiply by 1.4
	tokens := 0
	inWord := false
	for _, ch := range text {
		isWordChar := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch > 127 // includes CJK
		if isWordChar && !inWord {
			tokens++
			inWord = true
		} else if !isWordChar {
			inWord = false
		}
	}
	// Apply multiplier for subword tokenization
	return int(float64(tokens) * 1.4)
}
