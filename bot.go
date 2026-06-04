package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Design Ref: §2.1, §4.3 — Telegram polling, auth, command dispatch, single-flight + /cancel.
// Presentation layer; implements MessageSender for relay.

type Bot struct {
	api     *tgbotapi.BotAPI
	cfg     *Config
	store   StoreRepo
	manager *Manager

	mu            sync.Mutex
	busy          bool
	cancelCurrent context.CancelFunc
}

func NewBot(api *tgbotapi.BotAPI, cfg *Config, store StoreRepo, manager *Manager) *Bot {
	return &Bot{api: api, cfg: cfg, store: store, manager: manager}
}

// Send delivers a plain-text message (MessageSender).
func (b *Bot) Send(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := b.api.Send(msg)
	if err != nil {
		log.Printf("[bot] send error: %v", err)
	}
	return err
}

// Typing shows the "typing…" indicator (MessageSender).
func (b *Bot) Typing(chatID int64) {
	if _, err := b.api.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)); err != nil {
		log.Printf("[bot] typing error: %v", err)
	}
}

// Run starts the long-polling loop. Blocks until the update channel closes.
func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.api.GetUpdatesChan(u)
	log.Printf("[bot] @%s online, long-polling started", b.api.Self.UserName)

	for update := range updates {
		if update.Message == nil || update.Message.From == nil {
			continue
		}
		userID := update.Message.From.ID
		if !b.cfg.IsAllowed(userID) {
			log.Printf("[bot] denied user %d (%s)", userID, update.Message.From.UserName)
			continue
		}
		text := strings.TrimSpace(update.Message.Text)
		chatID := update.Message.Chat.ID
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "!") {
			b.handleCommand(chatID, text)
			continue
		}
		b.dispatchText(chatID, text)
	}
}

// dispatchText runs a free-text message through the Manager, one at a time.
func (b *Bot) dispatchText(chatID int64, text string) {
	b.mu.Lock()
	if b.busy {
		b.mu.Unlock()
		_ = b.Send(chatID, "⏳ 이전 작업을 처리 중입니다. !cancel 로 취소할 수 있어요.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(b.cfg.TimeoutMinutes)*time.Minute)
	b.busy = true
	b.cancelCurrent = cancel
	b.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[bot] panic recovered: %v", r)
				_ = b.Send(chatID, "⚠️ 내부 오류가 발생했습니다.")
			}
			cancel()
			b.mu.Lock()
			b.busy = false
			b.cancelCurrent = nil
			b.mu.Unlock()
		}()

		b.manager.Handle(ctx, chatID, text, b)

		switch ctx.Err() {
		case context.DeadlineExceeded:
			_ = b.Send(chatID, "⏱ 타임아웃으로 작업을 중단했습니다.")
		case context.Canceled:
			_ = b.Send(chatID, "🛑 작업이 취소되었습니다.")
		}
	}()
}

// handleCommand processes commands synchronously.
func (b *Bot) handleCommand(chatID int64, text string) {
	fields := strings.Fields(text)
	switch fields[0] {
	case "!start", "!help":
		_ = b.Send(chatID, helpText())
	case "!cancel":
		b.cancel(chatID)
	case "!status":
		_ = b.Send(chatID, b.manager.describeActive())
	case "!project":
		b.handleProject(chatID, text, fields)
	case "!chat":
		b.handleChat(chatID, text, fields)
	default:
		_ = b.Send(chatID, "알 수 없는 명령입니다. !help 를 참고하세요.")
	}
}

func (b *Bot) cancel(chatID int64) {
	b.mu.Lock()
	cancel, busy := b.cancelCurrent, b.busy
	b.mu.Unlock()
	if busy && cancel != nil {
		cancel()
		_ = b.Send(chatID, "🛑 취소 요청을 보냈습니다.")
		return
	}
	_ = b.Send(chatID, "취소할 작업이 없습니다.")
}

// handleProject: !project add <name> <path> | remove <name> | list
func (b *Bot) handleProject(chatID int64, text string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !project add <이름> <경로> | !project remove <이름> | !project list")
		return
	}
	switch fields[1] {
	case "add":
		// SplitN keeps spaces in the Windows path intact: [!project add name path...]
		parts := strings.SplitN(text, " ", 4)
		if len(parts) < 4 {
			_ = b.Send(chatID, "사용법: !project add <이름> <경로>")
			return
		}
		name, path := parts[2], strings.TrimSpace(parts[3])
		if err := b.store.AddProject(name, path); err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
			return
		}
		_ = b.Send(chatID, fmt.Sprintf("✅ 프로젝트 등록: %s → %s", name, path))
	case "remove":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !project remove <이름>")
			return
		}
		if err := b.store.RemoveProject(fields[2]); err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
			return
		}
		_ = b.Send(chatID, "🗑 프로젝트 제거: "+fields[2])
	case "list":
		_ = b.Send(chatID, b.formatProjectList())
	default:
		_ = b.Send(chatID, "사용법: !project add <이름> <경로> | !project remove <이름> | !project list")
	}
}

