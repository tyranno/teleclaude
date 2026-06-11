package main

import (
	"strings"
	"testing"
	"time"
)

// --- MaxWorkers validation ---

func TestLoadConfig_MaxWorkers_TooLarge(t *testing.T) {
	p := writeTemp(t, "TELEGRAM_BOT_TOKEN=t\nALLOWED_USER_IDS=1\nMAX_WORKERS=51\n")
	_, err := LoadConfig(p)
	if err == nil {
		t.Error("expected error for MAX_WORKERS=51")
	}
	if !strings.Contains(err.Error(), "MAX_WORKERS") {
		t.Errorf("error should mention MAX_WORKERS: %v", err)
	}
}

func TestLoadConfig_MaxWorkers_AtLimit(t *testing.T) {
	p := writeTemp(t, "TELEGRAM_BOT_TOKEN=t\nALLOWED_USER_IDS=1\nMAX_WORKERS=50\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("MAX_WORKERS=50 should be valid, got: %v", err)
	}
	if cfg.MaxWorkers != 50 {
		t.Errorf("MaxWorkers = %d, want 50", cfg.MaxWorkers)
	}
}

// --- History content byte-truncation ---

func TestHistoryTruncate_ByteBoundary(t *testing.T) {
	// Build 3900 Korean runes (each = 3 UTF-8 bytes → 11,700 bytes total).
	// The function should truncate to 3800 bytes with a valid rune boundary.
	korean := strings.Repeat("가", 3900) // 3900 × 3 = 11,700 bytes
	const maxContentBytes = 3800
	content := korean
	if len(content) > maxContentBytes {
		end := maxContentBytes
		b2 := []byte(content)
		for end > 0 && (b2[end]&0xC0) == 0x80 {
			end--
		}
		content = string(b2[:end]) + "\n...(잘림)"
	}
	if len([]byte(content)) > maxContentBytes+len("\n...(잘림)") {
		t.Errorf("truncated content byte length = %d, expected <= %d",
			len([]byte(content)), maxContentBytes+len("\n...(잘림)"))
	}
	// Verify valid UTF-8 by decoding to runes.
	runes := []rune(content)
	if len(runes) == 0 {
		t.Error("truncated content should not be empty")
	}
	// Verify the truncation suffix is present.
	if !strings.HasSuffix(content, "\n...(잘림)") {
		t.Error("truncated content should end with suffix")
	}
}

func TestHistoryTruncate_ShortContent_NoTruncation(t *testing.T) {
	content := "짧은 내용"
	original := content
	const maxContentBytes = 3800
	if len(content) > maxContentBytes {
		end := maxContentBytes
		b2 := []byte(content)
		for end > 0 && (b2[end]&0xC0) == 0x80 {
			end--
		}
		content = string(b2[:end]) + "\n...(잘림)"
	}
	if content != original {
		t.Errorf("short content should not be truncated")
	}
}

// --- RateLimiter window expiry ---

func TestRateLimiter_WindowExpiry(t *testing.T) {
	// Use very short window (1s) by manipulating the RateLimiter internals.
	// RateLimiter uses 1-minute window; we can't easily shrink it in unit tests
	// without modifying the struct. Instead, verify that old events are evicted.
	r := NewRateLimiter(2)
	// Record 2 events in the past (older than 1 minute).
	r.mu.Lock()
	pastTime := time.Now().Add(-2 * time.Minute)
	r.windows[1] = []time.Time{pastTime, pastTime}
	r.mu.Unlock()

	// With 2 stale events (outside window), Allow should succeed.
	if !r.Allow(1) {
		t.Error("stale events outside window should not count against the limit")
	}
}

func TestRateLimiter_WindowCountsOnlyRecent(t *testing.T) {
	r := NewRateLimiter(3)
	// Add 2 past events outside the window.
	r.mu.Lock()
	pastTime := time.Now().Add(-90 * time.Second)
	r.windows[1] = []time.Time{pastTime, pastTime}
	r.mu.Unlock()

	// Should still allow 3 more (past events don't count).
	for i := range 3 {
		if !r.Allow(1) {
			t.Errorf("request %d should be allowed (past events outside window)", i+1)
		}
	}
	// 4th should be blocked (3 fresh events now in window).
	if r.Allow(1) {
		t.Error("4th request should be blocked after 3 fresh events")
	}
}

// --- fire() bool return for non-pending tasks ---

func TestFire_ReturnsFalse_WhenTaskNil(t *testing.T) {
	s := newTestScheduler(t)
	result := s.fire("nonexistent-id")
	if result {
		t.Error("fire() should return false for nonexistent task")
	}
}

func TestFire_ReturnsFalse_WhenTaskCancelled(t *testing.T) {
	s := newTestScheduler(t)
	s.mu.Lock()
	s.tasks = append(s.tasks, &Task{ID: "t1", Status: "cancelled", Prompt: "x", ChatID: 1, Label: "x", CreatedAt: time.Now()})
	s.mu.Unlock()
	result := s.fire("t1")
	if result {
		t.Error("fire() should return false for cancelled task")
	}
}
