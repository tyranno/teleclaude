package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
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

	// interactiveClient is a second ClaudeClient for the "claude" backend backed
	// by a persistent ConPTY session (interactiveClaudeRunner) instead of a
	// per-turn headless process. nil unless cfg.InteractiveClaude is on and
	// claude is installed (see main.go). Only conversations with
	// Conversation.Interactive=true ever use it — see clientFor/IsInteractive.
	interactiveClient ClaudeClient

	store        StoreRepo
	workerStatus WorkerStatusStore
	scheduler    *Scheduler
	cfgh         *ConfigHolder

	telegramMu sync.Mutex // serializes turns on the single global telegram conversation

	// durationsMu/turnDurations back the "recent average" turn-time estimate
	// shown when a new turn starts — see recordTurnDuration/estimateTurnDuration.
	durationsMu   sync.Mutex
	turnDurations map[string][]time.Duration // backend name → rolling window of completed-turn durations
}

func NewManager(claude ClaudeClient, codex ClaudeClient, store StoreRepo, cfgh *ConfigHolder) *Manager {
	m := &Manager{
		claudeClient: claude,
		codexClient:  codex,
		store:        store,
		workerStatus: NewMemoryWorkerStatusStore(),
		cfgh:         cfgh,
	}
	// Default to claude when available (backward-compatible); otherwise fall back
	// to codex so a codex-only install still boots with a valid active client
	// instead of a nil m.client.
	if claude != nil {
		m.client = claude
		m.backendName = "claude"
	} else if codex != nil {
		m.client = codex
		m.backendName = "codex"
	}
	return m
}

func (m *Manager) cfg() *Config { return m.cfgh.Get() }

func (m *Manager) SetScheduler(s *Scheduler) { m.scheduler = s }

// SetInteractiveClient installs the ConPTY-backed interactive runner (see
// runner_conpty_windows.go). Called once at startup, only when
// cfg.InteractiveClaude is on and claude is installed; nil otherwise, in
// which case clientFor/IsInteractive-gated turns silently fall back to the
// normal headless client (see clientFor).
func (m *Manager) SetInteractiveClient(c ClaudeClient) { m.interactiveClient = c }

// interactiveCloser is the optional lifecycle hook a ClaudeClient may
// implement to release resources held outside the request/response cycle —
// only interactiveClaudeRunner does (its resident claude.exe TUI processes),
// so this is a type assertion rather than an addition to ClaudeClient itself.
type interactiveCloser interface{ Close() }

// CloseInteractive terminates every resident interactive claude.exe session,
// if an interactive client was installed. Must be called before any process
// exit that bypasses normal deferred cleanup (os.Exit in bot.go's !update
// handoff and Conflict-triggered restart) — otherwise ConPTY-spawned
// claude.exe processes are reparented as orphans instead of exiting with
// their parent. Safe to call when no interactive client exists, or more than
// once.
func (m *Manager) CloseInteractive() {
	if c, ok := m.interactiveClient.(interactiveCloser); ok {
		c.Close()
	}
}

// clientFor returns the ClaudeClient a turn should run on: the interactive
// session when the conversation opted in (interactive=true) and one is
// actually available for this backend, otherwise the normal per-backend
// client. Interactive mode only exists for "claude" — codex has no ConPTY
// runner.
func (m *Manager) clientFor(backend string, interactive bool) ClaudeClient {
	client := m.clientForBackend(backend)
	if client != nil && interactive && backend == "claude" && m.interactiveClient != nil {
		return m.interactiveClient
	}
	return client
}

// IsInteractive reports whether tgt's conversation is opted into interactive
// mode. Bot.dispatch calls this (before a turn starts) to decide whether a
// message arriving while the lane is already running should steer into the
// live session concurrently instead of FIFO-queueing behind it — see
// runTurn/finishTurn in bot.go and pendingTurn in runner_conpty_windows.go.
// Always false for the telegram stream — see SetInteractive.
func (m *Manager) IsInteractive(tgt Target) bool {
	if !tgt.IsWeb() {
		return false
	}
	c, ok := m.store.GetWebConv(tgt.ID)
	return ok && c.Interactive
}

// SetInteractive toggles tgt's web conversation into or out of interactive
// mode ("!interactive on|off"). Refused for the telegram stream: that lane is
// serialized end-to-end by telegramMu (see handleTelegram/HandleWebTarget), so
// a steered second message would just queue behind the mutex instead of
// reaching the live session concurrently — silently defeating the point of
// steering. Also errors if no interactive runner was constructed
// (cfg.InteractiveClaude off or claude not installed).
func (m *Manager) SetInteractive(tgt Target, on bool) error {
	if !tgt.IsWeb() {
		return fmt.Errorf("텔레그램 대화에는 interactive 모드를 켤 수 없습니다 — 웹 대화(topic)에서만 지원됩니다")
	}
	if on && m.interactiveClient == nil {
		return fmt.Errorf("interactive 백엔드가 비활성화되어 있습니다 (config.yaml의 interactive_claude.enabled 확인)")
	}
	c, ok := m.store.GetWebConv(tgt.ID)
	if !ok {
		return fmt.Errorf("대화를 찾을 수 없습니다")
	}
	c.Interactive = on
	return m.store.UpdateWebConv(c)
}

// SetBackend switches the active AI backend and persists the choice. Returns an
// error if the requested backend is unavailable.
func (m *Manager) SetBackend(name string) error {
	return m.setBackend(name, true)
}

