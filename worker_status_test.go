package main

import (
	"testing"
	"time"
)

func TestWorkerStatusStore_SetAndGet(t *testing.T) {
	store := NewMemoryWorkerStatusStore()
	status := WorkerStatus{
		Project:        "myapp",
		ConversationID: "1",
		Title:          "로그인 버그",
		Status:         "running",
		StartTime:      time.Now(),
	}

	if err := store.SetStatus(status); err != nil {
		t.Fatal(err)
	}

	retrieved, ok := store.GetStatus("myapp", "1")
	if !ok {
		t.Fatal("status not found")
	}
	if retrieved.Title != "로그인 버그" {
		t.Errorf("title = %q, want %q", retrieved.Title, "로그인 버그")
	}
	if retrieved.Status != "running" {
		t.Errorf("status = %q, want %q", retrieved.Status, "running")
	}
}

func TestWorkerStatusStore_ListActive(t *testing.T) {
	store := NewMemoryWorkerStatusStore()

	// Add 3 running workers
	for i := 1; i <= 3; i++ {
		status := WorkerStatus{
			Project:        "myapp",
			ConversationID: string(rune('0' + i)),
			Title:          "Task " + string(rune('0'+i)),
			Status:         "running",
			StartTime:      time.Now(),
		}
		_ = store.SetStatus(status)
	}

	active := store.ListActive()
	if len(active) != 3 {
		t.Fatalf("expected 3 active, got %d", len(active))
	}
}

func TestWorkerStatusStore_UpdateAndArchive(t *testing.T) {
	store := NewMemoryWorkerStatusStore()

	status := WorkerStatus{
		Project:        "myapp",
		ConversationID: "1",
		Title:          "작업",
		Status:         "running",
		StartTime:      time.Now(),
	}
	_ = store.SetStatus(status)

	// Mark as completed
	_ = store.UpdateStatus("myapp", "1", "completed", "")

	// Should not be in active list
	active := store.ListActive()
	if len(active) != 0 {
		t.Fatalf("expected 0 active after completion, got %d", len(active))
	}

	// Should be in recent list
	recent := store.ListRecent(10)
	if len(recent) != 1 || recent[0].Status != "completed" {
		t.Errorf("completion not archived properly")
	}
}

func TestManagerWorkerStatus(t *testing.T) {
	fc := &fakeClaude{runRes: RunResult{Text: "작업 완료"}}
	m, st, _ := mgrFixture(t, fc)

	c, _ := st.NewConversation("myapp", "테스트 작업")
	c.Started = true // Set to started so Handle uses this conversation
	_ = st.UpdateConversation("myapp", c)

	fc.decision = RouteDecision{Action: ActionResume, Project: "myapp", ConversationID: c.ID}

	// Manually test status setting (simulating what runWorker does)
	status := WorkerStatus{
		Project:        "myapp",
		ConversationID: c.ID,
		Title:          c.Title,
		Status:         "running",
		StartTime:      time.Now(),
	}
	_ = m.workerStatus.SetStatus(status)

	// Update to completed
	_ = m.workerStatus.UpdateStatus("myapp", c.ID, "completed", "")

	// Verify it was recorded
	retrieved, ok := m.GetWorkerStatus("myapp", c.ID)
	if !ok {
		t.Fatal("worker status not found after update")
	}
	if retrieved.Status != "completed" {
		t.Errorf("status = %q, want completed", retrieved.Status)
	}
}

func TestManagerDescribeActiveWorkers(t *testing.T) {
	store := NewMemoryWorkerStatusStore()
	cfg := &Config{ManagerAlways: true}
	m := &Manager{
		client:       &fakeClaude{},
		store:        NewFileStore("/tmp/test.json"),
		workerStatus: store,
		cfg:          cfg,
	}

	// No active workers
	desc := m.DescribeActiveWorkers()
	if desc != "실행 중인 작업 없음" {
		t.Errorf("empty list should say no jobs")
	}

	// Add one active worker
	_ = store.SetStatus(WorkerStatus{
		Project:        "myapp",
		ConversationID: "1",
		Title:          "분석",
		Status:         "running",
		StartTime:      time.Now().Add(-30 * time.Second),
	})

	desc = m.DescribeActiveWorkers()
	if !contains(desc, "실행 중인 작업 (1개)") {
		t.Errorf("should list 1 active job, got: %q", desc)
	}
	if !contains(desc, "분석") {
		t.Errorf("should show task title")
	}
}
