package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Design Ref: §2.2, §4.1, §6.3 — routing orchestration, clarify, fallback. Application layer.

type Manager struct {
	client       ClaudeClient
	backendName  string       // "claude" | "codex"
	backendMu    sync.RWMutex
	claudeClient ClaudeClient // preserved for switching back to claude
	codexClient  ClaudeClient // nil if codex not available

	store        StoreRepo
	workerStatus WorkerStatusStore
	scheduler    *Scheduler
	cfg          *Config
}

func NewManager(claude ClaudeClient, codex ClaudeClient, store StoreRepo, cfg *Config) *Manager {
	return &Manager{
		client:       claude,
		backendName:  "claude",
		claudeClient: claude,
		codexClient:  codex,
		store:        store,
		workerStatus: NewMemoryWorkerStatusStore(),
		cfg:          cfg,
	}
}

func (m *Manager) SetScheduler(s *Scheduler) { m.scheduler = s }

// SetBackend switches the active AI backend. Returns error if the requested backend is unavailable.
func (m *Manager) SetBackend(name string) error {
	m.backendMu.Lock()
	defer m.backendMu.Unlock()
	switch name {
	case "claude":
		m.client = m.claudeClient
		m.backendName = "claude"
	case "codex":
		if m.codexClient == nil {
			return fmt.Errorf("Codex가 설치되어 있지 않습니다")
		}
		m.client = m.codexClient
		m.backendName = "codex"
	default:
		return fmt.Errorf("알 수 없는 백엔드: %s (claude | codex)", name)
	}
	return nil
}

// Backend returns the current backend name.
func (m *Manager) Backend() string {
	m.backendMu.RLock()
	defer m.backendMu.RUnlock()
	return m.backendName
}

// CodexAvailable reports whether codex is registered.
func (m *Manager) CodexAvailable() bool {
	return m.codexClient != nil
}

// Handle routes a free-text message to the right project/conversation and runs the Worker.
// Plan SC: 자연어 → 정확 라우팅 → 해당 디렉토리 작업, 대화별 맥락 분리.
func (m *Manager) Handle(ctx context.Context, chatID int64, text string, s MessageSender) {
	m.backendMu.RLock()
	currentBackend := m.backendName
	m.backendMu.RUnlock()

	projects := m.store.ListProjects()
	if len(projects) == 0 {
		_ = s.Send(chatID, "등록된 프로젝트가 없습니다. 먼저 등록하세요:\n!project add <이름> <경로>")
		return
	}

	dec, ok := m.decide(ctx, text)
	if !ok {
		// Routing failed entirely → fall back to the active conversation, else ask.
		if active := m.store.GetActive(); active.Project != "" {
			if c, exists := m.store.GetConversation(active.Project, active.ConversationID); exists {
				m.runWorker(ctx, chatID, text, active.Project, c, s)
				return
			}
		}
		_ = s.Send(chatID, "🤔 어느 프로젝트/대화에서 할지 모르겠어요. !project list 로 확인하거나 !chat use <id> 로 지정해 주세요.")
		return
	}

	switch dec.Action {
	case ActionStatus:
		_ = s.Send(chatID, m.DescribeActiveWorkers())

	case ActionSchedule:
		m.handleSchedule(chatID, dec, s)

	case ActionClarify:
		msg := dec.Clarify
		if msg == "" {
			msg = "어느 대화를 말씀하시는지 알려주세요. !chat list 로 목록을 볼 수 있어요."
		}
		_ = s.Send(chatID, "🤔 "+msg)

	case ActionNew:
		if _, exists := m.store.GetProject(dec.Project); !exists {
			_ = s.Send(chatID, "🤔 어느 프로젝트인지 분명하지 않아요. !project list 를 확인해 주세요.")
			return
		}
		c, err := m.store.NewConversation(dec.Project, dec.NewTitle)
		if err != nil {
			_ = s.Send(chatID, "⚠️ 새 대화 생성 실패: "+err.Error())
			return
		}
		c.Backend = currentBackend
		_ = m.store.UpdateConversation(dec.Project, c)
		m.runWorker(ctx, chatID, text, dec.Project, c, s)

	case ActionResume:
		c, exists := m.store.GetConversation(dec.Project, dec.ConversationID)
		if !exists {
			_ = s.Send(chatID, "⚠️ 대화를 찾을 수 없습니다.")
			return
		}
		convBackend := c.Backend
		if convBackend == "" {
			convBackend = "claude"
		}
		if convBackend != currentBackend {
			_ = s.Send(chatID, fmt.Sprintf("⚠️ 백엔드 변경으로 새 대화를 시작합니다. [%s]", strings.ToUpper(currentBackend)))
			newConv, cerr := m.store.NewConversation(dec.Project, "새 대화 ("+currentBackend+")")
			if cerr != nil {
				_ = s.Send(chatID, "⚠️ 새 대화 생성 실패: "+cerr.Error())
				return
			}
			newConv.Backend = currentBackend
			_ = m.store.UpdateConversation(dec.Project, newConv)
			_ = m.store.SetActive(dec.Project, newConv.ID)
			m.runWorker(ctx, chatID, text, dec.Project, newConv, s)
			return
		}
		m.runWorker(ctx, chatID, text, dec.Project, c, s)

	default:
		_ = s.Send(chatID, "🤔 라우팅 결과를 이해하지 못했어요. !chat use <id> 로 대화를 지정해 주세요.")
	}
}

