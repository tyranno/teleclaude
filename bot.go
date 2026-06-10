package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Design Ref: §2.1, §4.3 — Telegram polling, auth, command dispatch, queue + /cancel.
// Presentation layer; implements MessageSender for relay.

type queuedMsg struct {
	chatID int64
	text   string
}

type Bot struct {
	api       *tgbotapi.BotAPI
	cfg       *Config
	store     StoreRepo
	manager   *Manager
	scheduler *Scheduler
	onReady   func() // called once after GetUpdatesChan starts (handoff signal)

	mu            sync.Mutex
	busy          bool
	cancelCurrent context.CancelFunc
	queue         []queuedMsg // pending messages while busy
}

func NewBot(api *tgbotapi.BotAPI, cfg *Config, store StoreRepo, manager *Manager, scheduler *Scheduler) *Bot {
	return &Bot{api: api, cfg: cfg, store: store, manager: manager, scheduler: scheduler}
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
	if b.onReady != nil {
		b.onReady() // fire after polling starts — used by handoff to signal old process
	}

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

// dispatchText routes a free-text message through the Manager.
// If a Worker is already running, the message is queued and processed in order.
func (b *Bot) dispatchText(chatID int64, text string) {
	b.mu.Lock()
	if b.busy {
		b.queue = append(b.queue, queuedMsg{chatID: chatID, text: text})
		pos := len(b.queue)
		b.mu.Unlock()
		_ = b.Send(chatID, fmt.Sprintf("📋 대기열 추가 (%d번째) — 현재 작업이 끝나면 순서대로 처리됩니다. !cancel 로 현재 작업을 취소할 수 있어요.", pos))
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
			var next *queuedMsg
			if len(b.queue) > 0 {
				n := b.queue[0]
				next = &n
				b.queue = b.queue[1:]
			}
			b.mu.Unlock()

			if next != nil {
				_ = b.Send(next.chatID, "▶️ 대기 중이던 요청을 시작합니다.")
				b.dispatchText(next.chatID, next.text)
			}
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
		msg := b.manager.describeActive()
		b.mu.Lock()
		qLen := len(b.queue)
		b.mu.Unlock()
		if qLen > 0 {
			msg += fmt.Sprintf("\n📋 대기 중: %d개", qLen)
		}
		_ = b.Send(chatID, msg)
	case "!project":
		b.handleProject(chatID, text, fields)
	case "!chat":
		b.handleChat(chatID, text, fields)
	case "!update":
		b.mu.Lock()
		busy := b.busy
		b.mu.Unlock()
		if busy {
			_ = b.Send(chatID, "⏳ 작업 중에는 업데이트할 수 없습니다. !cancel 후 다시 시도하세요.")
			return
		}
		b.handleUpdate(chatID)
	case "!remind":
		b.handleRemind(chatID, text, fields)
	case "!cron":
		b.handleCron(chatID, text, fields)
	case "!backend":
		b.handleBackend(chatID, fields)
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

// handleUpdate builds teleclaude_new.exe, starts it, waits for it to connect to
// Telegram, then hands over: old process exits cleanly, new process renames itself.
// Works without launcher.ps1 — zero downtime.
func (b *Bot) handleUpdate(chatID int64) {
	_ = b.Send(chatID, "🔨 빌드 시작...")

	exe, err := os.Executable()
	if err != nil {
		_ = b.Send(chatID, "⚠️ 실행 파일 경로 확인 실패: "+err.Error())
		return
	}
	srcDir := filepath.Dir(exe)
	newExe := filepath.Join(srcDir, "teleclaude_new"+exeSuffix)
	readyFile := filepath.Join(os.TempDir(), fmt.Sprintf(".teleclaude_ready_%d", os.Getpid()))

	// Verify source code exists in srcDir (fix: exe copied to different dir would silently fail)
	if _, serr := os.Stat(filepath.Join(srcDir, "main.go")); serr != nil {
		_ = b.Send(chatID, "⚠️ 소스 코드를 찾을 수 없습니다 ("+srcDir+")\nexe와 소스 코드가 같은 디렉터리에 있어야 !update가 작동합니다.")
		return
	}

	// If we're already running as teleclaude_new.exe, the self-rename from the previous
	// handoff hasn't completed yet (or failed). teleclaude_new.exe is our own exe file,
	// so go build cannot overwrite it. Abort and instruct the user.
	if filepath.Base(exe) == "teleclaude_new"+exeSuffix {
		if _, serr := os.Stat(newExe); serr == nil {
			_ = b.Send(chatID, "⚠️ 이전 핸드오프의 이름 변경이 아직 완료되지 않았습니다.\n잠시 후 다시 시도하거나 teleclaude_new를 teleclaude로 수동 교체 후 재시작하세요.")
			return
		}
	}

	// Build
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", newExe, ".")
	buildCmd.Dir = srcDir
	if out, berr := buildCmd.CombinedOutput(); berr != nil {
		_ = b.Send(chatID, "⚠️ 빌드 실패:\n"+strings.TrimSpace(string(out)))
		return
	}

	_ = b.Send(chatID, "✅ 빌드 성공! 새 버전 연결 중...")
	_ = os.Remove(readyFile)

	// Start new process — passes readyFile + chatID so it can signal and notify via Telegram
	newProc := exec.Command(newExe, "run",
		"--handoff-ready", readyFile,
		"--notify-chat", fmt.Sprintf("%d", chatID),
	)
	if err := newProc.Start(); err != nil {
		_ = b.Send(chatID, "⚠️ 새 버전 시작 실패: "+err.Error())
		return
	}

	// Wait up to 60s for new process to signal Telegram connection.
	// 60s: claude health check is up to 20s, bot init adds more time.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if _, serr := os.Stat(readyFile); serr == nil {
			_ = os.Remove(readyFile)
			_ = b.Send(chatID, "🔄 새 버전 연결됨! 전환합니다...")
			log.Println("[bot] handoff: new instance ready, exiting")
			os.Exit(0)
		}
	}

	// Timeout — kill new process, keep current running
	_ = newProc.Process.Kill()
	_ = b.Send(chatID, "⚠️ 새 버전 연결 대기 시간 초과 (60초). 이전 버전 계속 사용합니다.")
}

// handleRemind processes !remind commands.
// Usage:
//
//	!remind 30m 배포 확인           — 30분 후 알림
//	!remind 2h task 서버 확인해줘   — 2시간 후 Claude 작업
//	!remind list                    — 대기 중 목록
//	!remind cancel <id>             — 취소
func (b *Bot) handleRemind(chatID int64, text string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !remind <시간> [task] <메시지>  |  !remind list  |  !remind cancel <id>\n예) !remind 30m 배포 확인, !remind 2h task 서버 상태 확인해줘")
		return
	}
	switch fields[1] {
	case "list":
		reminders := b.scheduler.ListReminders()
		if len(reminders) == 0 {
			_ = b.Send(chatID, "대기 중인 알림이 없습니다.")
			return
		}
		var sb strings.Builder
		sb.WriteString("⏰ 대기 중인 알림:\n")
		for _, r := range reminders {
			remaining := time.Until(r.FireAt).Round(time.Second)
			fmt.Fprintf(&sb, "[%s] %s 후 — %s\n", r.ID, remaining, r.Message)
		}
		_ = b.Send(chatID, sb.String())
	case "cancel":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !remind cancel <id>")
			return
		}
		if b.scheduler.Remove(fields[2]) {
			_ = b.Send(chatID, "✅ 알림 취소됨: "+fields[2])
		} else {
			_ = b.Send(chatID, "⚠️ 알림을 찾을 수 없습니다: "+fields[2])
		}
	default:
		// !remind <duration> [task] <message>
		dur, _, err := ParseSchedule(fields[1])
		if err != nil {
			_ = b.Send(chatID, "⚠️ 시간 형식 오류: "+err.Error())
			return
		}
		isTask := len(fields) > 2 && fields[2] == "task"
		msgStart := 2
		if isTask {
			msgStart = 3
		}
		if msgStart >= len(fields) {
			_ = b.Send(chatID, "⚠️ 메시지를 입력해주세요.")
			return
		}
		msg := strings.Join(fields[msgStart:], " ")
		fireAt := time.Now().Add(dur)
		r, err := b.scheduler.AddReminder(chatID, msg, fireAt)
		if err != nil {
			_ = b.Send(chatID, "⚠️ 알림 등록 실패: "+err.Error())
			return
		}
		_ = isTask // isTask reminders use same path for now — sends notification
		_ = b.Send(chatID, fmt.Sprintf("✅ 알림 등록 [%s] — %s 후: %s", r.ID, dur.Round(time.Second), msg))
	}
}

