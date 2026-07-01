package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Design Ref: §2.2, §4.1, §6.3 — routing orchestration, clarify, fallback. Application layer.

type Manager struct {
	client       ClaudeClient
	backendName  string // "claude" | "codex"
	backendMu    sync.RWMutex
	claudeClient ClaudeClient // preserved for switching back to claude
	codexClient  ClaudeClient // nil if codex not available

	store        StoreRepo
	workerStatus WorkerStatusStore
	scheduler    *Scheduler
	cfgh         *ConfigHolder
}

func NewManager(claude ClaudeClient, codex ClaudeClient, store StoreRepo, cfgh *ConfigHolder) *Manager {
	return &Manager{
		client:       claude,
		backendName:  "claude",
		claudeClient: claude,
		codexClient:  codex,
		store:        store,
		workerStatus: NewMemoryWorkerStatusStore(),
		cfgh:         cfgh,
	}
}

func (m *Manager) cfg() *Config { return m.cfgh.Get() }

func (m *Manager) SetScheduler(s *Scheduler) { m.scheduler = s }

// SetBackend switches the active AI backend. Returns error if the requested backend is unavailable.
func (m *Manager) SetBackend(name string) error {
	m.backendMu.Lock()
	defer m.backendMu.Unlock()
	switch name {
	case "claude":
		m.client = m.claudeClient
		m.backendName = "claude"
		log.Printf("[manager] backend → claude (worker_model=%q)", m.cfg().WorkerModel)
	case "codex":
		if m.codexClient == nil {
			return fmt.Errorf("Codex가 설치되어 있지 않습니다")
		}
		m.client = m.codexClient
		m.backendName = "codex"
		log.Printf("[manager] backend → codex (codex_model=%q codex_manager_model=%q)", m.cfg().CodexModel, m.cfg().CodexManagerModel)
	default:
		return fmt.Errorf("알 수 없는 백엔드: %s (claude | codex)", name)
	}
	// Persist so the setting survives restarts.
	if err := m.store.SetStoredBackend(name); err != nil {
		log.Printf("[manager] backend persist failed: %v", err)
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

// detectBackendSwitchIntent checks if the message explicitly intends to switch the AI backend.
// Requires an explicit switch verb to avoid false positives when messages merely mention
// "codex" or "backend" in a non-switching context.
// Returns "codex" or "claude" if switching intent is detected, "" otherwise.
func detectBackendSwitchIntent(text string) string {
	lower := strings.ToLower(text)
	switchVerbs := []string{
		"전환", "바꿔", "변경", "switch", "써줘", "사용해줘", "사용해", "써", "바꿔줘", "전환해",
	}
	hasSwitchVerb := false
	for _, v := range switchVerbs {
		if strings.Contains(lower, v) {
			hasSwitchVerb = true
			break
		}
	}
	if !hasSwitchVerb {
		return ""
	}
	if strings.Contains(lower, "codex") {
		return "codex"
	}
	if strings.Contains(lower, "claude") {
		return "claude"
	}
	return ""
}

// Handle routes a free-text message to the right project/conversation and runs the Worker.
// Plan SC: 자연어 → 정확 라우팅 → 해당 디렉토리 작업, 대화별 맥락 분리.
func (m *Manager) Handle(ctx context.Context, chatID int64, text string, s MessageSender) {
	// Pre-check: auto-switch backend if the message mentions it.
	if target := detectBackendSwitchIntent(text); target != "" && target != m.Backend() {
		if err := m.SetBackend(target); err != nil {
			_ = s.Send(chatID, "⚠️ 백엔드 전환 실패: "+err.Error())
		} else {
			_ = s.Send(chatID, "🔄 백엔드 전환: "+strings.ToUpper(target))
		}
	}

	m.backendMu.RLock()
	currentBackend := m.backendName
	currentClient := m.client
	m.backendMu.RUnlock()

	projects := m.store.ListProjects()
	if len(projects) == 0 {
		_ = s.Send(chatID, "등록된 프로젝트가 없습니다. 먼저 등록하세요:\n!project add <이름> <경로>")
		return
	}

	dec, ok := m.decide(ctx, currentClient, text)
	if !ok {
		// Routing failed entirely → fall back to the active conversation, else ask.
		if active := m.store.GetActive(); active.Project != "" {
			if c, exists := m.store.GetConversation(active.Project, active.ConversationID); exists {
				m.runWorker(ctx, chatID, text, active.Project, "", c, s, currentClient)
				return
			}
		}
		_ = s.Send(chatID, "🤔 어느 프로젝트/대화에서 할지 모르겠어요. !project list 로 확인하거나 !chat use <id> 로 지정해 주세요.")
		return
	}

	// Guard: a resume/new decision that names an empty or unregistered project
	// (common when the LLM can't pin a vague follow-up). Continue the active
	// conversation if there is one; otherwise ask — never crash into NewConversation("").
	if dec.Action == ActionResume || dec.Action == ActionNew {
		if _, pok := m.store.GetProject(dec.Project); !pok {
			if active := m.store.GetActive(); active.Project != "" {
				if ac, exists := m.store.GetConversation(active.Project, active.ConversationID); exists {
					m.runWorker(ctx, chatID, text, active.Project, "", ac, s, currentClient)
					return
				}
			}
			_ = s.Send(chatID, "🤔 어느 프로젝트에서 이어갈지 모르겠어요. !project list 로 확인하거나 \"<프로젝트명> ...\" 처럼 프로젝트를 함께 적어 주세요.")
			return
		}
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
		m.runWorker(ctx, chatID, text, dec.Project, "", c, s, currentClient)

	case ActionResume:
		c, exists := m.store.GetConversation(dec.Project, dec.ConversationID)
		if !exists {
			// Conversation was deleted or never existed — start a new one instead of erroring.
			newC, cerr := m.store.NewConversation(dec.Project, "새 대화")
			if cerr != nil {
				_ = s.Send(chatID, "⚠️ 대화를 찾을 수 없어 새 대화 생성도 실패했습니다: "+cerr.Error())
				return
			}
			newC.Backend = currentBackend
			_ = m.store.UpdateConversation(dec.Project, newC)
			m.runWorker(ctx, chatID, text, dec.Project, "", newC, s, currentClient)
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
			m.runWorker(ctx, chatID, text, dec.Project, "", newConv, s, currentClient)
			return
		}
		m.runWorker(ctx, chatID, text, dec.Project, "", c, s, currentClient)

	default:
		_ = s.Send(chatID, "🤔 라우팅 결과를 이해하지 못했어요. !chat use <id> 로 대화를 지정해 주세요.")
	}
}

// decide returns the routing decision. With ManagerAlways=false it reuses the active
// conversation without a Manager call when one is set (token-saving optimization).
func (m *Manager) decide(ctx context.Context, client ClaudeClient, text string) (RouteDecision, bool) {
	if !m.cfg().ManagerAlways {
		if active := m.store.GetActive(); active.Project != "" {
			if _, exists := m.store.GetConversation(active.Project, active.ConversationID); exists {
				return RouteDecision{Action: ActionResume, Project: active.Project, ConversationID: active.ConversationID}, true
			}
		}
	}
	req := m.buildRouteRequest(text)
	dec, err := client.Route(ctx, req)
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

// isSessionNotFound detects a lost/absent CLI session — e.g. `--resume <id>`
// after a bot restart or CLI update, where the session store no longer has that
// ID. The CLI exits non-zero with "No conversation found with session ID: ...".
func isSessionNotFound(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "no conversation found") ||
		strings.Contains(lower, "session not found")
}

// workerModelForBackend returns the right model string based on the active backend.
func (m *Manager) workerModelForBackend() string {
	if m.Backend() == "codex" {
		return m.cfg().CodexModel
	}
	return m.cfg().WorkerModel
}

// runWorker executes the Worker turn for a resolved (project, conversation) and relays output.
// workDir overrides the project's path as the Claude CLI working directory (e.g. a git worktree).
// Pass "" to use the project's registered path.
func (m *Manager) runWorker(ctx context.Context, chatID int64, text, project, workDir string, c *Conversation, s MessageSender, client ClaudeClient) {
	p, ok := m.store.GetProject(project)
	if !ok {
		_ = s.Send(chatID, "⚠️ 프로젝트를 찾을 수 없습니다: "+project)
		return
	}
	if workDir == "" {
		workDir = p.Path
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

	// Pass history in the prompt as a restart-safe fallback.
	// When --resume is in play, the CLI session already carries full context server-side,
	// so only a short trailing reminder is needed (avoids re-sending the whole history
	// every turn, which was making prompts — and response times — grow with each message).
	// Without an existing session (fresh conversation), history is empty anyway.
	// If the session is ever lost (e.g. after restart or CLI update), the short reminder
	// is what's available; deeper recovery relies on parentSummary/.teleclaude/memory.md.
	const maxHistoryInPrompt = 3
	historyForPrompt := workConv.History
	if len(historyForPrompt) > maxHistoryInPrompt {
		historyForPrompt = historyForPrompt[len(historyForPrompt)-maxHistoryInPrompt:]
	}
	globalMemory := readGlobalMemory()
	projectMemory := readProjectMemory(p.Path)
	prompt := buildContextPrompt(text, parentSummary, globalMemory, projectMemory, historyForPrompt)

	backend := m.Backend()
	workerModel := m.workerModelForBackend()
	log.Printf("[worker] ▶ backend=%s model=%q project=%s conv=%s resume=%v prompt=%d chars",
		backend, workerModel, project, workConv.ID, workConv.Started, len(prompt))

	res, err := client.Run(ctx, RunRequest{
		Prompt:    prompt,
		WorkDir:   workDir,
		SessionID: workConv.SessionID,
		Resume:    workConv.Started,
		Model:     workerModel,
	})
	close(heartbeatDone)
	elapsed := time.Since(startTime)

	if err != nil {
		if ctx.Err() != nil {
			log.Printf("[worker] ✗ backend=%s context cancelled/timeout after %s", backend, elapsed)
			_ = m.workerStatus.UpdateStatus(project, workConv.ID, "timeout", ctx.Err().Error())
			return
		}
		// If --resume hard-failed because the CLI session was lost (bot restart or
		// CLI update dropped the session store), retry once as a fresh session. The
		// prompt already carries the recent-history reminder, so the conversation
		// continues seamlessly instead of dead-ending on an error.
		if workConv.Started && isSessionNotFound(err.Error()) {
			log.Printf("[worker] session lost (%v) — retrying once without --resume", err)
			_ = s.Send(chatID, "🔄 세션을 새로 시작해 대화를 이어갑니다...")
			// The retry is a full fresh turn (may take a while); keep a heartbeat
			// alive for it — the original one was already closed above.
			recoverDone := make(chan struct{})
			go func() {
				ticker := time.NewTicker(2 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						e := time.Since(startTime)
						_ = s.Send(chatID, fmt.Sprintf("⏳ 세션 복구 진행 중... (%d분 %d초 경과)", int(e.Minutes()), int(e.Seconds())%60))
					case <-recoverDone:
						return
					}
				}
			}()
			res, err = client.Run(ctx, RunRequest{
				Prompt:    prompt,
				WorkDir:   workDir,
				SessionID: workConv.SessionID,
				Resume:    false,
				Model:     workerModel,
			})
			close(recoverDone)
			elapsed = time.Since(startTime)
		}
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("[worker] ✗ backend=%s context cancelled/timeout after %s", backend, elapsed)
				_ = m.workerStatus.UpdateStatus(project, workConv.ID, "timeout", ctx.Err().Error())
				return
			}
			log.Printf("[worker] ✗ backend=%s error after %s: %v", backend, elapsed, err)
			_ = s.Send(chatID, "⚠️ 작업 실패: "+err.Error())
			_ = m.workerStatus.UpdateStatus(project, workConv.ID, "failed", err.Error())
			return
		}
		log.Printf("[worker] ✅ (session-recovered) backend=%s elapsed=%s output=%d bytes session=%q",
			backend, elapsed, len(res.Text), res.SessionID)
	}

	log.Printf("[worker] ✅ backend=%s elapsed=%s output=%d bytes session=%q",
		backend, elapsed, len(res.Text), res.SessionID)

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
			log.Printf("[worker] ▶ retry backend=%s model=%q conv=%s (context overflow)", backend, workerModel, workConv.ID)

			// Restart heartbeat for the retry turn — it may take another full TimeoutMinutes.
			retryDone := make(chan struct{})
			go func() {
				ticker := time.NewTicker(2 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						e := time.Since(startTime)
						_ = s.Send(chatID, fmt.Sprintf("⏳ 재시작 진행 중... (%d분 %d초 경과)", int(e.Minutes()), int(e.Seconds())%60))
					case <-retryDone:
						return
					}
				}
			}()
			res, err = client.Run(ctx, RunRequest{
				Prompt:    retryPrompt,
				WorkDir:   workDir,
				SessionID: workConv.SessionID,
				Resume:    false,
				Model:     workerModel,
			})
			close(retryDone)
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

	// Append to date-based history log for !history command.
	if res.Text != "" {
		if herr := WriteHistory(project, workConv.Title, text, res.Text); herr != nil {
			log.Printf("[manager] history write error: %v", herr)
		}
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
func (m *Manager) DescribeActiveWorkers() string {
	active := m.workerStatus.ListActive()
	if len(active) == 0 {
		return "실행 중인 작업 없음"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔄 실행 중인 작업 (%d개):\n\n", len(active))
	for i, ws := range active {
		elapsed := time.Since(ws.StartTime)
		mins := int(elapsed.Minutes())
		secs := int(elapsed.Seconds()) % 60

		var elapsedStr string
		if mins > 0 {
			elapsedStr = fmt.Sprintf("%d분 %d초", mins, secs)
		} else {
			elapsedStr = fmt.Sprintf("%d초", secs)
		}

		fmt.Fprintf(&sb, "%d) 📂 %s · 💬 %s\n", i+1, ws.Project, ws.Title)
		fmt.Fprintf(&sb, "   ⏱️ %s 경과\n", elapsedStr)
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

	// If LLM returned a 5-field cron expression (or @-shorthand), use AddTask directly.
	if isCronExpr(dec.ScheduleInterval) {
		kind := "알림"
		if dec.ScheduleIsTask {
			kind = "Claude 작업"
		}
		t := &Task{
			ID:        newTaskID(),
			ChatID:    chatID,
			Prompt:    dec.ScheduleTask,
			CronExpr:  dec.ScheduleInterval,
			Status:    "pending",
			IsTask:    dec.ScheduleIsTask,
			Label:     truncate(dec.ScheduleTask, 30),
			CreatedAt: time.Now(),
		}
		if err := m.scheduler.AddTask(t); err != nil {
			_ = s.Send(chatID, "⚠️ 작업 등록 실패: "+err.Error())
			return
		}
		_ = s.Send(chatID, fmt.Sprintf("✅ 예약 등록 [%s] %s (%s)\n  %s", t.ID, dec.ScheduleInterval, kind, dec.ScheduleTask))
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

// isCronExpr returns true if s looks like a 5-field cron expression or @-shorthand.
func isCronExpr(s string) bool {
	if strings.HasPrefix(s, "@") {
		return true
	}
	return len(strings.Fields(s)) == 5
}

// HandleScheduledTask executes a pre-scheduled task in a fresh conversation,
// bypassing the Manager LLM routing so the task prompt is not misinterpreted
// as a routing request and never leaks into a prior conversation's context.
func (m *Manager) HandleScheduledTask(ctx context.Context, chatID int64, text string, s MessageSender) {
	m.backendMu.RLock()
	currentBackend := m.backendName
	currentClient := m.client
	m.backendMu.RUnlock()

	projects := m.store.ListProjects()
	if len(projects) == 0 {
		_ = s.Send(chatID, "⚠️ 예약 작업 실행 실패: 등록된 프로젝트가 없습니다. !project add <이름> <경로>")
		return
	}

	// Prefer the active project; fall back to alphabetically first to ensure determinism.
	projectName := m.store.GetActive().Project
	if _, ok := m.store.GetProject(projectName); !ok {
		names := make([]string, 0, len(projects))
		for name := range projects {
			names = append(names, name)
		}
		sort.Strings(names)
		projectName = names[0]
	}

	c, err := m.store.NewConversation(projectName, "📅 "+truncate(text, 28))
	if err != nil {
		_ = s.Send(chatID, "⚠️ 예약 작업 대화 생성 실패: "+err.Error())
		return
	}
	c.Backend = currentBackend
	_ = m.store.UpdateConversation(projectName, c)

	// Create a git worktree for isolation: parallel scheduled tasks on the same project
	// work in separate directories, preventing file-level conflicts.
	p, _ := m.store.GetProject(projectName)
	workDir := p.Path
	wtID := newTaskID()
	if wtPath, err := CreateWorktree(p.Path, wtID); err != nil {
		log.Printf("[manager] worktree create failed, falling back to project dir: %v", err)
	} else if wtPath != "" {
		workDir = wtPath
		defer RemoveWorktree(p.Path, wtPath)
		log.Printf("[manager] worktree created: %s", wtPath)
	}

	log.Printf("[manager] scheduled task → project=%s conv=%s workDir=%s", projectName, c.ID, workDir)
	m.runWorker(ctx, chatID, text, projectName, workDir, c, s, currentClient)
}

// timeNow is a replaceable clock for testing.
var timeNow = time.Now