// decide returns the routing decision. With ManagerAlways=false it reuses the active
// conversation without a Manager call when one is set (token-saving optimization).
func (m *Manager) decide(ctx context.Context, text string) (RouteDecision, bool) {
	if !m.cfg.ManagerAlways {
		if active := m.store.GetActive(); active.Project != "" {
			if _, exists := m.store.GetConversation(active.Project, active.ConversationID); exists {
				return RouteDecision{Action: ActionResume, Project: active.Project, ConversationID: active.ConversationID}, true
			}
		}
	}
	req := m.buildRouteRequest(text)
	dec, err := m.client.Route(ctx, req)
	if err != nil {
		log.Printf("[manager] route error: %v", err)
		return RouteDecision{}, false
	}
	return dec, true
}

func (m *Manager) buildRouteRequest(text string) RouteRequest {
	const maxConvsPerProject = 10 // keep routing prompt lean
	projects := m.store.ListProjects()
	summaries := make([]ProjectSummary, 0, len(projects))
	for name, p := range projects {
		ids := sortedConvIDsByActivity(p.Conversations)
		if len(ids) > maxConvsPerProject {
			ids = ids[:maxConvsPerProject]
		}
		convs := make([]ConversationSummary, 0, len(ids))
		for _, id := range ids {
			c := p.Conversations[id]
			convs = append(convs, ConversationSummary{ID: c.ID, Title: c.Title, Summary: c.Summary})
		}
		summaries = append(summaries, ProjectSummary{Name: name, Conversations: convs})
	}
	return RouteRequest{Message: text, Projects: summaries, Active: m.store.GetActive()}
}

// chainInfo walks to the chain root and returns the base title and next series number.
func (m *Manager) chainInfo(project string, c *Conversation) (string, int) {
	baseTitle := c.Title
	seriesNum := 2
	cur := c
	for cur.ParentID != "" {
		parent, ok := m.store.GetParent(project, cur.ID)
		if !ok {
			break
		}
		baseTitle = parent.Title
		seriesNum++
		cur = parent
	}
	return baseTitle, seriesNum
}

// makeContinuation creates a new continuation conversation linked to parent.
func (m *Manager) makeContinuation(project string, parent *Conversation) (*Conversation, error) {
	baseTitle, seriesNum := m.chainInfo(project, parent)
	newC, err := m.store.NewConversation(project, fmt.Sprintf("%s (시리즈 %d)", baseTitle, seriesNum))
	if err != nil {
		return nil, err
	}
	newC.ParentID = parent.ID
	newC.IsContinuation = true
	parent.ChildID = newC.ID
	if uerr := m.store.UpdateConversation(project, parent); uerr != nil {
		log.Printf("[manager] update parent childID: %v", uerr)
	}
	return newC, nil
}

// isContextOverflow detects Claude CLI "Prompt is too long" context limit errors.
func isContextOverflow(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "prompt is too long") ||
		strings.Contains(lower, "context length exceeded") ||
		strings.Contains(lower, "context window")
}