// handleCron processes !cron commands.
// Usage:
//
//	!cron add <schedule> <메시지>          — 반복 알림
//	!cron add <schedule> task <프롬프트>   — 반복 Claude 작업
//	!cron list                             — 목록
//	!cron remove <id>                      — 제거
func (b *Bot) handleCron(chatID int64, text string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !cron add <주기> [task] <내용>  |  !cron list  |  !cron remove <id>\n주기 예) 30m, 2h, daily, hourly\n예) !cron add 1h 서버 상태 확인, !cron add daily task 오늘의 작업 요약해줘")
		return
	}
	switch fields[1] {
	case "list":
		crons := b.scheduler.ListCrons()
		if len(crons) == 0 {
			_ = b.Send(chatID, "등록된 크론 작업이 없습니다.")
			return
		}
		var sb strings.Builder
		sb.WriteString("🔔 크론 작업 목록:\n")
		for _, c := range crons {
			kind := "알림"
			if c.IsTask {
				kind = "작업"
			}
			next := time.Until(c.NextFire).Round(time.Second)
			fmt.Fprintf(&sb, "[%s] %s (%s) — 다음: %s 후\n  %s\n", c.ID, c.Label, kind, next, c.Task)
		}
		_ = b.Send(chatID, sb.String())
	case "remove":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !cron remove <id>")
			return
		}
		if b.scheduler.Remove(fields[2]) {
			_ = b.Send(chatID, "✅ 크론 제거됨: "+fields[2])
		} else {
			_ = b.Send(chatID, "⚠️ 크론을 찾을 수 없습니다: "+fields[2])
		}
	case "add":
		if len(fields) < 4 {
			_ = b.Send(chatID, "사용법: !cron add <주기> [task] <내용>")
			return
		}
		dur, label, err := ParseSchedule(fields[2])
		if err != nil {
			_ = b.Send(chatID, "⚠️ 주기 형식 오류: "+err.Error())
			return
		}
		isTask := fields[3] == "task"
		msgStart := 3
		if isTask {
			msgStart = 4
		}
		if msgStart >= len(fields) {
			_ = b.Send(chatID, "⚠️ 내용을 입력해주세요.")
			return
		}
		task := strings.Join(fields[msgStart:], " ")
		c, err := b.scheduler.AddCron(chatID, label, dur, task, isTask)
		if err != nil {
			_ = b.Send(chatID, "⚠️ 크론 등록 실패: "+err.Error())
			return
		}
		kind := "알림"
		if isTask {
			kind = "Claude 작업"
		}
		_ = b.Send(chatID, fmt.Sprintf("✅ 크론 등록 [%s] %s (%s)\n  내용: %s", c.ID, label, kind, task))
	default:
		_ = b.Send(chatID, "사용법: !cron add | list | remove")
	}
}

