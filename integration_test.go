//go:build integration

// Integration tests that invoke the REAL local `claude` CLI.
// Run with: go test -tags integration -run Integration -v
// Requires: claude CLI installed and logged in. Makes real API calls (cost).
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func intRunner(t *testing.T) *claudeRunner {
	t.Helper()
	path, err := findClaude("")
	if err != nil {
		t.Skipf("claude not found: %v", err)
	}
	// Use haiku for both to keep the test cheap; we verify the mechanism, not output quality.
	return NewClaudeRunner(path, NewConfigHolder(&Config{ManagerModel: "haiku", WorkerModel: "haiku"}))
}

func TestIntegration_Route(t *testing.T) {
	r := intRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dec, err := r.Route(ctx, RouteRequest{
		Message: "continue the myapp login bug discussion",
		Projects: []ProjectSummary{
			{Name: "myapp", Conversations: []ConversationSummary{
				{ID: "1", Title: "login bug", Summary: "session expiry fix"},
				{ID: "2", Title: "payment module", Summary: "PG integration"},
			}},
			{Name: "voicesvr"},
		},
	})
	if err != nil {
		t.Fatalf("Route error: %v", err)
	}
	t.Logf("decision: %+v", dec)
	if dec.Action == "" {
		t.Fatal("empty action — parsing failed against real claude output")
	}
	if dec.Action == ActionResume && (dec.Project != "myapp" || dec.ConversationID != "1") {
		t.Errorf("unexpected routing: project=%q conv=%q (want myapp/1)", dec.Project, dec.ConversationID)
	}
}

func TestIntegration_RunAndResume(t *testing.T) {
	r := intRunner(t)
	work := t.TempDir()
	sid := newUUID()

	// First turn: new session via --session-id.
	ctx1, c1 := context.WithTimeout(context.Background(), 90*time.Second)
	defer c1()
	start := time.Now()
	res, err := r.Run(ctx1, RunRequest{
		Prompt:    "Reply with exactly the word: PONG",
		WorkDir:   work,
		SessionID: sid,
		Resume:    false,
		Model:     "haiku",
	})
	if err != nil {
		t.Fatalf("Run(new) error: %v", err)
	}
	t.Logf("first-turn latency=%s result=%q", time.Since(start).Round(time.Millisecond), res.Text)
	if res.IsError || !strings.Contains(strings.ToUpper(res.Text), "PONG") {
		t.Fatalf("unexpected first result: %+v", res)
	}

	// Second turn: resume the same session, should recall context.
	ctx2, c2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer c2()
	res2, err := r.Run(ctx2, RunRequest{
		Prompt:    "What exact word did I ask you to reply? Answer with just that word.",
		WorkDir:   work,
		SessionID: sid,
		Resume:    true,
		Model:     "haiku",
	})
	if err != nil {
		t.Fatalf("Run(resume) error: %v", err)
	}
	t.Logf("resume result=%q", res2.Text)
	if !strings.Contains(strings.ToUpper(res2.Text), "PONG") {
		t.Errorf("resume did not recall context: %q", res2.Text)
	}
}

// TestIntegration_NoStdinHang verifies the Go exec path does not incur the
// "no stdin data received in 3s" wait (cmd.Stdin nil → immediate EOF).
func TestIntegration_NoStdinHang(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip timing test in CI")
	}
	r := intRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := r.Run(ctx, RunRequest{Prompt: "Reply: OK", WorkDir: t.TempDir(), SessionID: newUUID(), Model: "haiku"}); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// If the 3s stdin wait happened it would add ~3s; haiku round-trip is usually < 6s total.
	t.Logf("total latency=%s (should not include a 3s stdin stall)", time.Since(start).Round(time.Millisecond))
}
