package main

import (
	"context"
	"time"
)

// Design Ref: §3.1 — Domain types. Plan SC: 멀티프로젝트 × 프로젝트별 다중 대화.

// Config holds runtime settings loaded from %USERPROFILE%\.teleclaude\config.txt.
type Config struct {
	TelegramBotToken      string
	AllowedUserIDs        []int64
	ManagerModel          string   // default "haiku"
	WorkerModel           string   // "" = claude default
	ClaudePath            string   // "" = auto-detect
	ClaudeOauthToken      string   // CLAUDE_CODE_OAUTH_TOKEN injected into worker env ("" = use claude's own login)
	TimeoutMinutes        int      // default 10
	ManagerAlways         bool     // default true (route every text via manager)
	CodexPath             string   // "" = auto-detect
	CodexModel            string   // worker model (powerful) — "" = codex built-in default
	CodexManagerModel     string   // routing model (fast/cheap) — "" = same as CodexModel
	DefaultBackend        string   // "claude" | "codex" — "" = "claude"
	MaxWorkers            int      // max concurrent Worker goroutines, default 3
	RateLimitPerMin       int      // max user messages per minute, 0 = unlimited, default 20
	AllowScripts          bool     // permit --script in !task add/update, default false
	AllowedScriptCommands []string // whitelist of allowed script first-tokens; empty = any
	AllowedUsernames      []string // Telegram usernames (without @) allowed to use the bot
	ScreenControl         bool     // screen-control MCP 활성화 (Windows). 기본 false
	ScreenPresetsFile     string   // 좌표 프리셋 파일 경로. 빈 값이면 ~/.teleclaude/presets.json
	ScreenElevated        bool     // 관리자 권한으로 실행해 관리자 대상 앱도 제어 (Windows UIPI 우회). 기본 false
	ConversationTTLDays   int      // 이 기간(일) 동안 활동 없는 대화/히스토리 파일을 자동 정리. 0 = 비활성화, 기본 30
}

// ConversationTurn represents one exchange in a conversation.
type ConversationTurn struct {
	Timestamp time.Time `json:"timestamp"`
	Prompt    string    `json:"prompt"`   // user input
	Response  string    `json:"response"` // claude output
}

// Conversation is one topic within a project; maps 1:1 to a claude session.
// Design Ref: §3.1 — SessionID is a UUID we generate (--session-id first turn, --resume after).
// ParentID chains conversations when context grows too large (auto-continuation).
type Conversation struct {
	ID             string             `json:"id"`
	Title          string             `json:"title"`
	Summary        string             `json:"summary"`
	SessionID      string             `json:"sessionId"` // UUID assigned at creation
	Started        bool               `json:"started"`   // false until first worker turn completes
	LastActivity   time.Time          `json:"lastActivity"`
	History        []ConversationTurn `json:"history"`                  // conversation turns for context preservation
	ParentID       string             `json:"parentId,omitempty"`       // ID of previous conversation in chain
	ChildID        string             `json:"childId,omitempty"`        // ID of next conversation in chain
	IsContinuation bool               `json:"isContinuation,omitempty"` // auto-generated continuation
	Backend        string             `json:"backend,omitempty"`        // "claude"|"codex"|"" (""=claude)
}

// Project is a registered directory holding multiple conversations.
type Project struct {
	Path          string                   `json:"path"`
	Conversations map[string]*Conversation `json:"conversations"`
}

// ActiveRef points at the current project/conversation (fallback + manual switching).
type ActiveRef struct {
	Project        string `json:"project"`
	ConversationID string `json:"conversationId"`
}

// StoreData is the root persisted to store.json (별도 저장소).
type StoreData struct {
	Projects      map[string]*Project `json:"projects"`
	Active        ActiveRef           `json:"active"`
	ActiveBackend string              `json:"activeBackend,omitempty"` // "claude"|"codex"; "" means claude
}

// --- Manager routing I/O (Design §3.2) ---

type ConversationSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

type ProjectSummary struct {
	Name          string                `json:"name"`
	Conversations []ConversationSummary `json:"conversations"`
}

type RouteRequest struct {
	Message  string
	Projects []ProjectSummary
	Active   ActiveRef
}

