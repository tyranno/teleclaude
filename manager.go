package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// Design Ref: §2.2, §4.1, §6.3 — routing orchestration, clarify, fallback. Application layer.

type Manager struct {
	client ClaudeClient
	store  StoreRepo
	cfg    *Config
}

func NewManager(client ClaudeClient, store StoreRepo, cfg *Config) *Manager {
	return &Manager{client: client, store: store, cfg: cfg}
}

// Handle routes a free-text message to the right project/conversation and runs the Worker.
// Plan SC: 자연어 → 정확 라우팅 → 해당 디렉토리 작업, 대화별 맥락 분리.
func (m *Manager) Handle(ctx context.Context, chatID int64, text string, s MessageSender) {
	projects := m.store.ListProjects()
	if len(projects) == 0 {
		_ = s.Send(chatID, "등록된 프로젝트가 없습니다. 먼저 등록하세요:\n/project add <이름> <경로>")
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
		_ = s.Send(chatID, "🤔 어느 프로젝트/대화에서 할지 모르겠어요. /project list 로 확인하거나 /chat use <id> 로 지정해 주세요.")
		return
	}

	switch dec.Action {
	case ActionClarify:
		msg := dec.Clarify
		if msg == "" {
			msg = "어느 대화를 말씀하시는지 알려주세요. /chat list 로 목록을 볼 수 있어요."
		}
		_ = s.Send(chatID, "🤔 "+msg)

	case ActionNew:
		if _, exists := m.store.GetProject(dec.Project); !exists {
			_ = s.Send(chatID, "🤔 어느 프로젝트인지 분명하지 않아요. /project list 를 확인해 주세요.")
			return
		}
		c, err := m.store.NewConversation(dec.Project, dec.NewTitle)
		if err != nil {
			_ = s.Send(chatID, "⚠️ 새 대화 생성 실패: "+err.Error())
			return
		}
		m.runWorker(ctx, chatID, text, dec.Project, c, s)

	case ActionResume:
		c, exists := m.store.GetConversation(dec.Project, dec.ConversationID)
		if !exists {
			_ = s.Send(chatID, "🤔 해당 대화를 찾지 못했어요. /chat list 로 확인해 주세요.")
			return
		}
		m.runWorker(ctx, chatID, text, dec.Project, c, s)

	default:
		_ = s.Send(chatID, "🤔 라우팅 결과를 이해하지 못했어요. /chat use <id> 로 대화를 지정해 주세요.")
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
	projects := m.store.ListProjects()
	summaries := make([]ProjectSummary, 0, len(projects))
	for name, p := range projects {
		convs := make([]ConversationSummary, 0, len(p.Conversations))
		for _, id := range sortedConvIDs(p.Conversations) {
			c := p.Conversations[id]
			convs = append(convs, ConversationSummary{ID: c.ID, Title: c.Title, Summary: c.Summary})
		}
		summaries = append(summaries, ProjectSummary{Name: name, Conversations: convs})
	}
	return RouteRequest{Message: text, Projects: summaries, Active: m.store.GetActive()}
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
		// Compute series number and base title by walking to the chain root.
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

		// Create continuation conversation with parent reference
		newC, err := m.store.NewConversation(project, fmt.Sprintf("%s (시리즈 %d)", baseTitle, seriesNum))
		if err != nil {
			log.Printf("[manager] auto-continuation failed: %v", err)
			// Fall back: just use current conversation
		} else {
			// Link parent-child chain
			newC.ParentID = c.ID
			newC.IsContinuation = true
			parentSummary = c.Summary
			if parentSummary == "" {
				parentSummary = "이전 대화 내용을 참고해 주세요."
			}

			// Update current conversation with child reference
			c.ChildID = newC.ID
			if err := m.store.UpdateConversation(project, c); err != nil {
				log.Printf("[manager] update parent childID: %v", err)
			}

			workConv = newC
			_ = s.Send(chatID, "📝 대화가 길어져서 새 시리즈를 시작합니다...")
		}
	}

	s.Typing(chatID)
	isNewConv := !workConv.Started
	_ = s.Send(chatID, routingHeader(project, workConv.Title, isNewConv))

	startTime := time.Now()
	// Pass history only when there is no existing Claude session to resume.
	// When Started=true, --resume already carries full session history; passing
	// it again via prompt would double the context and skew the threshold check.
	var historyForPrompt []ConversationTurn
	if !workConv.Started {
		historyForPrompt = workConv.History
	}
	prompt := buildContextPrompt(text, parentSummary, historyForPrompt)
	res, err := m.client.Run(ctx, RunRequest{
		Prompt:    prompt,
		WorkDir:   p.Path,
		SessionID: workConv.SessionID,
		Resume:    workConv.Started,
		Model:     m.cfg.WorkerModel,
	})
	elapsed := time.Since(startTime)

	if err != nil {
		if ctx.Err() != nil {
			return // cancelled/timed out — bot already notified
		}
		_ = s.Send(chatID, "⚠️ 작업 실패: "+err.Error())
		return
	}

	_ = sendChunked(s, chatID, res.Text)

	// Persist conversation progress and history.
	workConv.Started = true
	workConv.LastActivity = time.Now().UTC()
	if res.Text != "" {
		workConv.Summary = truncate(res.Text, 80)
	}

	// Append this turn to conversation history for context preservation
	workConv.History = append(workConv.History, ConversationTurn{
		Timestamp: time.Now().UTC(),
		Prompt:    text,
		Response:  res.Text,
	})

	if err := m.store.UpdateConversation(project, workConv); err != nil {
		log.Printf("[manager] update conversation: %v", err)
	}
	if err := m.store.SetActive(project, workConv.ID); err != nil {
		log.Printf("[manager] set active: %v", err)
	}

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

// buildContextPrompt prepends conversation history to the current prompt for context continuity.
// If parentSummary is provided, it's included first (for continuation conversations).
func buildContextPrompt(currentPrompt, parentSummary string, history []ConversationTurn) string {
	var sb strings.Builder

	if parentSummary != "" {
		sb.WriteString("## 이전 대화 요약\n\n")
		sb.WriteString(parentSummary)
		sb.WriteString("\n\n---\n\n")
	}

	if len(history) > 0 {
		sb.WriteString("## 현재 대화 기록\n\n")
		for i, turn := range history {
			sb.WriteString(fmt.Sprintf("**Turn %d** (%s)\n", i+1, turn.Timestamp.Format("2006-01-02 15:04")))
			sb.WriteString(fmt.Sprintf("**요청:** %s\n", turn.Prompt))
			sb.WriteString(fmt.Sprintf("**응답:** %s\n\n", turn.Response))
		}
		sb.WriteString("---\n\n")
	}

	sb.WriteString("## 현재 요청\n\n")
	sb.WriteString(currentPrompt)

	return sb.String()
}
