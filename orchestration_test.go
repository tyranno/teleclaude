package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- parseTaskAddArgs --depends-on ---

func TestParseTaskAddArgs_DependsOn(t *testing.T) {
	_, _, deps, _, _, err := parseTaskAddArgs(
		[]string{"1h", "--depends-on", "abc,def", "task", "follow-up"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deps) != 2 || deps[0] != "abc" || deps[1] != "def" {
		t.Errorf("dependsOn = %v, want [abc def]", deps)
	}
}

func TestParseDependsOn_Whitespace(t *testing.T) {
	deps := parseDependsOn(" abc , def , ghi ")
	if len(deps) != 3 {
		t.Errorf("parseDependsOn = %v, want 3 items", deps)
	}
}

func TestParseDependsOn_Empty(t *testing.T) {
	deps := parseDependsOn("")
	if len(deps) != 0 {
		t.Errorf("parseDependsOn(\"\") = %v, want empty", deps)
	}
}

// --- depsMetLocked ---

func newOrchTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	return NewScheduler(filepath.Join(t.TempDir(), "tasks.json"))
}

func TestDepsMetLocked_NoDeps(t *testing.T) {
	s := newOrchTestScheduler(t)
	task := &Task{ID: "t1", DependsOn: nil}
	s.tasks = append(s.tasks, task)
	s.mu.Lock()
	met := s.depsMetLocked(task)
	s.mu.Unlock()
	if !met {
		t.Error("task with no deps should be met immediately")
	}
}

func TestDepsMetLocked_DepCompleted(t *testing.T) {
	s := newOrchTestScheduler(t)
	// dep task absent from list → treated as completed
	task := &Task{ID: "t2", DependsOn: []string{"dep1"}}
	s.tasks = append(s.tasks, task)
	s.mu.Lock()
	met := s.depsMetLocked(task)
	s.mu.Unlock()
	if !met {
		t.Error("dep not in task list → should be considered done")
	}
}

func TestDepsMetLocked_DepStillPending(t *testing.T) {
	s := newOrchTestScheduler(t)
	dep := &Task{ID: "dep1", Status: "pending", CronExpr: "* * * * *", ChatID: 1, Prompt: "x", Label: "x", CreatedAt: time.Now()}
	task := &Task{ID: "t3", DependsOn: []string{"dep1"}}
	s.tasks = append(s.tasks, dep, task)
	s.mu.Lock()
	met := s.depsMetLocked(task)
	s.mu.Unlock()
	if met {
		t.Error("dep still pending → should not be met")
	}
}

func TestDepsMetLocked_DepCancelled(t *testing.T) {
	s := newOrchTestScheduler(t)
	dep := &Task{ID: "dep1", Status: "cancelled"}
	task := &Task{ID: "t4", DependsOn: []string{"dep1"}}
	s.tasks = append(s.tasks, dep, task)
	s.mu.Lock()
	met := s.depsMetLocked(task)
	s.mu.Unlock()
	if !met {
		t.Error("dep cancelled → should be met")
	}
}

// --- UpdateTask with DependsOn ---

func TestUpdateTask_DependsOn(t *testing.T) {
	s := newOrchTestScheduler(t)
	task := &Task{
		ID:        newTaskID(),
		ChatID:    1,
		Prompt:    "p",
		CronExpr:  "0 * * * *",
		Status:    "pending",
		IsTask:    false,
		Label:     "l",
		CreatedAt: time.Now(),
	}
	go s.Run()
	defer s.Stop()
	if err := s.AddTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(task.ID, "", "", "", []string{"dep-a", "dep-b"}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	s.mu.Lock()
	found := s.findByID(task.ID)
	s.mu.Unlock()
	if found == nil {
		t.Fatal("task not found")
	}
	if len(found.DependsOn) != 2 || found.DependsOn[0] != "dep-a" {
		t.Errorf("DependsOn = %v, want [dep-a dep-b]", found.DependsOn)
	}
}

// --- parseFlags4 ---

func TestParseFlags4_AllFlags(t *testing.T) {
	v1, v2, v3, v4 := parseFlags4(
		strings.Fields("--cron 0 9 * * 1-5 --prompt hello world --script echo ok --depends-on abc,def"),
		"--cron", "--prompt", "--script", "--depends-on",
	)
	if v1 != "0 9 * * 1-5" {
		t.Errorf("v1 = %q, want cron expr", v1)
	}
	if v2 != "hello world" {
		t.Errorf("v2 = %q, want 'hello world'", v2)
	}
	if v3 != "echo ok" {
		t.Errorf("v3 = %q, want 'echo ok'", v3)
	}
	if v4 != "abc,def" {
		t.Errorf("v4 = %q, want 'abc,def'", v4)
	}
}

func TestParseFlags4_MissingFlags(t *testing.T) {
	v1, v2, v3, v4 := parseFlags4(
		strings.Fields("--cron 30m"),
		"--cron", "--prompt", "--script", "--depends-on",
	)
	if v1 != "30m" {
		t.Errorf("v1 = %q, want '30m'", v1)
	}
	if v2 != "" || v3 != "" || v4 != "" {
		t.Errorf("unset flags should be empty: v2=%q v3=%q v4=%q", v2, v3, v4)
	}
}
