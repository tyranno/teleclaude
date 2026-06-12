package main

import (
	"testing"
	"time"
)

// --- !task update --depends-on none clears deps ---

func TestUpdateTask_DepsNone_Clears(t *testing.T) {
	s := newTestScheduler(t)
	task := &Task{
		ID:        newTaskID(),
		ChatID:    1,
		Prompt:    "original",
		CronExpr:  "@every 1h",
		Status:    "pending",
		IsTask:    false,
		Label:     "lbl",
		CreatedAt: time.Now(),
		DependsOn: []string{"dep-a", "dep-b"},
	}
	if err := s.AddTask(task); err != nil {
		t.Fatal(err)
	}

	// Pass empty slice to clear deps.
	if err := s.UpdateTask(task.ID, "", "", "", []string{}); err != nil {
		t.Fatalf("UpdateTask returned error: %v", err)
	}

	s.mu.Lock()
	found := s.findByID(task.ID)
	s.mu.Unlock()
	if found == nil {
		t.Fatal("task not found after UpdateTask")
	}
	if len(found.DependsOn) != 0 {
		t.Errorf("DependsOn should be empty after clear, got %v", found.DependsOn)
	}
}

// --- !task update --script clear removes script ---

func TestUpdateTask_ScriptClearSentinel(t *testing.T) {
	s := newTestScheduler(t)
	task := &Task{
		ID:        newTaskID(),
		ChatID:    1,
		Prompt:    "original",
		CronExpr:  "@every 1h",
		Script:    "echo test",
		Status:    "pending",
		IsTask:    false,
		Label:     "lbl",
		CreatedAt: time.Now(),
	}
	if err := s.AddTask(task); err != nil {
		t.Fatal(err)
	}

	// "\x00" is the clear-script sentinel from bot.go.
	if err := s.UpdateTask(task.ID, "", "", "\x00", nil); err != nil {
		t.Fatalf("UpdateTask returned error: %v", err)
	}

	s.mu.Lock()
	found := s.findByID(task.ID)
	s.mu.Unlock()
	if found == nil {
		t.Fatal("task not found after UpdateTask")
	}
	if found.Script != "" {
		t.Errorf("Script should be cleared, got %q", found.Script)
	}
}

// --- parseDependsOn "none" produces non-empty slice (handled in bot.go, not parseDependsOn) ---

func TestParseDependsOn_NoneIsNotSpecial(t *testing.T) {
	// parseDependsOn itself treats "none" as a literal ID.
	// The "none" → clear mapping is handled in bot.go before calling parseDependsOn.
	result := parseDependsOn("none")
	if len(result) != 1 || result[0] != "none" {
		t.Errorf("parseDependsOn(\"none\") = %v, want [none]", result)
	}
}

func TestParseDependsOn_EmptyStringReturnsNil(t *testing.T) {
	result := parseDependsOn("")
	if result != nil {
		t.Errorf("parseDependsOn(\"\") should return nil, got %v", result)
	}
}

func TestParseDependsOn_MultipleIDs(t *testing.T) {
	result := parseDependsOn("a, b , c")
	if len(result) != 3 {
		t.Fatalf("expected 3 IDs, got %d: %v", len(result), result)
	}
	for i, want := range []string{"a", "b", "c"} {
		if result[i] != want {
			t.Errorf("result[%d] = %q, want %q", i, result[i], want)
		}
	}
}

// --- UpdateTask nil dependsOn leaves deps unchanged ---

func TestUpdateTask_NilDepsNoChange(t *testing.T) {
	s := newTestScheduler(t)
	task := &Task{
		ID:        newTaskID(),
		ChatID:    1,
		Prompt:    "original",
		CronExpr:  "@every 1h",
		Status:    "pending",
		IsTask:    false,
		Label:     "lbl",
		CreatedAt: time.Now(),
		DependsOn: []string{"dep-x"},
	}
	if err := s.AddTask(task); err != nil {
		t.Fatal(err)
	}

	// Pass nil → no change to DependsOn.
	if err := s.UpdateTask(task.ID, "", "updated prompt", "", nil); err != nil {
		t.Fatalf("UpdateTask returned error: %v", err)
	}

	s.mu.Lock()
	found := s.findByID(task.ID)
	s.mu.Unlock()
	if found == nil {
		t.Fatal("task not found")
	}
	if len(found.DependsOn) != 1 || found.DependsOn[0] != "dep-x" {
		t.Errorf("DependsOn unexpectedly changed: %v", found.DependsOn)
	}
	if found.Prompt != "updated prompt" {
		t.Errorf("Prompt not updated: %q", found.Prompt)
	}
}

// --- UpdateTask empty string fields leave them unchanged ---

func TestUpdateTask_EmptyStringFieldsNoChange(t *testing.T) {
	s := newTestScheduler(t)
	task := &Task{
		ID:        newTaskID(),
		ChatID:    1,
		Prompt:    "keep me",
		CronExpr:  "@every 2h",
		Script:    "keep script",
		Status:    "pending",
		IsTask:    false,
		Label:     "lbl",
		CreatedAt: time.Now(),
	}
	if err := s.AddTask(task); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateTask(task.ID, "", "", "", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s.mu.Lock()
	found := s.findByID(task.ID)
	s.mu.Unlock()
	if found.Prompt != "keep me" {
		t.Errorf("Prompt changed unexpectedly: %q", found.Prompt)
	}
	if found.CronExpr != "@every 2h" {
		t.Errorf("CronExpr changed unexpectedly: %q", found.CronExpr)
	}
	if found.Script != "keep script" {
		t.Errorf("Script changed unexpectedly: %q", found.Script)
	}
}