// setBackend switches the active backend. persist controls whether the choice is
// written to the store — startup restoration passes false so that falling back to
// an installed backend (when the preferred one is missing) never clobbers the
// user's saved preference.
func (m *Manager) setBackend(name string, persist bool) error {
	m.backendMu.Lock()
	defer m.backendMu.Unlock()
	switch name {
	case "claude":
		if m.claudeClient == nil {
			return fmt.Errorf("Claude가 설치되어 있지 않습니다")
		}
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
	if persist {
		if err := m.store.SetStoredBackend(name); err != nil {
			log.Printf("[manager] backend persist failed: %v", err)
		}
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

// scheduleWordRe matches an explicit request to schedule or be reminded.
var scheduleWordRe = regexp.MustCompile(`(?i)예약|스케줄|스케쥴|리마인|알림|크론|cron|schedule|remind`)

// scheduleTimeRe matches the temporal shapes a scheduling request takes: a
// recurrence ("매일", "every 5 minutes", "daily"), a delay ("30분 뒤", "in 2
// hours"), or a clock time ("14:30", "3시", "at 9am").
var scheduleTimeRe = regexp.MustCompile(`(?i)매\s*(일|주|시간|분|달|초)|내일|모레|이따|잠시\s*후|` +
	`\d+\s*(초|분|시간|일|주)\s*(뒤|후)|\d{1,2}\s*:\s*\d{2}|\d{1,2}\s*시\b|오전|오후|` +
	`\bdaily\b|\bhourly\b|\bweekly\b|\btomorrow\b|every\s+\d*\s*(second|minute|hour|day|week)|` +
	`\bin\s+\d+\s*(second|minute|hour|day|week)s?\b|\bat\s+\d{1,2}(:\d{2})?\s*(am|pm)\b`)

// mightBeScheduleRequest is the cheap gate in front of the manager LLM call.
//
// Every telegram message used to pay for one Route() round-trip whose only jobs
// were to spot a scheduling request and hand back a project hint — measured at 7
// to 20 seconds on the codex manager model, before the worker even started. The
// project hint is redundant (detectProjectSwitchIntent already resolves it
// locally), so the LLM is now only consulted when the text carries some surface
// sign of scheduling.
//
// It errs toward calling the LLM: a false positive merely costs what every
// message used to cost. A false negative means a scheduling request is answered
// as an ordinary turn — the user can rephrase, or use !task / !remind / !cron,
// which never went through this path.
func mightBeScheduleRequest(text string) bool {
	return scheduleWordRe.MatchString(text) || scheduleTimeRe.MatchString(text)
}

// detectProjectSwitchIntent returns a registered project name mentioned in text,
// preferring the longest match (so "voice-server" wins over "voice"). Case-
// insensitive. Deterministic; no LLM. Returns ("", false) when none match.
func detectProjectSwitchIntent(text string, projectNames []string) (string, bool) {
	lower := strings.ToLower(text)
	best := ""
	for _, name := range projectNames {
		if name == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(name)) && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// routeProjectOnly asks the manager LLM which project the message targets, used
// only as a fallback when keyword matching fails. Returns the project name if the
// LLM names a registered project, else ("", false).
func (m *Manager) routeProjectOnly(ctx context.Context, client ClaudeClient, text string) (string, bool) {
	req := m.buildRouteRequest(text)
	dec, err := client.Route(ctx, req)
	if err != nil {
		log.Printf("[manager] routeProjectOnly error: %v", err)
		return "", false
	}
	if dec.Project == "" {
		return "", false
	}
	if _, ok := m.store.GetProject(dec.Project); !ok {
		return "", false
	}
	return dec.Project, true
}

// projectNames returns the registered project names (unordered).
func projectNames(store StoreRepo) []string {
	projects := store.ListProjects()
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	return names
}

// Handle routes a free-text message to a worker. origin ("telegram"|"web") selects
// the channel: telegram is a single global conversation stream; web uses per-topic
// conversations (handleWeb, Task 5).
func (m *Manager) Handle(ctx context.Context, chatID int64, text, origin string, s MessageSender) {
	if origin == OriginWeb {
		m.handleWeb(ctx, chatID, text, s) // Task 5
		return
	}
	m.handleTelegram(ctx, chatID, text, s)
}

// handleTelegram continues the single global telegram conversation. A project-
// switch intent only moves the working-directory pointer; the conversation and
// its history stay one continuous stream. The telegram channel no longer does LLM
// project/conversation routing, but one manager LLM call still serves natural-
// language scheduling (and, incidentally, a project hint).
func (m *Manager) handleTelegram(ctx context.Context, chatID int64, text string, s MessageSender) {
	// Scope every output of this turn to the telegram stream.
	s = bindTarget(s, TelegramTarget())

	// Signal completion however this turn ends. runWorker signals its own, but
	// the early returns below (routing errors, a missing backend, a schedule
	// request) never reach it, and a web client that already showed its working
	// indicator would spin forever behind an answer that had arrived.
	defer s.Done(chatID)

	// Backend auto-switch pre-check (unchanged behavior).
	if target := detectBackendSwitchIntent(text); target != "" && target != m.Backend() {
		if err := m.SetBackend(target); err != nil {
			_ = s.Send(chatID, "⚠️ 백엔드 전환 실패: "+err.Error())
		} else {
			_ = s.Send(chatID, "🔄 백엔드 전환: "+strings.ToUpper(target))
		}
	}

	// The manager LLM call exists to honor a schedule request (and, incidentally,
	// to hint at a project). It is skipped unless the message looks like one:
	// otherwise every message paid 7-20s for a routing decision the local
	// detectors below already make. See mightBeScheduleRequest.
	hint := ""
	if mightBeScheduleRequest(text) {
		m.backendMu.RLock()
		routeClient := m.client
		m.backendMu.RUnlock()
		if dec, err := routeClient.Route(ctx, m.buildRouteRequest(text)); err == nil {
			if dec.Action == ActionSchedule {
				m.handleSchedule(chatID, dec, s)
				return
			}
			hint = dec.Project
		}
	}

	// Working directory: the active project's path if set & valid, else the service
	// home. No project is required — telegram always works, defaulting to home. A
	// switch intent (keyword, then the LLM hint) only moves the working-dir pointer;
	// the conversation stays one continuous stream.
	project := m.store.TelegramActiveProject()
	if sw, ok := detectProjectSwitchIntent(text, projectNames(m.store)); ok {
		if sw != project {
			_ = m.store.SetTelegramActiveProject(sw)
			_ = s.Send(chatID, "📂 이제 "+sw+"에서 진행합니다.")
		}
		project = sw
	} else if hint != "" {
		if _, exists := m.store.GetProject(hint); exists && hint != project {
			_ = m.store.SetTelegramActiveProject(hint)
			_ = s.Send(chatID, "📂 이제 "+hint+"에서 진행합니다.")
			project = hint
		}
	}
	// A stale active pointer (project since removed) must not reach runWorker, which
	// would error "프로젝트를 찾을 수 없습니다"; treat it as no project → run in home.
	if project != "" {
		if _, exists := m.store.GetProject(project); !exists {
			project = ""
		}
	}
	workDir := resolveHomeDir(m.cfg())
	if project != "" {
		if p, ok := m.store.GetProject(project); ok {
			workDir = p.Path
		}
	}
	tc := m.store.TelegramConversation()
	if tc.WorkDir != "" {
		workDir = tc.WorkDir
	}
	// The global telegram stream always follows the manager's live active backend
	// (set by !backend / detectBackendSwitchIntent), not a value stamped on the
	// conversation at creation time — otherwise a backend switch would report
	// success but silently keep routing turns through the old backend forever
	// (tc.Backend, once set, was never updated by SetBackend).
	// Interactive mode is deliberately not offered on the telegram stream: it is
	// one lane shared by construction (telegramMu below serializes every turn on
	// it end-to-end), so a steered second message would just queue behind
	// telegramMu.Lock() instead of reaching the live session concurrently —
	// silently defeating the point of steering. See IsInteractive/SetInteractive.
	backend := m.effectiveBackend(tc.Backend)
	client := m.clientForBackend(backend)
	if client == nil {
		_ = s.Send(chatID, fmt.Sprintf("⚠️ 텔레그램 대화는 %s로 생성됐는데 %s가 설치되어 있지 않습니다. `!backend`로 전환하거나 설치 후 다시 시도해 주세요.",
			strings.ToUpper(backend), strings.ToUpper(backend)))
		return
	}
	m.telegramMu.Lock()
	defer m.telegramMu.Unlock()
	m.runWorker(ctx, chatID, text, m.telegramSink(project), workDir, tc, s, client, backend)
}

// projectMemoryDirName picks the per-project memory directory for projectPath.
//
// New projects get .aglink/. Projects that already accumulated memory under the
// pre-rename .teleclaude/ keep using it in place — silently switching them to a
// fresh empty .aglink/ would drop every decision a conversation had recorded
// there. Move the directory by hand to opt a project into the new name.
func projectMemoryDirName(projectPath string) string {
	if projectPath != "" {
		if _, err := os.Stat(filepath.Join(projectPath, ".aglink")); os.IsNotExist(err) {
			if st, lerr := os.Stat(filepath.Join(projectPath, legacyDataDirName)); lerr == nil && st.IsDir() {
				return legacyDataDirName
			}
		}
	}
	return ".aglink"
}

// conversationMemoryPath returns the relative, conversation-scoped memory file
// path a Worker should read/write. Each conversation gets its own file under
// <dir>/memory/ instead of every conversation sharing one growing
// <dir>/memory.md — mixing unrelated topics into one file made it
// unreadable and caused conversations to leak context into each other
// (2026-07-16 decision). Pre-existing memory.md files are left on
// disk untouched as an archive; they are no longer read or written.
func conversationMemoryPath(projectPath, convID string) string {
	dir := projectMemoryDirName(projectPath)
	if convID == "" {
		return dir + "/memory.md"
	}
	return dir + "/memory/" + convID + ".md"
}

// compactPromptFor asks the Worker to externalize this conversation's durable
// facts into its own conversation-scoped memory file instead of relying on the
// CLI session's ever-growing --resume context to carry them. Mirrors the
// manual /compact command in devladpopov/aglink, which the project journal
// names as a gap worth closing (see the project memory file, 2026-07-09).
func compactPromptFor(projectPath, convID string) string {
	return fmt.Sprintf("지금까지의 대화에서 앞으로도 참고해야 할 핵심 결정사항/사실을 이 대화 전용 메모리 파일(%s)에 정리해서 저장해줘(파일이 없으면 새로 만들고, 이미 있는 내용과 중복되면 병합·정리). 다른 대화의 메모와 섞이지 않도록 이 경로만 사용해줘. 저장한 뒤에는 무엇을 저장했는지 3~5줄로 요약해서 답해줘.", conversationMemoryPath(projectPath, convID))
}

// CompactTelegramConversation runs one Worker turn asking Claude to persist the
// telegram stream's key decisions to its conversation-scoped memory file, then
// drops the local history mirror and the CLI SessionID so the next turn starts
// a fresh, cheap session instead of resuming an ever-growing one. The
// externalized memory file is already read into every Worker prompt
// (buildContextPrompt), so nothing is actually lost — just moved out of the
// expensive live session.
// State is left untouched if the compaction turn itself fails, so a failure
// never discards history that wasn't actually saved anywhere durable.
func (m *Manager) CompactTelegramConversation(ctx context.Context, chatID int64, s MessageSender) {
	m.telegramMu.Lock()
	defer m.telegramMu.Unlock()

	tc := m.store.TelegramConversation()
	// SessionID is pre-generated by TelegramConversation() even before the first
	// turn ever runs, so it can't signal "nothing to compact" — Started (set once
	// a turn actually completes) is the real indicator, same as isNewConv elsewhere.
	if len(tc.History) == 0 && !tc.Started {
		_ = s.Send(chatID, "압축할 대화 내용이 없습니다.")
		return
	}

	project := m.store.TelegramActiveProject()
	workDir := resolveHomeDir(m.cfg())
	if project != "" {
		if p, ok := m.store.GetProject(project); ok {
			workDir = p.Path
		}
	}
	if tc.WorkDir != "" {
		workDir = tc.WorkDir
	}
	workDir = m.validWorkDirOrHome(workDir)

	backend := m.effectiveBackend(tc.Backend)
	client := m.clientForBackend(backend)
	if client == nil {
		_ = s.Send(chatID, "⚠️ 백엔드를 사용할 수 없습니다.")
		return
	}

	s.Typing(chatID)
	_ = s.Send(chatID, "🗜 대화 압축 중...")

	res, err := client.Run(ctx, RunRequest{
		Prompt:    compactPromptFor(workDir, tc.ID),
		WorkDir:   workDir,
		SessionID: tc.SessionID,
		Resume:    tc.Started,
		Model:     m.workerModelForBackendName(backend),
	})
	if err != nil {
		_ = s.Send(chatID, "⚠️ 압축 실패: "+err.Error())
		return
	}

	tc.History = nil
	tc.SessionID = ""
	tc.Started = false
	tc.Summary = truncate(res.Text, 80)
	if uerr := m.store.UpdateTelegramConversation(tc); uerr != nil {
		log.Printf("[manager] compact: update telegram conv: %v", uerr)
	}

	_ = sendChunked(s, chatID, "✅ 압축 완료 — 다음 대화는 새 세션으로 시작합니다.\n\n"+res.Text)
}

// handleWeb handles a web send with no explicit target (legacy path): default to
// the telegram stream, the always-present conversation.
func (m *Manager) handleWeb(ctx context.Context, chatID int64, text string, s MessageSender) {
	m.HandleWebTarget(ctx, chatID, text, TelegramTarget(), s)
}

// HandleWebTarget routes a web send to its explicit target: the global telegram
// stream or a specific web topic. Web never does LLM project routing.
func (m *Manager) HandleWebTarget(ctx context.Context, chatID int64, text string, tgt Target, s MessageSender) {
	// Scope every output of this turn — replies, errors, typing, images — to the
	// conversation it belongs to. Without this a web topic's output fans out to
	// the global (Telegram) channels too, surfacing web conversations in Telegram.
	s = bindTarget(s, tgt)

	// Signal completion however this turn ends: the early returns below (unknown
	// conversation, missing backend) never reach runWorker, which is the only
	// other place that signals it.
	defer s.Done(chatID)

	if !tgt.IsWeb() {
		// Telegram stream: the default for kind "telegram", and for any unknown
		// or empty kind (e.g. a not-yet-updated web client). Working directory
		// resolution mirrors handleTelegram: the active project's path if set &
		// valid, else the service home. No project is required — this must
		// always run, never error, even with zero projects registered.
		project := m.store.TelegramActiveProject()
		// A stale active pointer (project since removed) must not reach
		// runWorker, which would error "프로젝트를 찾을 수 없습니다"; treat it as
		// no project → run in home.
		if project != "" {
			if _, exists := m.store.GetProject(project); !exists {
				project = ""
			}
		}
		workDir := resolveHomeDir(m.cfg())
		if project != "" {
			if p, ok := m.store.GetProject(project); ok {
				workDir = p.Path
			}
		}
		tc := m.store.TelegramConversation()
		if tc.WorkDir != "" {
			workDir = tc.WorkDir
		}
		// See handleTelegram: the global stream follows the live active backend,
		// not the conversation's creation-time stamp.
		backend := m.effectiveBackend(tc.Backend)
		client := m.clientForBackend(backend)
		if client == nil {
			_ = s.Send(chatID, fmt.Sprintf("⚠️ 텔레그램 대화는 %s로 생성됐는데 %s가 설치되어 있지 않습니다.", strings.ToUpper(backend), strings.ToUpper(backend)))
			return
		}
		m.telegramMu.Lock()
		defer m.telegramMu.Unlock()
		m.runWorker(ctx, chatID, text, m.telegramSink(project), workDir, tc, s, client, backend)
		return
	}

	// Web conversation target (top-level, project-independent).
	c, ok := m.store.GetWebConv(tgt.ID)
	if !ok {
		_ = s.Send(chatID, "웹 대화를 찾을 수 없습니다. 새 대화를 만들어 주세요.")
		return
	}
	backend := m.effectiveBackend(c.Backend)
	client := m.clientFor(backend, c.Interactive)
	if client == nil {
		_ = s.Send(chatID, fmt.Sprintf("⚠️ 이 대화는 %s로 만들어졌는데 %s가 설치되어 있지 않습니다.", strings.ToUpper(backend), strings.ToUpper(backend)))
		return
	}
	workDir := c.WorkDir
	if workDir == "" {
		workDir = resolveHomeDir(m.cfg())
	}
	_ = m.store.SetActive("", c.ID)
	m.runWorker(ctx, chatID, text, m.newWebConvSink(), workDir, c, s, client, backend)
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
	// Active is a routing hint for the telegram stream only, so it must reflect
	// telegram's own active project (TelegramActiveProject) — not the shared
	// store.Active pointer, which a concurrent web conversation can overwrite,
	// bleeding an unrelated channel's project into telegram's routing decision.
	active := ActiveRef{Project: m.store.TelegramActiveProject()}
	if tc := m.store.TelegramConversation(); tc != nil {
		active.ConversationID = tc.ID
	}
	return RouteRequest{Message: text, Projects: summaries, Active: active}
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
	// A continuation belongs to the same channel as its parent series.
	newC, err := m.store.NewConversation(project, fmt.Sprintf("%s (시리즈 %d)", baseTitle, seriesNum), parent.Origin)
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

// convSink abstracts where a worker turn persists its conversation: a project
// topic (web) or the global telegram conversation. This keeps runWorker unaware
// of the two storage locations.
type convSink interface {
	project() string                 // workdir/status/history-log scope
	label() string                   // channel/project label for display & history filing
	save(c *Conversation) error      // persist updated conversation
	setActive(c *Conversation) error // update the channel's active pointer
	makeContinuation(c *Conversation) (*Conversation, error)
}

type projectSink struct {
	m    *Manager
	proj string
}

func (p projectSink) project() string { return p.proj }
func (p projectSink) label() string   { return p.proj }
func (p projectSink) save(c *Conversation) error {
	return p.m.store.UpdateConversation(p.proj, c)
}
func (p projectSink) setActive(c *Conversation) error {
	return p.m.store.SetActive(p.proj, c.ID)
}
func (p projectSink) makeContinuation(c *Conversation) (*Conversation, error) {
	return p.m.makeContinuation(p.proj, c)
}

func (m *Manager) projectSink(project string) convSink { return projectSink{m: m, proj: project} }

// telegramSink persists the single global telegram conversation. `proj` is only
// the working-directory/history scope (the active project), never the owner of
// the conversation record. Continuation is an in-place session reset: the same
// record continues with a fresh CLI session, so the telegram stream stays one
// conversation to the user.
type telegramSink struct {
	m    *Manager
	proj string
}

func (t telegramSink) project() string { return t.proj }
func (t telegramSink) label() string   { return "telegram" }
func (t telegramSink) save(c *Conversation) error {
	return t.m.store.UpdateTelegramConversation(c)
}
func (t telegramSink) setActive(c *Conversation) error { return nil } // telegram active is the stream itself
func (t telegramSink) makeContinuation(c *Conversation) (*Conversation, error) {
	// In-place continuation: keep the same telegram record but drop the CLI
	// session so the next turn starts fresh with the carried summary. History is
	// preserved (capped), so the user still sees a single continuous stream.
	c.SessionID = newUUID()
	c.Started = false
	return c, nil
}

func (m *Manager) telegramSink(project string) convSink { return telegramSink{m: m, proj: project} }

// webConvSink persists a top-level, project-independent web conversation. It has
// no project scope (project()==""), so runWorker skips project lookup/memory and
// runs in the conversation's WorkDir (or the service home). Continuation is an
// in-place session reset, matching the telegram stream's single-record behavior.
type webConvSink struct {
	m *Manager
}

func (w webConvSink) project() string            { return "" }
func (w webConvSink) label() string              { return "web" }
func (w webConvSink) save(c *Conversation) error { return w.m.store.UpdateWebConv(c) }
func (w webConvSink) setActive(c *Conversation) error {
	return w.m.store.SetActive("", c.ID)
}
func (w webConvSink) makeContinuation(c *Conversation) (*Conversation, error) {
	c.SessionID = newUUID()
	c.Started = false
	return c, nil
}

func (m *Manager) newWebConvSink() convSink { return webConvSink{m: m} }

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
		strings.Contains(lower, "session not found") ||
		// codex, when the rollout behind a thread id is gone (pruned, or the
		// thread was first created before we stopped passing --ephemeral).
		strings.Contains(lower, "no rollout found")
}

// isAuthFailure detects a turn that produced no answer because the CLI could not
// authenticate — e.g. the OAuth access token in ~/.claude/.credentials.json
// expired and could not be refreshed. The CLI exits 0 and puts the message in
// the result body ("Failed to authenticate. API Error: 401 OAuth access token
// has expired. Re-authenticate to continue."), so without this check the worker
// records the error string as the assistant's answer, relays it to the user, and
// — worse — marks the conversation Started while no CLI session was ever
// created. Every later turn then resumes a session id that does not exist and
// dies with "No conversation found with session ID".
func isAuthFailure(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "failed to authenticate") ||
		strings.Contains(lower, "oauth access token has expired") ||
		strings.Contains(lower, "re-authenticate to continue") ||
		// codex reports an expired/absent login this way.
		strings.Contains(lower, "not logged in")
}

// workerModelForBackend returns the right model string based on the active backend.
func (m *Manager) workerModelForBackend() string {
	return m.workerModelForBackendName(m.Backend())
}

// workerModelForBackendName returns the worker model for a specific backend,
// independent of the globally active backend — so a conversation resumed on its
// own backend uses that backend's model, not the currently selected one.
func (m *Manager) workerModelForBackendName(backend string) string {
	if backend == "codex" {
		return m.cfg().CodexModel
	}
	return m.cfg().WorkerModel
}

// clientForBackend returns the CLI client for a backend name, or nil if that
// backend is not installed. Used to resume a conversation on the backend it was
// created with, regardless of the current global backend selection.
func (m *Manager) clientForBackend(name string) ClaudeClient {
	m.backendMu.RLock()
	defer m.backendMu.RUnlock()
	switch name {
	case "codex":
		return m.codexClient
	case "claude":
		return m.claudeClient
	default:
		return nil
	}
}

// turnDurationSamples caps the rolling window of completed-turn durations kept
// per backend for the startup estimate — recent conditions, not an
// all-time average that would drift stale as the workload changes.
const turnDurationSamples = 20

// recordTurnDuration appends a completed turn's wall-clock duration to the
// backend's rolling window. Called once a turn actually finishes (success or
// session-recovered success) — see the call site in runWorker.
func (m *Manager) recordTurnDuration(backend string, d time.Duration) {
	m.durationsMu.Lock()
	defer m.durationsMu.Unlock()
	if m.turnDurations == nil {
		m.turnDurations = make(map[string][]time.Duration)
	}
	ds := append(m.turnDurations[backend], d)
	if len(ds) > turnDurationSamples {
		ds = ds[len(ds)-turnDurationSamples:]
	}
	m.turnDurations[backend] = ds
}

// estimateTurnDuration returns the average of the backend's recent completed
// turns. ok is false with fewer than 3 samples — too little data to show
// without it reading as a made-up number. This is deliberately NOT a real
// ETA: there is no way to know upfront how many tool calls an agentic turn
// will take. It's an honest "here's roughly what recent turns have looked
// like" instead of the user waiting in total silence with no sense of scale.
func (m *Manager) estimateTurnDuration(backend string) (avg time.Duration, samples int, ok bool) {
	m.durationsMu.Lock()
	defer m.durationsMu.Unlock()
	ds := m.turnDurations[backend]
	if len(ds) < 3 {
		return 0, len(ds), false
	}
	var total time.Duration
	for _, d := range ds {
		total += d
	}
	return total / time.Duration(len(ds)), len(ds), true
}

// formatMinSec renders a duration as "N분 M초" (or "M초" under a minute),
// matching runHeartbeat's own elapsed-time formatting below.
func formatMinSec(d time.Duration) string {
	secs := int(d.Seconds())
	mins := secs / 60
	secs %= 60
	if mins == 0 {
		return fmt.Sprintf("%d초", secs)
	}
	return fmt.Sprintf("%d분 %d초", mins, secs)
}

// runHeartbeat sends a periodic "still going" text message every 2 minutes
// while a Worker turn is in flight, and — critically — also refreshes the
// channel's live "typing" signal on each tick (s.Typing), not just once at
// turn start. Without this, the web UI's "작업 진행 중" indicator was a client-
// side timer with no server-side confirmation the worker was still alive: a
// hung/dead worker and a slow-but-fine one looked identical until the turn
// eventually returned (or timed out). label distinguishes the normal run
// ("작업 진행 중") from a post-session-loss retry ("세션 복구 진행 중").
//
// When timeoutMinutes is set, the last heartbeat before the deadline switches
// to an explicit warning instead of the routine message, so a slow turn gets
// a heads-up (and a chance to !cancel) before the hard timeout silently kills
// it — rather than the user only finding out after the fact.
func runHeartbeat(s MessageSender, chatID int64, label string, startTime time.Time, timeoutMinutes int, done <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	var deadline time.Time
	if timeoutMinutes > 0 {
		deadline = startTime.Add(time.Duration(timeoutMinutes) * time.Minute)
	}
	for {
		select {
		case <-ticker.C:
			s.Typing(chatID) // refresh the live-signal independent of the text bubble below
			elapsed := time.Since(startTime)
			mins, secs := int(elapsed.Minutes()), int(elapsed.Seconds())%60
			msg := fmt.Sprintf("⏳ %s... (%d분 %d초 경과)", label, mins, secs)
			if !deadline.IsZero() {
				if remaining := time.Until(deadline); remaining > 0 && remaining <= 2*time.Minute {
					msg = fmt.Sprintf("⏳ %s... (%d분 %d초 경과) — 곧 제한 시간(%d분)에 도달합니다. 계속 기다리는 중이며, 그만두려면 !cancel",
						label, mins, secs, timeoutMinutes)
				}
			}
			_ = s.Send(chatID, msg)
		case <-done:
			return
		}
	}
}

// validWorkDirOrHome falls back to the service home when workDir no longer
// exists as a directory. A project/conversation WorkDir persisted earlier can
// go stale (folder moved, deleted, or never existed on this machine — e.g. a
// store.json carried over from another PC). Passing a missing directory as
// cmd.Dir makes Windows' CreateProcess fail with the cryptic "The directory
// name is invalid.", which then surfaces as an opaque "OO worker 실행 실패"
// with no clue it's a path problem — fail safe into the home dir instead of
// erroring the whole turn.
func (m *Manager) validWorkDirOrHome(workDir string) string {
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		log.Printf("[manager] workDir %q missing/invalid (%v) — falling back to home", workDir, err)
		return resolveHomeDir(m.cfg())
	}
	return workDir
}

// runWorker executes the Worker turn for a resolved (project, conversation) and relays output.
// workDir overrides the project's path as the Claude CLI working directory (e.g. a git worktree).
// Pass "" to use the project's registered path.
// backend names the AI backend this turn runs on and MUST match `client`. It is
// passed explicitly (rather than read from the global active backend) so a
// conversation resumed on its own backend uses the right model, logging, and
// continuation tagging even when the global backend differs.
// recordCompletedTurn fills in the response on the pending prompt-only turn at
// pendingIdx (persisted when the run started) by replacing it in place, so the
// turn isn't duplicated. If that slot no longer holds the matching pending turn
// (e.g. dropped by the history cap or shifted by a concurrent turn), it appends
// a fresh completed turn instead.
func recordCompletedTurn(history []ConversationTurn, pendingIdx int, prompt, response string, now time.Time) []ConversationTurn {
	completed := ConversationTurn{Timestamp: now, Prompt: prompt, Response: response}
	if pendingIdx >= 0 && pendingIdx < len(history) &&
		history[pendingIdx].Response == "" && history[pendingIdx].Prompt == prompt {
		history[pendingIdx] = completed
		return history
	}
	return append(history, completed)
}

// screenOwnerLabel builds the AGLINK_OWNER_LABEL for a worker turn so the
// aglink-screen control lease can name which conversation currently holds the
// screen in its SCREEN_BUSY / control_status messages (see aglink-screen
// docs/control-ownership.md §5). A nil/empty conversation falls back to the chat
// id alone.
func screenOwnerLabel(chatID int64, c *Conversation) string {
	if c != nil && c.ID != "" {
		return fmt.Sprintf("chat:%d/conv:%s", chatID, c.ID)
	}
	return fmt.Sprintf("chat:%d", chatID)
}

func (m *Manager) runWorker(ctx context.Context, chatID int64, text string, sink convSink, workDir string, c *Conversation, s MessageSender, client ClaudeClient, backend string) {
	// Signal turn completion on every exit path (success, error, timeout, or the
	// early "project not found" return) so channels with a live "working"
	// indicator (web chat) always get a matching Done for their Typing.
	defer s.Done(chatID)

	// project scopes workdir/status/history-log. For a top-level web conversation
	// (webConvSink) project()=="" — there is no registered project to look up, so
	// skip GetProject and fall back to the passed workDir / the service home.
	project := sink.project()
	var pPath string
	if project != "" {
		p, ok := m.store.GetProject(project)
		if !ok {
			_ = s.Send(chatID, "⚠️ 프로젝트를 찾을 수 없습니다: "+project)
			return
		}
		pPath = p.Path
	}
	if workDir == "" {
		workDir = pPath
	}
	if workDir == "" {
		workDir = resolveHomeDir(m.cfg())
	}
	workDir = m.validWorkDirOrHome(workDir)

	// Forward tool images (screen MCP screenshot/capture_window/capture_region) to
	// the chat. Only when screen control is on (images come only from those tools)
	// and the sender can send photos. Setting OnImage makes the worker stream NDJSON
	// so tool_result image blocks can be recovered — the final envelope drops them.
	var onImage func(png []byte, caption string)
	if m.cfg().ScreenControl {
		if ps, ok := s.(interface {
			SendPhoto(int64, []byte, string) error
		}); ok {
			onImage = func(png []byte, caption string) { _ = ps.SendPhoto(chatID, png, caption) }
		}
	}

	// Forward live tool-use progress lines (e.g. "🔧 Bash: go test ./...") to
	// channels with a progress view (aglink-desktop/aglink-chat control API;
	// Telegram no-ops). Setting OnProgress makes the worker stream NDJSON, same
	// as OnImage above — both flags share the one streaming code path in
	// runner.go, so turning this on doesn't cost a second CLI invocation.
	var onProgress func(msg string)
	if ps, ok := s.(interface {
		Progress(int64, string)
	}); ok {
		onProgress = func(msg string) { ps.Progress(chatID, msg) }
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
		if newC, err := sink.makeContinuation(c); err != nil {
			log.Printf("[manager] auto-continuation failed: %v", err)
		} else {
			newC.Backend = backend
			parentSummary = summary
			workConv = newC
			// Internal context-length split: the conversation continues seamlessly
			// (summary carried forward), so don't expose the series boundary to the
			// user — just log it for debugging.
			log.Printf("[manager] context length → new series (conv %s → %s)", c.ID, newC.ID)
		}
	}

	s.Typing(chatID)
	isNewConv := !workConv.Started
	// Show the "📂 project · 💬 conversation" header only when a genuinely new topic
	// begins — never on a resume, and never for an internal continuation (a
	// context-length series split). This keeps Telegram feeling like one continuous
	// chat while the backend still manages conversations/series behind the scenes.
	if isNewConv && !workConv.IsContinuation {
		headerProject := project
		if headerProject == "" {
			// No project scope: label by the actual channel (telegram/web), not a
			// hardcoded "웹" — a telegram turn with no active project must show
			// "telegram", never "web".
			headerProject = sink.label()
		}
		_ = s.Send(chatID, routingHeader(headerProject, workConv.Title, isNewConv))
	}

	// Record Worker status as running
	_ = m.workerStatus.SetStatus(WorkerStatus{
		Project:        project,
		ConversationID: workConv.ID,
		Title:          workConv.Title,
		Status:         "running",
		StartTime:      time.Now(),
	})

	startTime := time.Now()
	// Gate the turn on backend CLI readiness before any "starting" signal or
	// heartbeat. Only codexRunner implements backendReadiness: an older codex-cli
	// missing --ignore-user-config can't run a clean turn, so block with upgrade
	// guidance instead of dead-ending mid-turn; on the first ready codex turn,
	// surface a one-time heads-up naming the detected version. Claude-backed
	// turns don't implement the interface and skip this entirely.
	if rc, ok := client.(backendReadiness); ok {
		proceed, msg := rc.CheckReadiness()
		if !proceed {
			log.Printf("[worker] ✗ backend=%s blocked: CLI not ready", backend)
			if msg != "" {
				_ = s.Send(chatID, msg)
			}
			_ = m.workerStatus.UpdateStatus(project, workConv.ID, "failed", "backend not ready")
			return
		}
		if msg != "" {
			_ = s.Send(chatID, msg)
		}
	}

	// Give the user a sense of scale before the wait starts, not just silence
	// until the (up to 2-minute-away) first heartbeat tick — a turn that
	// finishes in 90s currently produces zero progress signal at all.
	if avg, samples, ok := m.estimateTurnDuration(backend); ok {
		_ = s.Send(chatID, fmt.Sprintf("⏳ 작업 시작 (최근 %d건 평균 %s)", samples, formatMinSec(avg)))
	}
	timeoutMinutes := m.cfg().TimeoutMinutes
	heartbeatDone := make(chan struct{})
	go runHeartbeat(s, chatID, "작업 진행 중", startTime, timeoutMinutes, heartbeatDone)

	// Pass history in the prompt sized to whether the CLI carries the session.
	// A resuming turn only needs a short trailing reminder (the CLI holds the
	// rest); a non-resuming turn — a fresh conversation, or the session-loss
	// recovery below — gets a much larger, char-bounded slice, because then the
	// stored history is the only context there is. historyForContext(...,
	// workConv.Started): Started is true exactly when a CLI session should exist.
	historyForPrompt := historyForContext(workConv.History, workConv.Started)
	globalMemory := readGlobalMemory()
	projectMemory := ""
	if pPath != "" {
		projectMemory = readProjectMemory(pPath, workConv.ID)
	}
	memPath := conversationMemoryPath(pPath, workConv.ID)
	prompt := buildContextPrompt(text, parentSummary, globalMemory, projectMemory, memPath, historyForPrompt)

	workerModel := m.workerModelForBackendName(backend)
	log.Printf("[worker] ▶ backend=%s model=%q project=%s conv=%s resume=%v prompt=%d chars",
		backend, workerModel, project, workConv.ID, workConv.Started, len(prompt))

	// sessionRecovered records that this turn ran as a fresh CLI session after the
	// stored one turned out to be gone. The persistence guard below skips a
	// SessionID on an already-started conversation — right for a resumed turn,
	// which returns none — but a recovered turn returns a *new* id that must
	// replace the dead one, or every later turn repeats the failed resume.
	sessionRecovered := false

	// Persist the user's prompt immediately (response filled in at completion),
	// so it survives a web reload/reconnect during the — possibly minutes-long —
	// run. Without this the turn only becomes durable when it completes, and a
	// reload in between re-fetches /api/history without the just-sent message and
	// wipes the client's optimistic echo (see aglink-chat selectTarget/
	// replaceChildren). historyForPrompt above was already built, so this pending
	// turn is not double-counted in the LLM context.
	pendingIdx := len(workConv.History)
	workConv.History = append(workConv.History, ConversationTurn{
		Timestamp: time.Now().UTC(),
		Prompt:    text,
	})
	if err := sink.save(workConv); err != nil {
		log.Printf("[manager] persist pending prompt: %v", err)
	}

	res, err := client.Run(ctx, RunRequest{
		Prompt:     prompt,
		WorkDir:    workDir,
		SessionID:  workConv.SessionID,
		Resume:     workConv.Started,
		Model:      workerModel,
		OnImage:    onImage,
		OnProgress: onProgress,
		OwnerLabel: screenOwnerLabel(chatID, workConv),
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
			// Session store was lost (bot restart / CLI update / rollout pruned /
			// backend switch). Retry as a fresh session. The first prompt only
			// carried a 3-turn reminder because the CLI was supposed to hold the
			// rest — but it doesn't now, so rebuild the prompt with a much larger,
			// char-bounded slice of the stored history, or the conversation
			// "forgets" everything older than three turns. workConv.History[:
			// pendingIdx] is the history as it stood before this turn's own prompt
			// was appended — the same view the first build used. Internal
			// recovery; don't tell the user.
			log.Printf("[worker] session lost (%v) — retrying once without --resume, with fuller history", err)
			sessionRecovered = true
			recoveryHistory := historyForContext(workConv.History[:pendingIdx], false)
			recoveryPrompt := buildContextPrompt(text, parentSummary, globalMemory, projectMemory, memPath, recoveryHistory)
			// The retry is a full fresh turn (may take a while); keep a heartbeat
			// alive for it — the original one was already closed above.
			recoverDone := make(chan struct{})
			go runHeartbeat(s, chatID, "세션 복구 진행 중", startTime, timeoutMinutes, recoverDone)
			res, err = client.Run(ctx, RunRequest{
				Prompt:     recoveryPrompt,
				WorkDir:    workDir,
				SessionID:  workConv.SessionID,
				Resume:     false,
				Model:      workerModel,
				OnImage:    onImage,
				OnProgress: onProgress,
				OwnerLabel: screenOwnerLabel(chatID, workConv),
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
	m.recordTurnDuration(backend, elapsed)

	// The CLI can fail to authenticate and still exit 0, putting the 401 in the
	// result body. Bail out before the turn is recorded and before Started is
	// set: the run produced no answer and no CLI session, so treating it as a
	// normal turn would both relay the raw error as if it were the assistant's
	// reply and leave the conversation pinned to a session id that never
	// existed — every later turn would then die on --resume.
	if isAuthFailure(res.Text) {
		log.Printf("[worker] ✗ backend=%s auth failure after %s: %s", backend, elapsed, strings.TrimSpace(res.Text))
		_ = s.Send(chatID, "⚠️ 작업 실패: Claude CLI 인증 만료 — 토큰을 갱신해 주세요.\n"+
			"`claude setup-token` 으로 발급한 값을 config의 CLAUDE_CODE_OAUTH_TOKEN 에 넣고 재시작하면 됩니다.")
		_ = m.workerStatus.UpdateStatus(project, workConv.ID, "failed", "auth: "+strings.TrimSpace(res.Text))
		return
	}

	// Reactive: if Worker hit Claude's context limit, auto-create continuation and retry once.
	if res.IsError && isContextOverflow(res.Text) {
		overflowSummary := workConv.Summary
		if overflowSummary == "" {
			overflowSummary = "이전 대화 내용을 참고해 주세요."
		}
		if newC, cerr := sink.makeContinuation(workConv); cerr == nil {
			newC.Backend = backend
			workConv = newC
			// The continuation is a new conversation with its own memory file — the
			// old conv's projectMemory doesn't belong under the new conv's ID.
			if pPath != "" {
				projectMemory = readProjectMemory(pPath, workConv.ID)
			}
			// Reactive context-overflow split: continue seamlessly in a new series
			// (summary carried); keep it internal — logged below, not sent to the user.
			retryPrompt := buildContextPrompt(text, overflowSummary, globalMemory, projectMemory, memPath, nil)
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
				Prompt:     retryPrompt,
				WorkDir:    workDir,
				SessionID:  workConv.SessionID,
				Resume:     false,
				Model:      workerModel,
				OnImage:    onImage,
				OnProgress: onProgress,
				OwnerLabel: screenOwnerLabel(chatID, workConv),
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

	// Persist conversation progress and history.
	wasStarted := workConv.Started
	workConv.Started = true
	workConv.LastActivity = time.Now().UTC()
	if res.Text != "" {
		workConv.Summary = truncate(res.Text, 80)
	}

	// Append this turn to conversation history. This cap is a storage/display
	// limit only — it does NOT affect LLM prompt size, which is separately
	// (and much more tightly) bounded by maxHistoryInPrompt below. Web chat's
	// /api/history reads this same array (store.go HistorySnapshot), so a small
	// cap here made old turns silently vanish from the web view (while still
	// visible in Telegram's own server-side scrollback) well before it mattered
	// for prompt size — kept generous so that doesn't happen in normal use.
	const maxHistoryTurns = 200
	// Fill in the response on the pending turn persisted before the run (update in
	// place), or append a completed turn if that slot no longer holds it.
	workConv.History = recordCompletedTurn(workConv.History, pendingIdx, text, res.Text, time.Now().UTC())
	if len(workConv.History) > maxHistoryTurns {
		dropped := len(workConv.History) - maxHistoryTurns
		workConv.History = workConv.History[dropped:]
		// The cap silently dropped visible history once before (see the comment
		// above) without so much as a log line, so a user report was the only way
		// to notice it happened at all. Logging it costs nothing and means the
		// next occurrence is visible in the server log instead of a surprise.
		log.Printf("[manager] conv %s history capped at %d turns, dropped %d oldest", workConv.ID, maxHistoryTurns, dropped)
	}
	if res.SessionID != "" && (!wasStarted || sessionRecovered) {
		workConv.SessionID = res.SessionID
	}

	if err := sink.save(workConv); err != nil {
		log.Printf("[manager] update conversation: %v", err)
	}
	if err := sink.setActive(workConv); err != nil {
		log.Printf("[manager] set active: %v", err)
	}

	// Send only after the turn is durably in workConv.History and saved above —
	// otherwise a web client's concurrent /api/history read (e.g. on page
	// refresh/reconnect) can land in between and fetch a snapshot that doesn't
	// yet include the turn it just watched stream in live, silently losing it
	// once the client's history reload replaces the DOM (see aglink-chat's
	// selectTarget/replaceChildren).
	_ = sendChunked(s, chatID, res.Text)

	// Append to date-based history log for !history command. When there's no
	// project scope (project==""), which would make WriteHistory write to the
	// history root, fall back to the sink's channel label ("telegram" or "web")
	// so those turns land in a dedicated per-channel folder instead — never
	// mislabeling a telegram-no-project turn as "web".
	if res.Text != "" {
		historyLabel := project
		if historyLabel == "" {
			historyLabel = sink.label()
		}
		if herr := WriteHistory(historyLabel, workConv.Title, text, res.Text); herr != nil {
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

// ActiveWorkers returns the workers still running. Clients poll this to
// reconcile a "working" indicator that a lost or out-of-order Done frame would
// otherwise leave spinning: a conversation absent from this list is not busy.
func (m *Manager) ActiveWorkers() []WorkerStatus {
	return m.workerStatus.ListActive()
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

// readProjectMemory reads this conversation's own memory file
// (<memory dir>/memory/<convID>.md) from the project directory. Worker Claude
// can freely update this file to persist decisions specific to this
// conversation — it is not shared with other conversations in the same
// project (see conversationMemoryPath).
func readProjectMemory(projectPath, convID string) string {
	if convID == "" {
		return ""
	}
	dir := projectMemoryDirName(projectPath)
	b, err := os.ReadFile(filepath.Join(projectPath, dir, "memory", convID+".md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readGlobalMemory reads <data dir>/global-memory.md for cross-project long-term memory.
func readGlobalMemory() string {
	dir, err := dataDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "global-memory.md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

const (
	// maxHistoryInPromptResume: when the CLI session carries the full
	// conversation server-side (a --resume / codex-resume turn), only a short
	// trailing reminder is inlined, so the prompt doesn't grow every turn.
	maxHistoryInPromptResume = 3
	// maxHistoryOnRecovery / maxHistoryCharsOnRecovery: when there is NO
	// server-side session — a fresh run, or a session-loss recovery after the
	// CLI's rollout was pruned / expired / a bot restart / a backend switch —
	// the stored history is the only context the model has. Inline much more of
	// it, bounded by a char budget so a long conversation can't produce an
	// oversized prompt (see the past "Request too large" incident).
	maxHistoryOnRecovery      = 40
	maxHistoryCharsOnRecovery = 24000
)

// historyForContext picks how much stored history to inline in a worker prompt.
// resume=true (the CLI already holds the conversation) → a short reminder;
// resume=false (fresh or recovering, the CLI holds nothing) → as much recent
// history as the budget allows, because that store IS the context now.
func historyForContext(history []ConversationTurn, resume bool) []ConversationTurn {
	if resume {
		return tailTurns(history, maxHistoryInPromptResume, 0)
	}
	return tailTurns(history, maxHistoryOnRecovery, maxHistoryCharsOnRecovery)
}

// tailTurns returns the last turns of history: at most maxTurns, and — when
// maxChars > 0 — trimmed from the front so the inlined prompt+response text
// stays within maxChars. The most recent turn is always kept even if it alone
// exceeds the budget.
func tailTurns(history []ConversationTurn, maxTurns, maxChars int) []ConversationTurn {
	n := len(history)
	if maxTurns <= 0 || n == 0 {
		return nil
	}
	chars := 0
	start := n
	for start > 0 && n-start < maxTurns {
		t := history[start-1]
		if maxChars > 0 {
			chars += len(t.Prompt) + len(t.Response)
			if chars > maxChars && start < n {
				break // budget exceeded; keep what fits (at least the last turn)
			}
		}
		start--
	}
	return history[start:]
}

// buildContextPrompt assembles the Worker prompt from available context layers.
// Layer order: global memory → project memory → parent summary → recent history → current request.
// Only adds sections when content exists — no empty headers. convID scopes the
// trailing memory-save reminder to this conversation's own memory file.
func buildContextPrompt(currentPrompt, parentSummary, globalMemory, projectMemory, memoryPath string, history []ConversationTurn) string {
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
		sb.WriteString("## 이 대화의 메모리\n\n")
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
	fmt.Fprintf(&sb, "\n\n> 중요한 결정/해결책은 이 대화 전용 메모리 파일(%s)에 기록해두세요. 다른 대화의 메모와 섞이지 않도록 이 경로만 사용하세요.", memoryPath)
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

	// Pin the task to telegram's own active project now, at creation time — not
	// the shared store.Active pointer, which a concurrent web conversation can
	// overwrite before this task ever fires (see HandleScheduledTask).
	project := m.store.TelegramActiveProject()

	// If LLM returned a 5-field cron expression (or @-shorthand), use AddTask directly.
	if isCronExpr(dec.ScheduleInterval) {
		kind := "알림"
		if dec.ScheduleIsTask {
			kind = "Claude 작업"
		}
		t := &Task{
			ID:        newTaskID(),
			ChatID:    chatID,
			Project:   project,
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
		r, err := m.scheduler.AddReminder(chatID, project, dec.ScheduleTask, timeNow().Add(dur))
		if err != nil {
			_ = s.Send(chatID, "⚠️ 알림 등록 실패: "+err.Error())
			return
		}
		// label describes a recurrence ("45분마다"); a reminder fires once.
		_ = s.Send(chatID, fmt.Sprintf("✅ 알림 등록 [%s] — %s 후\n  %s", r.ID, humanDelay(dur), dec.ScheduleTask))
	case "cron":
		c, err := m.scheduler.AddCron(chatID, project, label, dur, dec.ScheduleTask, dec.ScheduleIsTask)
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
// project is the project pinned on the Task at creation time (see
// handleSchedule) — not re-resolved from the shared store.Active pointer,
// which a concurrent, unrelated channel could have overwritten by fire time.
func (m *Manager) HandleScheduledTask(ctx context.Context, chatID int64, text, project string, s MessageSender) {
	m.backendMu.RLock()
	currentBackend := m.backendName
	currentClient := m.client
	m.backendMu.RUnlock()

	projects := m.store.ListProjects()
	if len(projects) == 0 {
		_ = s.Send(chatID, "⚠️ 예약 작업 실행 실패: 등록된 프로젝트가 없습니다. !project add <이름> <경로>")
		return
	}

	// Prefer the task's pinned project; fall back to alphabetically first to ensure determinism.
	projectName := project
	if _, ok := m.store.GetProject(projectName); !ok {
		names := make([]string, 0, len(projects))
		for name := range projects {
			names = append(names, name)
		}
		sort.Strings(names)
		projectName = names[0]
	}

	c, err := m.store.NewConversation(projectName, "📅 "+truncate(text, 28), OriginTelegram)
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
	m.runWorker(ctx, chatID, text, m.projectSink(projectName), workDir, c, s, currentClient, currentBackend)
}

// timeNow is a replaceable clock for testing.
var timeNow = time.Now
