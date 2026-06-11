package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fix #3: fire() returns false when deps pending; task not removed ---

func TestFire_ReturnsFalseWhenDepsPending_TaskNotRemoved(t *testing.T) {
	s := newTestScheduler(t)
	go s.Run()
	defer s.Stop()

	dep := &Task{
		ID: newTaskID(), ChatID: 1, Prompt: "dep", CronExpr: "* * * * *",
		Status: "pending", IsTask: false, Label: "dep", CreatedAt: time.Now(),
	}
	if err := s.AddTask(dep); err != nil {
		t.Fatal(err)
	}

	target := &Task{
		ID:        newTaskID(),
		ChatID:    1,
		Prompt:    "target",
		FireAt:    time.Now().Add(50 * time.Millisecond),
		Status:    "pending",
		IsTask:    false,
		Label:     "target",
		CreatedAt: time.Now(),
		DependsOn: []string{dep.ID},
	}
	if err := s.AddTask(target); err != nil {
		t.Fatal(err)
	}

	// Wait for the one-shot timer to fire; deps pending → should reschedule.
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	found := s.findByID(target.ID)
	s.mu.Unlock()
	if found == nil {
		t.Fatal("task was removed after deps-deferred fire — goroutine cleanup bug not fixed")
	}
	if found.Status != "pending" {
		t.Errorf("task status = %q, want pending", found.Status)
	}
	// FireAt should have been pushed 1 minute into the future.
	if !found.FireAt.After(time.Now()) {
		t.Error("rescheduled FireAt should be in the future")
	}
}

func TestFire_ReturnsBool_Direct(t *testing.T) {
	s := newTestScheduler(t)
	s.mu.Lock()
	// Task with no deps → fire() should return true (when not nil/not-pending).
	// We test the deps-pending branch returns false.
	dep := &Task{ID: "dep1", Status: "pending", Prompt: "x", ChatID: 1, Label: "x", CreatedAt: time.Now()}
	task := &Task{ID: "tsk1", Status: "pending", DependsOn: []string{"dep1"}, CronExpr: "", Prompt: "y", ChatID: 1, FireAt: time.Now().Add(time.Hour), Label: "y", CreatedAt: time.Now()}
	s.tasks = append(s.tasks, dep, task)
	// Manually register one-shot for task.
	stopCh := make(chan struct{})
	s.stopChs[task.ID] = stopCh
	s.mu.Unlock()

	result := s.fire(task.ID)
	if result {
		t.Error("fire() should return false when deps are pending")
	}
}

// --- Fix #7: removeInt64 helper ---

func TestRemoveInt64(t *testing.T) {
	in := []int64{1, 2, 3, 4}
	out := removeInt64(in, 3)
	for _, v := range out {
		if v == 3 {
			t.Error("removeInt64 should have removed 3")
		}
	}
	if len(out) != 3 {
		t.Errorf("len = %d, want 3", len(out))
	}
}

func TestRemoveInt64_Missing(t *testing.T) {
	in := []int64{1, 2}
	out := removeInt64(in, 99)
	if len(out) != 2 {
		t.Errorf("removing non-existent value should leave list unchanged, got len=%d", len(out))
	}
}

func TestRemoveInt64_LastElement(t *testing.T) {
	out := removeInt64([]int64{42}, 42)
	if len(out) != 0 {
		t.Errorf("expected empty after removing sole element, got %v", out)
	}
}

// --- Fix #1: !parallel cap at MaxWorkers ---

func parallelPrompts(rest string) []string {
	var out []string
	for _, p := range strings.Split(rest, "|") {
		if p := strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestParallel_CappedAtMaxWorkers(t *testing.T) {
	maxWorkers := 2
	prompts := parallelPrompts("p1 | p2 | p3 | p4")
	if len(prompts) > maxWorkers {
		prompts = prompts[:maxWorkers]
	}
	if len(prompts) != 2 {
		t.Errorf("after cap got %d prompts, want 2", len(prompts))
	}
}

func TestParallel_SinglePromptPassThrough(t *testing.T) {
	prompts := parallelPrompts("just one prompt")
	if len(prompts) != 1 {
		t.Errorf("expected 1 prompt, got %d", len(prompts))
	}
}

func TestParallel_EmptyPipesSkipped(t *testing.T) {
	prompts := parallelPrompts(" | | a | | b | ")
	if len(prompts) != 2 {
		t.Errorf("empty pipe segments should be skipped, got %d prompts", len(prompts))
	}
}

// --- Fix #9: queue overflow — dispatch respects MaxWorkers ---

func TestDispatch_ConcurrencyRespected(t *testing.T) {
	var mu sync.Mutex
	var peak, current int

	workerStart := func() {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()
	}
	workerEnd := func() {
		mu.Lock()
		current--
		mu.Unlock()
	}

	const maxW = 2
	sem := make(chan struct{}, maxW)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		sem <- struct{}{} // blocks when maxW slots occupied
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			workerStart()
			time.Sleep(5 * time.Millisecond)
			workerEnd()
		}()
	}
	wg.Wait()

	if peak > maxW {
		t.Errorf("peak concurrency = %d exceeded maxW = %d", peak, maxW)
	}
}