// runWorker executes the Worker turn for a resolved (project, conversation) and relays output.
// If history grows too large, it auto-creates a continuation conversation.
func (m *Manager) runWorker(ctx context.Context, chatID int64, text, project string, c *Conversation, s MessageSender) {
	p, ok := m.store.GetProject(project)
	if !ok {
		_ = s.Send(chatID, "⚠️ 프로젝트를 찾을 수 없습니다: "+project)
		return
	}

	// Check if context is growing too large; auto-create continuation if needed.
	// Threshold: ~40k tokens (conservative estimate for claude-haiku).
	const contextThreshold = 40000
	parentSummary := ""
	workConv := c

	historyTokens := 0
	for _, turn := range c.History {
		historyTokens += estimateTokens(turn.Prompt)
		historyTokens += estimateTokens(turn.Response)
	}
	currentTokens := estimateTokens(text)
	totalTokens := historyTokens + currentTokens

	if totalTokens > contextThreshold {
		summary := c.Summary
		if summary == "" {
			summary = "이전 대화 내용을 참고해 주세요."
		}
		if newC, err := m.makeContinuation(project, c); err != nil {
			log.Printf("[manager] auto-continuation failed: %v", err)
		} else {
			newC.Backend = m.Backend()
			parentSummary = summary
			workConv = newC
			_ = s.Send(chatID, "📝 대화가 길어져서 새 시리즈를 시작합니다...")
		}
	}

	s.Typing(chatID)
	isNewConv := !workConv.Started
	_ = s.Send(chatID, routingHeader(project, workConv.Title, isNewConv))

	// Record Worker status as running
	_ = m.workerStatus.SetStatus(WorkerStatus{
		Project:        project,
		ConversationID: workConv.ID,
		Title:          workConv.Title,
		Status:         "running",
		StartTime:      time.Now(),
	})

	startTime := time.Now()

	// Heartbeat: notify every 2 minutes while Worker is running.
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(startTime)
				mins := int(elapsed.Minutes())
				secs := int(elapsed.Seconds()) % 60
				_ = s.Send(chatID, fmt.Sprintf("⏳ 작업 진행 중... (%d분 %d초 경과)", mins, secs))
			case <-heartbeatDone:
				return
			}
		}
	}()

	// Always pass history in the prompt as a restart-safe fallback.
	// If --resume finds the claude session, the truncated history is a lightweight reminder.
	// If the session is lost (e.g. after restart or CLI update), history is the only context.
	historyForPrompt := workConv.History
	globalMemory := readGlobalMemory()
	projectMemory := readProjectMemory(p.Path)
	prompt := buildContextPrompt(text, parentSummary, globalMemory, projectMemory, historyForPrompt)
	res, err := m.client.Run(ctx, RunRequest{
		Prompt:    prompt,
		WorkDir:   p.Path,
		SessionID: workConv.SessionID,
		Resume:    workConv.Started,
		Model:     m.cfg.WorkerModel,
	})
	close(heartbeatDone)
	elapsed := time.Since(startTime)

	if err != nil {
		if ctx.Err() != nil {
			// Timeout or cancelled
			_ = m.workerStatus.UpdateStatus(project, workConv.ID, "timeout", ctx.Err().Error())
			return
		}
		_ = s.Send(chatID, "⚠️ 작업 실패: "+err.Error())
		_ = m.workerStatus.UpdateStatus(project, workConv.ID, "failed", err.Error())
		return
	}

	// Reactive: if Worker hit Claude's context limit, auto-create continuation and retry once.
	if res.IsError && isContextOverflow(res.Text) {
		overflowSummary := workConv.Summary
		if overflowSummary == "" {
			overflowSummary = "이전 대화 내용을 참고해 주세요."
		}
		if newC, cerr := m.makeContinuation(project, workConv); cerr == nil {
			newC.Backend = m.Backend()
			workConv = newC
			_ = s.Send(chatID, "📝 세션 한계에 도달해 새 시리즈로 재시작합니다...")
			retryPrompt := buildContextPrompt(text, overflowSummary, globalMemory, projectMemory, nil)
			res, err = m.client.Run(ctx, RunRequest{
				Prompt:    retryPrompt,
				WorkDir:   p.Path,
				SessionID: workConv.SessionID,
				Resume:    false,
				Model:     m.cfg.WorkerModel,
			})
			elapsed = time.Since(startTime)
			if err != nil {
				_ = s.Send(chatID, "⚠️ 재시작 후 작업 실패: "+err.Error())
				_ = m.workerStatus.UpdateStatus(project, workConv.ID, "failed", err.Error())
				return
			}
		} else {
			log.Printf("[manager] reactive continuation failed: %v", cerr)
		}
	}

	_ = sendChunked(s, chatID, res.Text)

	// Persist conversation progress and history.
	wasStarted := workConv.Started
	workConv.Started = true
	workConv.LastActivity = time.Now().UTC()
	if res.Text != "" {
		workConv.Summary = truncate(res.Text, 80)
	}

	// Append this turn to conversation history.
	// Keep only the most recent maxHistoryTurns (short-term memory).
	// Older context should be summarised into .teleclaude/memory.md by the Worker.
	const maxHistoryTurns = 20
	workConv.History = append(workConv.History, ConversationTurn{
		Timestamp: time.Now().UTC(),
		Prompt:    text,
		Response:  res.Text,
	})
	if len(workConv.History) > maxHistoryTurns {
		workConv.History = workConv.History[len(workConv.History)-maxHistoryTurns:]
	}
	if res.SessionID != "" && !wasStarted {
		workConv.SessionID = res.SessionID
	}

	if err := m.store.UpdateConversation(project, workConv); err != nil {
		log.Printf("[manager] update conversation: %v", err)
	}
	if err := m.store.SetActive(project, workConv.ID); err != nil {
		log.Printf("[manager] set active: %v", err)
	}

	// Update Worker status to completed
	_ = m.workerStatus.UpdateStatus(project, workConv.ID, "completed", "")

	// Send completion notification with elapsed time
	completionMsg := formatCompletion(elapsed)
	_ = s.Send(chatID, completionMsg)
}