// RouteDecision is the structured output the Manager (claude) must return.
type RouteDecision struct {
	Project        string  `json:"project"`
	ConversationID string  `json:"conversationId"`
	Action         string  `json:"action"` // "resume" | "new" | "clarify" | "status" | "schedule"
	NewTitle       string  `json:"newTitle"`
	Clarify        string  `json:"clarify"`
	Confidence     float64 `json:"confidence"`

	// Schedule fields — only set when action == "schedule"
	ScheduleType     string `json:"scheduleType,omitempty"`     // "remind" | "cron"
	ScheduleInterval string `json:"scheduleInterval,omitempty"` // "30m", "2h", "hourly", "daily" …
	ScheduleTask     string `json:"scheduleTask,omitempty"`     // message or Claude prompt
	ScheduleIsTask   bool   `json:"scheduleIsTask,omitempty"`   // true → dispatch through Worker
}

// Task is a unified scheduled item replacing Reminder and CronJob.
// CronExpr != "" → recurring (robfig/cron/v3 syntax, e.g. "0 9 * * 1-5").
// CronExpr == "" → one-shot (FireAt used).
// Status: "pending" | "paused" | "cancelled"
type Task struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chatId"`
	Prompt    string    `json:"prompt"`
	Script    string    `json:"script,omitempty"`   // bash pre-check; empty = skip
	CronExpr  string    `json:"cronExpr,omitempty"` // standard 5-field cron
	FireAt    time.Time `json:"fireAt,omitempty"`   // one-shot: when to fire
	Status    string    `json:"status"`
	IsTask    bool      `json:"isTask"` // true = Claude Worker, false = notify
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
	LastFired time.Time `json:"lastFired,omitempty"`
	DependsOn []string  `json:"dependsOn,omitempty"` // task IDs that must complete before this fires
}

// Action constants.
const (
	ActionResume   = "resume"
	ActionNew      = "new"
	ActionClarify  = "clarify"
	ActionStatus   = "status"
	ActionSchedule = "schedule"
)

// --- Worker run I/O (Design §3.3) ---

type RunRequest struct {
	Prompt    string
	WorkDir   string
	SessionID string // UUID
	Resume    bool   // true → --resume, false → --session-id
	Model     string

	// OnProgress, when non-nil, requests realtime NDJSON streaming from the
	// backend and is called with a short human-readable line for each tool-use
	// event as it happens (e.g. "🔧 Bash: go test ./..."). Optional — nil means
	// the backend runs its normal single-envelope turn. Currently only honored
	// by claudeRunner; codexRunner ignores it (codex already streams JSONL events
	// via logCodexEvent).
	OnProgress func(string)
}

type RunResult struct {
	Text      string
	IsError   bool
	SessionID string // non-empty only on first codex turn (thread_id from JSONL)
}

// --- Interfaces (Design §4.1, Option C boundaries) ---

// ClaudeClient abstracts the local `claude` CLI for both Manager routing and Worker execution.
type ClaudeClient interface {
	Route(ctx context.Context, req RouteRequest) (RouteDecision, error)
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// --- Worker status tracking (real-time monitoring) ---

// WorkerStatus tracks the state of a running or completed Worker task.
type WorkerStatus struct {
	Project        string    // project name
	ConversationID string    // conversation ID
	Title          string    // conversation title for display
	Status         string    // "running" | "completed" | "failed" | "timeout"
	StartTime      time.Time // when the worker started
	EndTime        time.Time // when the worker finished (zero if still running)
	Error          string    // error message if failed
}

// WorkerStatusStore tracks all active and recent Workers.
type WorkerStatusStore interface {
	GetStatus(project, convID string) (WorkerStatus, bool)
	SetStatus(status WorkerStatus) error
	ListActive() []WorkerStatus          // return workers that are still running
	ListRecent(limit int) []WorkerStatus // return last N completed workers
	UpdateStatus(project, convID string, newStatus, errorMsg string) error
}

// StoreRepo abstracts the conversation store (JSON for MVP, SQLite later).
type StoreRepo interface {
	Load() error
	Save() error
	ListProjects() map[string]*Project
	AddProject(name, path string) error
	RemoveProject(name string) error
	GetProject(name string) (*Project, bool)
	NewConversation(project, title string) (*Conversation, error)
	GetConversation(project, convID string) (*Conversation, bool)
	UpdateConversation(project string, c *Conversation) error
	SetActive(project, convID string) error
	GetActive() ActiveRef
	GetParent(project, convID string) (*Conversation, bool)
	GetStoredBackend() string
	SetStoredBackend(name string) error
}
