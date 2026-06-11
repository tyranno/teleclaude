package main

import (
	"path/filepath"
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

// newTestScheduler returns a scheduler wired to a temp tasks.json.
// The caller must start the cron runner if needed; for unit tests we usually skip it.
func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	s := NewScheduler(filepath.Join(t.TempDir(), "tasks.json"))
	// Start the cron runner so register() can actually add entries.
	go s.Run()
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestUpdateTask_InvalidCronRollback(t *testing.T) {
	s := newTestScheduler(t)

	task := &Task{
		ID:       newTaskID(),
		ChatID:   1,
		CronExpr: "* * * * *", // valid — every minute
		Prompt:   "original prompt",
		Status:   "pending",
		IsTask:   true,
	}
	if err := s.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Attempt to update with an invalid cron expression.
	err := s.UpdateTask(task.ID, "not-a-valid-cron", "", "")
	if err == nil {
		t.Fatal("UpdateTask with invalid cron: expected error, got nil")
	}

	// Task must have rolled back to original CronExpr.
	s.mu.Lock()
	found := s.findByID(task.ID)
	cronAfter := found.CronExpr
	statusAfter := found.Status
	_, stillRegistered := s.cronEntries[task.ID]
	s.mu.Unlock()

	if cronAfter != "* * * * *" {
		t.Errorf("CronExpr after failed update = %q, want %q", cronAfter, "* * * * *")
	}
	if statusAfter != "paused" {
		t.Errorf("Status after failed update = %q, want %q", statusAfter, "paused")
	}
	if stillRegistered {
		t.Error("task should not remain in cronEntries after failed update")
	}
}

func TestUpdateTask_ValidUpdate(t *testing.T) {
	s := newTestScheduler(t)

	task := &Task{
		ID:       newTaskID(),
		ChatID:   1,
		CronExpr: "* * * * *",
		Prompt:   "old prompt",
		Status:   "pending",
		IsTask:   true,
	}
	if err := s.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	if err := s.UpdateTask(task.ID, "0 * * * *", "new prompt", ""); err != nil {
		t.Fatalf("UpdateTask: unexpected error: %v", err)
	}

	s.mu.Lock()
	found := s.findByID(task.ID)
	cronAfter := found.CronExpr
	promptAfter := found.Prompt
	statusAfter := found.Status
	s.mu.Unlock()

	if cronAfter != "0 * * * *" {
		t.Errorf("CronExpr = %q, want %q", cronAfter, "0 * * * *")
	}
	if promptAfter != "new prompt" {
		t.Errorf("Prompt = %q, want %q", promptAfter, "new prompt")
	}
	if statusAfter != "pending" {
		t.Errorf("Status = %q, want %q", statusAfter, "pending")
	}
}

func TestResumeTask_Idempotent_NoCronLeak(t *testing.T) {
	// Resuming an already-pending task must not register a second cron entry.
	// The bug: register() writes cronEntries[id] = newEntryID, overwriting the old
	// EntryID without removing it from cronRunner — ghost entry fires indefinitely.
	s := newTestScheduler(t)

	task := &Task{
		ID:       newTaskID(),
		ChatID:   1,
		CronExpr: "* * * * *",
		Prompt:   "ping",
		Status:   "pending",
		IsTask:   true,
	}
	if err := s.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Capture entry count before the spurious resume.
	s.mu.Lock()
	entriesBefore := len(s.cronRunner.Entries())
	s.mu.Unlock()

	// Resume a task that is already pending — should be a no-op.
	if err := s.ResumeTask(task.ID); err != nil {
		t.Fatalf("ResumeTask on pending task: unexpected error: %v", err)
	}

	s.mu.Lock()
	entriesAfter := len(s.cronRunner.Entries())
	s.mu.Unlock()

	if entriesAfter != entriesBefore {
		t.Errorf("cron entries: before=%d after=%d; spurious resume must not add a ghost entry",
			entriesBefore, entriesAfter)
	}
}

func TestResumeTask_FromPaused(t *testing.T) {
	s := newTestScheduler(t)

	task := &Task{
		ID:       newTaskID(),
		ChatID:   1,
		CronExpr: "* * * * *",
		Prompt:   "ping",
		Status:   "pending",
		IsTask:   true,
	}
	if err := s.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := s.PauseTask(task.ID); err != nil {
		t.Fatalf("PauseTask: %v", err)
	}

	s.mu.Lock()
	entriesAfterPause := len(s.cronRunner.Entries())
	s.mu.Unlock()
	if entriesAfterPause != 0 {
		t.Errorf("after pause, cron entries = %d, want 0", entriesAfterPause)
	}

	if err := s.ResumeTask(task.ID); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}

	s.mu.Lock()
	t2 := s.findByID(task.ID)
	statusAfter := t2.Status
	entriesAfterResume := len(s.cronRunner.Entries())
	s.mu.Unlock()

	if statusAfter != "pending" {
		t.Errorf("status = %q, want %q", statusAfter, "pending")
	}
	if entriesAfterResume != 1 {
		t.Errorf("cron entries after resume = %d, want 1", entriesAfterResume)
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
