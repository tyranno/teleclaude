package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// fakeClaude is a programmable ClaudeClient for manager tests.
type fakeClaude struct {
	decision RouteDecision
	routeErr error
	runRes   RunResult
	runErr   error
	lastRun  RunRequest
	runCalls int
}

func (f *fakeClaude) Route(_ context.Context, _ RouteRequest) (RouteDecision, error) {
	return f.decision, f.routeErr
}
func (f *fakeClaude) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.lastRun = req
	f.runCalls++
	return f.runRes, f.runErr
}

func mgrFixture(t *testing.T, fc *fakeClaude) (*Manager, *fileStore, string) {
	t.Helper()
	st := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := st.AddProject("myapp", dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ManagerAlways: true}
	return NewManager(fc, st, cfg), st, dir
}

func TestManager_New_CreatesConversationAndRuns(t *testing.T) {
	fc := &fakeClaude{
		decision: RouteDecision{Action: ActionNew, Project: "myapp", NewTitle: "로그인 버그"},
		runRes:   RunResult{Text: "수정 완료"},
	}
	m, st, _ := mgrFixture(t, fc)
	f := &fakeSender{}

	m.Handle(context.Background(), 1, "로그인 고쳐줘", f)

	if fc.runCalls != 1 {
		t.Fatalf("Run called %d times", fc.runCalls)
	}
	if fc.lastRun.Resume {
		t.Error("new conversation should not Resume")
	}
	p, _ := st.GetProject("myapp")
	if len(p.Conversations) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(p.Conversations))
	}
	// header + result sent
	joined := ""
	for _, s := range f.sent {
		joined += s + "\n"
	}
	if !contains(joined, "수정 완료") || !contains(joined, "새 대화") {
		t.Errorf("messages = %v", f.sent)
	}
	// conversation persisted as Started with summary
	if st.GetActive().Project != "myapp" {
		t.Error("active project not set")
	}
}

func TestManager_Resume_UsesExistingSession(t *testing.T) {
	fc := &fakeClaude{runRes: RunResult{Text: "이어서 처리"}}
	m, st, _ := mgrFixture(t, fc)
	c, _ := st.NewConversation("myapp", "기존 대화")
	c.Started = true
	_ = st.UpdateConversation("myapp", c)

	fc.decision = RouteDecision{Action: ActionResume, Project: "myapp", ConversationID: c.ID}
	f := &fakeSender{}
	m.Handle(context.Background(), 1, "계속하자", f)

	if !fc.lastRun.Resume {
		t.Error("existing started conversation should Resume")
	}
	if fc.lastRun.SessionID != c.SessionID {
		t.Errorf("session = %q, want %q", fc.lastRun.SessionID, c.SessionID)
	}
}

func TestManager_Clarify_DoesNotRun(t *testing.T) {
	fc := &fakeClaude{decision: RouteDecision{Action: ActionClarify, Clarify: "1) A 2) B"}}
	m, _, _ := mgrFixture(t, fc)
	f := &fakeSender{}
	m.Handle(context.Background(), 1, "그거 다시", f)

	if fc.runCalls != 0 {
		t.Error("clarify must not run worker")
	}
	if len(f.sent) != 1 || !contains(f.sent[0], "1) A 2) B") {
		t.Errorf("messages = %v", f.sent)
	}
}

func TestManager_RouteError_FallsBackToActive(t *testing.T) {
	fc := &fakeClaude{routeErr: errors.New("boom"), runRes: RunResult{Text: "fallback ok"}}
	m, st, _ := mgrFixture(t, fc)
	c, _ := st.NewConversation("myapp", "활성 대화")
	c.Started = true
	_ = st.UpdateConversation("myapp", c)
	_ = st.SetActive("myapp", c.ID)

	f := &fakeSender{}
	m.Handle(context.Background(), 1, "뭔가 해줘", f)

	if fc.runCalls != 1 {
		t.Errorf("expected fallback run, got %d calls", fc.runCalls)
	}
}

func TestManager_RouteError_NoActive_Asks(t *testing.T) {
	fc := &fakeClaude{routeErr: errors.New("boom")}
	m, _, _ := mgrFixture(t, fc)
	f := &fakeSender{}
	m.Handle(context.Background(), 1, "뭔가 해줘", f)

	if fc.runCalls != 0 {
		t.Error("should not run without active conversation")
	}
	if len(f.sent) == 0 {
		t.Error("should ask the user")
	}
}

func TestManager_NoProjects_Guides(t *testing.T) {
	fc := &fakeClaude{}
	st := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	_ = st.Load()
	m := NewManager(fc, st, &Config{ManagerAlways: true})
	f := &fakeSender{}
	m.Handle(context.Background(), 1, "hi", f)
	if len(f.sent) != 1 || !contains(f.sent[0], "!project add") {
		t.Errorf("messages = %v", f.sent)
	}
}