// describeActive returns a human-readable active pointer (used by !status-like replies).
func (m *Manager) describeActive() string {
	a := m.store.GetActive()
	if a.Project == "" {
		return "활성 대화 없음"
	}
	if c, ok := m.store.GetConversation(a.Project, a.ConversationID); ok {
		return fmt.Sprintf("📂 %s · 💬 %s", a.Project, c.Title)
	}
	return "활성 대화 없음"
}

// GetWorkerStatus returns status of a specific Worker or empty if not found.
func (m *Manager) GetWorkerStatus(project, convID string) (WorkerStatus, bool) {
	return m.workerStatus.GetStatus(project, convID)
}

// DescribeActiveWorkers returns a human-readable status of all running Workers.
// If hasETA is true, it also estimates remaining time (rough heuristic).
func (m *Manager) DescribeActiveWorkers() string {
	active := m.workerStatus.ListActive()
	if len(active) == 0 {
		return "실행 중인 작업 없음"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔄 실행 중인 작업 (%d개):\n\n", len(active)))
	for i, ws := range active {
		elapsed := time.Since(ws.StartTime)
		mins := int(elapsed.Minutes())
		secs := int(elapsed.Seconds()) % 60

		elapsedStr := ""
		if mins > 0 {
			elapsedStr = fmt.Sprintf("%d분 %d초", mins, secs)
		} else {
			elapsedStr = fmt.Sprintf("%d초", secs)
		}

		sb.WriteString(fmt.Sprintf("%d) 📂 %s · 💬 %s\n", i+1, ws.Project, ws.Title))
		sb.WriteString(fmt.Sprintf("   ⏱️ %s 경과\n", elapsedStr))

		// Rough ETA: if running < 30s, likely quick; if > 2min, likely long task
		if mins > 2 {
			estimatedTotal := time.Duration(mins*2) * time.Minute // rough guess: double the elapsed time
			remaining := estimatedTotal - elapsed
			if remaining > 0 {
				remainMins := int(remaining.Minutes())
				remainSecs := int(remaining.Seconds()) % 60
				sb.WriteString(fmt.Sprintf("   ≈ %d분 %d초 남음 (예상)\n", remainMins, remainSecs))
			}
		}
	}
	return sb.String()
}

// formatCompletion formats the work completion notification with elapsed time.
func formatCompletion(elapsed time.Duration) string {
	secs := int(elapsed.Seconds())
	mins := secs / 60
	secs = secs % 60

	var duration string
	if mins > 0 {
		duration = fmt.Sprintf("%d분 %d초", mins, secs)
	} else {
		duration = fmt.Sprintf("%d초", secs)
	}

	return fmt.Sprintf("✅ 작업 완료 (%s)", duration)
}

// readProjectMemory reads .teleclaude/memory.md from the project directory.
// Worker Claude can freely update this file to persist project-level knowledge.
func readProjectMemory(projectPath string) string {
	b, err := os.ReadFile(filepath.Join(projectPath, ".teleclaude", "memory.md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readGlobalMemory reads ~/.teleclaude/global-memory.md for cross-project long-term memory.
func readGlobalMemory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".teleclaude", "global-memory.md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// buildContextPrompt assembles the Worker prompt from available context layers.
// Layer order: global memory → project memory → parent summary → recent history → current request.
// Only adds sections when content exists — no empty headers.
func buildContextPrompt(currentPrompt, parentSummary, globalMemory, projectMemory string, history []ConversationTurn) string {
	hasContext := globalMemory != "" || projectMemory != "" || parentSummary != "" || len(history) > 0
	if !hasContext {
		return currentPrompt
	}

	var sb strings.Builder

	if globalMemory != "" {
		sb.WriteString("## 장기 기억 (글로벌)\n\n")
		sb.WriteString(globalMemory)
		sb.WriteString("\n\n---\n\n")
	}

	if projectMemory != "" {
		sb.WriteString("## 프로젝트 메모리\n\n")
		sb.WriteString(projectMemory)
		sb.WriteString("\n\n---\n\n")
	}

	if parentSummary != "" {
		sb.WriteString("## 이전 대화 요약\n\n")
		sb.WriteString(parentSummary)
		sb.WriteString("\n\n---\n\n")
	}

	if len(history) > 0 {
		sb.WriteString("## 최근 대화 기록\n\n")
		for i, turn := range history {
			// Truncate response to 300 chars — enough for context, avoids token bloat
			// when --resume also carries the full session.
			fmt.Fprintf(&sb, "**Turn %d** (%s)\n**요청:** %s\n**응답:** %s\n\n",
				i+1, turn.Timestamp.Format("2006-01-02 15:04"), turn.Prompt, truncate(turn.Response, 300))
		}
		sb.WriteString("---\n\n")
	}

	sb.WriteString("## 현재 요청\n\n")
	sb.WriteString(currentPrompt)
	sb.WriteString("\n\n> 중요한 결정/해결책은 .teleclaude/memory.md에 기록해두세요.")
	return sb.String()
}

// handleSchedule registers a reminder or cron job decoded from the Manager's routing decision.
func (m *Manager) handleSchedule(chatID int64, dec RouteDecision, s MessageSender) {
	if m.scheduler == nil {
		_ = s.Send(chatID, "⚠️ 스케줄러가 초기화되지 않았습니다.")
		return
	}
	if dec.ScheduleTask == "" {
		_ = s.Send(chatID, "🤔 어떤 내용을 언제 알림/실행할지 좀 더 구체적으로 말씀해주세요.")
		return
	}

	dur, label, err := ParseSchedule(dec.ScheduleInterval)
	if err != nil {
		_ = s.Send(chatID, fmt.Sprintf("🤔 시간을 파악하지 못했어요 (%q). 예) 30분 후에, 매시간, 매일", dec.ScheduleInterval))
		return
	}

	switch dec.ScheduleType {
	case "remind":
		r, err := m.scheduler.AddReminder(chatID, dec.ScheduleTask, timeNow().Add(dur))
		if err != nil {
			_ = s.Send(chatID, "⚠️ 알림 등록 실패: "+err.Error())
			return
		}
		_ = s.Send(chatID, fmt.Sprintf("✅ 알림 등록 [%s] — %s 후\n  %s", r.ID, label, dec.ScheduleTask))
	case "cron":
		c, err := m.scheduler.AddCron(chatID, label, dur, dec.ScheduleTask, dec.ScheduleIsTask)
		if err != nil {
			_ = s.Send(chatID, "⚠️ 크론 등록 실패: "+err.Error())
			return
		}
		kind := "알림"
		if dec.ScheduleIsTask {
			kind = "Claude 작업"
		}
		_ = s.Send(chatID, fmt.Sprintf("✅ 반복 등록 [%s] %s (%s)\n  %s", c.ID, label, kind, dec.ScheduleTask))
	default:
		_ = s.Send(chatID, "🤔 알림(일회성)인지 반복인지 명확하지 않아요. 예) 30분 후에 알림 / 매시간 서버 확인")
	}
}

// timeNow is a replaceable clock for testing.
var timeNow = time.Now
