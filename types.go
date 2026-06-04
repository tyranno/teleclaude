package main

import (
	"context"
	"time"
)

// Design Ref: §3.1 — Domain types. Plan SC: 멀티프로젝트 × 프로젝트별 다중 대화.

// Config holds runtime settings loaded from %USERPROFILE%\.teleclaude\config.txt.
type Config struct {
	TelegramBotToken string
	AllowedUserIDs   []int64
	ManagerModel     string // default "haiku"
	WorkerModel      string // "" = claude default
	ClaudePath       string // "" = auto-detect
	TimeoutMinutes   int    // default 10
	ManagerAlways    bool   // default true (route every text via manager)
}

// ConversationTurn represents one exchange in a conversation.
type ConversationTurn struct {
	Timestamp time.Time `json:"timestamp"`
	Prompt    string    `json:"prompt"`    // user input
	Response  string    `json:"response"`  // claude output
}

// Conversation is one topic within a project; maps 1:1 to a claude session.
// Design Ref: §3.1 — SessionID is a UUID we generate (--session-id first turn, --resume after).
// ParentID chains conversations when context grows too large (auto-continuation).
type Conversation struct {
	ID             string              `json:"id"`
	Title          string              `json:"title"`
	Summary        string              `json:"summary"`
	SessionID      string              `json:"sessionId"` // UUID assigned at creation
	Started        bool                `json:"started"`   // false until first worker turn completes
	LastActivity   time.Time           `json:"lastActivity"`
	History        []ConversationTurn  `json:"history"`   // conversation turns for context preservation
	ParentID       string              `json:"parentId,omitempty"`     // ID of previous conversation in chain
	ChildID        string              `json:"childId,omitempty"`      // ID of next conversation in chain
	IsContinuation bool                `json:"isContinuation,omitempty"` // auto-generated continuation
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
	Projects map[string]*Project `json:"projects"`
	Active   ActiveRef           `json:"active"`
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
	Action         string  `json:"action"` // "resume" | "new" | "clarify"
	NewTitle       string  `json:"newTitle"`
	Clarify        string  `json:"clarify"`
	Confidence     float64 `json:"confidence"`
}

// Action constants.
const (
	ActionResume  = "resume"
	ActionNew     = "new"
	ActionClarify = "clarify"
)

// --- Worker run I/O (Design §3.3) ---

type RunRequest struct {
	Prompt    string
	WorkDir   string
	SessionID string // UUID
	Resume    bool   // true → --resume, false → --session-id
	Model     string
}

type RunResult struct {
	Text    string
	IsError bool
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
	ListActive() []WorkerStatus // return workers that are still running
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
}