func TestManager_AutoContinuation_LargeHistory(t *testing.T) {
	// Create a conversation with large history to trigger auto-continuation
	fc := &fakeClaude{runRes: RunResult{Text: "더 많은 작업 완료"}}
	m, st, _ := mgrFixture(t, fc)

	// Create initial conversation with large history
	c, _ := st.NewConversation("myapp", "큰 프로젝트")
	c.Started = true
	c.Summary = "이전에 많은 작업을 했습니다"

	// Add large history to trigger continuation (with actual characters to count tokens)
	longPrompt := "여기는 매우 긴 프롬프트입니다. "
	longResponse := "여기는 매우 긴 응답입니다. "
	for i := 0; i < 5000; i++ { // ~70k tokens with multiplier
		longPrompt += "긴 텍스트를 반복합니다. "
		longResponse += "긴 응답을 반복합니다. "
	}

	largeTurn := ConversationTurn{
		Prompt:   longPrompt,
		Response: longResponse,
	}
	c.History = append(c.History, largeTurn)
	_ = st.UpdateConversation("myapp", c)

	// Verify token estimation exceeds threshold
	estimatedTokens := estimateTokens(longPrompt) + estimateTokens(longResponse)
	if estimatedTokens < 50000 {
		t.Logf("Warning: estimated tokens %d < threshold 50000; test may not trigger continuation", estimatedTokens)
	}

	// Set this as active and resume with more input
	_ = st.SetActive("myapp", c.ID)

	fc.decision = RouteDecision{Action: ActionResume, Project: "myapp", ConversationID: c.ID}
	f := &fakeSender{}
	m.Handle(context.Background(), 1, "계속 작업해줘", f)

	// Verify continuation was created
	p, _ := st.GetProject("myapp")
	if len(p.Conversations) != 2 {
		t.Fatalf("expected 2 conversations (original + continuation), got %d", len(p.Conversations))
	}

	// Find the continuation conversation
	var continuation *Conversation
	for _, conv := range p.Conversations {
		if conv.ParentID != "" && conv.IsContinuation {
			continuation = conv
			break
		}
	}
	if continuation == nil {
		t.Fatal("continuation conversation not found")
	}

	// Verify parent-child link
	if continuation.ParentID != c.ID {
		t.Errorf("continuation.ParentID = %q, want %q", continuation.ParentID, c.ID)
	}
	if c.ChildID != continuation.ID {
		t.Errorf("original.ChildID not updated: %q, want %q", c.ChildID, continuation.ID)
	}

	// Verify the prompt includes parent summary
	if !contains(fc.lastRun.Prompt, "이전에 많은 작업을 했습니다") {
		t.Errorf("continuation prompt should include parent summary, got: %q", fc.lastRun.Prompt)
	}

	// Verify title includes series indicator
	if !contains(continuation.Title, "시리즈") {
		t.Errorf("continuation title should include series indicator: %q", continuation.Title)
	}

	// Verify active is set to continuation
	if st.GetActive().ConversationID != continuation.ID {
		t.Errorf("active should be continuation, got %q", st.GetActive().ConversationID)
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text  string
		name  string
		check func(int) bool
	}{
		{"", "empty", func(n int) bool { return n == 0 }},
		{"hello", "one-word", func(n int) bool { return n > 0 && n < 10 }},
		{"hello world this is a test", "multi-word", func(n int) bool { return n >= 5 && n < 15 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.text)
			if !tt.check(got) {
				t.Errorf("estimateTokens(%q) = %d, check failed", tt.text, got)
			}
		})
	}
}

func TestManager_StatusAction_ReturnsActiveWorkers(t *testing.T) {
	// Manager Claude returns ActionStatus → should show worker status without running Worker.
	fc := &fakeClaude{
		decision: RouteDecision{Action: ActionStatus},
		runRes:   RunResult{Text: "처리 중"},
	}
	m, st, _ := mgrFixture(t, fc)

	// Create conversation and mark as started
	c, _ := st.NewConversation("myapp", "작업 중")
	c.Started = true
	_ = st.UpdateConversation("myapp", c)

	// Manually record a running worker
	_ = m.workerStatus.SetStatus(WorkerStatus{
		Project:        "myapp",
		ConversationID: c.ID,
		Title:          "작업 중",
		Status:         "running",
		StartTime:      time.Now().Add(-30 * time.Second),
	})

	f := &fakeSender{}
	m.Handle(context.Background(), 1, "진행 중이야?", f)

	if fc.runCalls != 0 {
		t.Errorf("ActionStatus must not run Worker, got %d calls", fc.runCalls)
	}
	joined := ""
	for _, s := range f.sent {
		joined += s + "\n"
	}
	if !contains(joined, "실행 중인 작업") {
		t.Errorf("should show active workers, got: %s", joined)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