func (b *Bot) formatProjectList() string {
	projects := b.store.ListProjects()
	if len(projects) == 0 {
		return "등록된 프로젝트가 없습니다. !project add <이름> <경로>"
	}
	active := b.store.GetActive()
	var sb strings.Builder
	sb.WriteString("📂 프로젝트 목록\n")
	for name, p := range projects {
		marker := ""
		if name == active.Project {
			marker = " ⭐"
		}
		sb.WriteString(fmt.Sprintf("\n• %s%s\n  %s\n", name, marker, p.Path))
		if len(p.Conversations) == 0 {
			sb.WriteString("  (대화 없음)\n")
		}
		for _, id := range sortedConvIDs(p.Conversations) {
			c := p.Conversations[id]
			cm := ""
			if name == active.Project && id == active.ConversationID {
				cm = " ⭐"
			}
			sb.WriteString(fmt.Sprintf("  [%s] %s%s\n", id, c.Title, cm))
		}
	}
	return sb.String()
}

// handleChat: !chat new [title] | list | use <id> — operates on the active project.
func (b *Bot) handleChat(chatID int64, text string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !chat new [제목] | !chat list | !chat use <id>")
		return
	}
	active := b.store.GetActive()
	if active.Project == "" {
		_ = b.Send(chatID, "활성 프로젝트가 없습니다. 먼저 메시지를 보내거나 !project list 후 작업하세요.")
		return
	}
	switch fields[1] {
	case "new":
		title := ""
		if parts := strings.SplitN(text, " ", 3); len(parts) == 3 {
			title = strings.TrimSpace(parts[2])
		}
		c, err := b.store.NewConversation(active.Project, title)
		if err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
			return
		}
		_ = b.store.SetActive(active.Project, c.ID)
		_ = b.Send(chatID, fmt.Sprintf("🆕 새 대화 [%s] %s (활성화됨)", c.ID, c.Title))
	case "list":
		_ = b.Send(chatID, b.formatChatList(active.Project))
	case "use":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !chat use <id>")
			return
		}
		c, ok := b.store.GetConversation(active.Project, fields[2])
		if !ok {
			_ = b.Send(chatID, "해당 대화를 찾을 수 없습니다: "+fields[2])
			return
		}
		_ = b.store.SetActive(active.Project, c.ID)
		_ = b.Send(chatID, fmt.Sprintf("✅ 대화 전환 [%s] %s", c.ID, c.Title))
	default:
		_ = b.Send(chatID, "사용법: !chat new [제목] | !chat list | !chat use <id>")
	}
}

func (b *Bot) formatChatList(project string) string {
	p, ok := b.store.GetProject(project)
	if !ok {
		return "프로젝트를 찾을 수 없습니다: " + project
	}
	if len(p.Conversations) == 0 {
		return fmt.Sprintf("📂 %s: 대화가 없습니다. !chat new [제목]", project)
	}
	active := b.store.GetActive()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("💬 %s 대화 목록\n", project))
	for _, id := range sortedConvIDs(p.Conversations) {
		c := p.Conversations[id]
		cm := ""
		if id == active.ConversationID {
			cm = " ⭐"
		}
		line := fmt.Sprintf("[%s] %s%s", id, c.Title, cm)
		if c.Summary != "" {
			line += " — " + c.Summary
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func helpText() string {
	return strings.TrimSpace(`
🤖 teleclaude — 폰에서 PC의 Claude를 자연어로 쓰세요.

그냥 말하세요. 예) "myapp 로그인 버그 이어서 보자", "voice 서버에 헬스체크 추가해줘"
→ 어느 프로젝트의 어느 대화인지 알아서 찾아 작업합니다.

명령어:
!project add <이름> <경로>   프로젝트 등록
!project remove <이름>       프로젝트 제거
!project list                프로젝트·대화 목록
!chat new [제목]             현재 프로젝트에 새 대화
!chat list                   현재 프로젝트의 대화 목록
!chat use <id>               대화 수동 전환
!status                      현재 활성 대화
!cancel                      진행 중 작업 취소
!help                        이 도움말
`)
}