// handleBackend handles !backend — displays or switches the active AI backend.
func (b *Bot) handleBackend(chatID int64, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "현재 백엔드: "+strings.ToUpper(b.manager.Backend()))
		return
	}
	target := strings.ToLower(fields[1])
	switch target {
	case "claude", "codex":
	default:
		_ = b.Send(chatID, "사용법: !backend [claude|codex]")
		return
	}

	b.mu.Lock()
	busy := b.busy
	b.mu.Unlock()
	if busy {
		_ = b.Send(chatID, "⏳ 작업 중에는 백엔드를 전환할 수 없습니다. !cancel 후 다시 시도하세요.")
		return
	}

	current := b.manager.Backend()
	if current == target {
		_ = b.Send(chatID, "이미 "+strings.ToUpper(target)+" 백엔드입니다.")
		return
	}

	if err := b.manager.SetBackend(target); err != nil {
		_ = b.Send(chatID, "⚠️ "+err.Error())
		return
	}
	_ = b.Send(chatID, fmt.Sprintf("✅ 백엔드 전환됨: %s → %s", strings.ToUpper(current), strings.ToUpper(target)))
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
!status                      현재 활성 대화 및 실행 중 작업
!cancel                      진행 중 작업 취소
!remind <시간> <메시지>      일회성 알림 (예: !remind 30m 배포 확인)
!remind list / cancel <id>   알림 목록 / 취소
!cron add <주기> <내용>      반복 알림/작업 (예: !cron add hourly 서버 체크)
!cron list / remove <id>     크론 목록 / 제거
!backend [claude|codex]      AI 백엔드 전환 (현재 상태 확인 또는 전환)
!update                      새 버전 빌드 & 자동 재시작
!help                        이 도움말

주기 형식: 30m, 2h, 1d, hourly, daily, weekly
task 접두어: !remind 1h task 서버 확인해줘  →  Claude 작업으로 실행
`)
}
