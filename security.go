package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// --- Rate Limiter ---

// RateLimiter implements a per-user sliding-window rate limiter.
// Counts events in a rolling 1-minute window.
type RateLimiter struct {
	mu        sync.Mutex
	windows   map[int64][]time.Time
	maxPerMin int
}

func NewRateLimiter(maxPerMin int) *RateLimiter {
	return &RateLimiter{
		windows:   make(map[int64][]time.Time),
		maxPerMin: maxPerMin,
	}
}

// SetLimit updates the per-minute cap live (0 = unlimited).
func (r *RateLimiter) SetLimit(maxPerMin int) {
	r.mu.Lock()
	r.maxPerMin = maxPerMin
	r.mu.Unlock()
}

// Allow returns true and records the event if the user is within the rate limit.
// Returns false without recording if the limit would be exceeded.
// A maxPerMin of 0 means unlimited.
func (r *RateLimiter) Allow(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxPerMin <= 0 {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)

	raw := r.windows[userID]
	// Compact: keep only events within the sliding window.
	valid := raw[:0]
	for _, t := range raw {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= r.maxPerMin {
		r.windows[userID] = valid
		return false
	}
	r.windows[userID] = append(valid, now)
	return true
}

// Remaining returns how many more events the user can make in the current window.
// Returns -1 when unlimited (maxPerMin == 0).
func (r *RateLimiter) Remaining(userID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxPerMin <= 0 {
		return -1
	}
	cutoff := time.Now().Add(-time.Minute)
	count := 0
	for _, t := range r.windows[userID] {
		if t.After(cutoff) {
			count++
		}
	}
	if rem := r.maxPerMin - count; rem > 0 {
		return rem
	}
	return 0
}

// --- Script Validation ---

// validateScript checks whether a pre-check script is allowed by the config:
//   - ALLOW_SCRIPTS must be true (default false).
//   - If ALLOWED_SCRIPT_COMMANDS is non-empty, the first token of the script
//     must appear in the whitelist.
func validateScript(cfg *Config, script string) error {
	if !cfg.AllowScripts {
		return fmt.Errorf("스크립트 실행이 비활성화되어 있습니다. config.txt에 ALLOW_SCRIPTS=true 추가 필요")
	}
	if len(cfg.AllowedScriptCommands) == 0 {
		return nil // whitelist empty → allow any command
	}
	fields := strings.Fields(script)
	if len(fields) == 0 {
		return fmt.Errorf("스크립트가 비어 있습니다")
	}
	cmd := fields[0]
	for _, allowed := range cfg.AllowedScriptCommands {
		if cmd == allowed {
			return nil
		}
	}
	return fmt.Errorf("허용되지 않은 스크립트 명령: %q (허용 목록: %s)",
		cmd, strings.Join(cfg.AllowedScriptCommands, ", "))
}
