package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
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
	chatID   int64
	text     string
	isTask   bool // true = scheduled task (bypass manager routing)
}

// Bot dispatches Telegram messages to concurrent Workers.
// Up to cfg.MaxWorkers (default 3) can run at the same time; extras are queued.
type Bot struct {
	api         *tgbotapi.BotAPI
	cfg         *Config
	store       StoreRepo
	manager     *Manager
	scheduler   *Scheduler
	rateLimiter *RateLimiter
	userStore   *UserStore
	onReady     func() // called once after GetUpdatesChan starts (handoff signal)

	mu          sync.Mutex
	activeCount int                        // current running workers
	workerSeq   int                        // monotonic counter for worker IDs
	cancels     map[int]context.CancelFunc // workerID → cancel (for !cancel)
	queue       []queuedMsg               // messages waiting for a free slot
}

func NewBot(api *tgbotapi.BotAPI, cfg *Config, store StoreRepo, manager *Manager, scheduler *Scheduler, userStore *UserStore) *Bot {
	return &Bot{
		api:         api,
		cfg:         cfg,
		store:       store,
		manager:     manager,
		scheduler:   scheduler,
		rateLimiter: NewRateLimiter(cfg.RateLimitPerMin),
		userStore:   userStore,
		cancels:     make(map[int]context.CancelFunc),
	}
}

// isAllowed checks all auth sources: config IDs, config usernames, and runtime UserStore.
func (b *Bot) isAllowed(userID int64, username string) bool {
	return b.cfg.IsAllowed(userID) ||
		b.cfg.IsAllowedByUsername(username) ||
		(b.userStore != nil && b.userStore.Contains(userID))
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

// Run starts the long-polling loop. Blocks until the process exits.
// Uses GetUpdates directly (not GetUpdatesChan) so Conflict errors are visible
// and trigger an automatic restart via os.Exit(1) + systemd Restart=on-failure.
func (b *Bot) Run() {
	log.Printf("[bot] @%s online, long-polling started", b.api.Self.UserName)
	if b.onReady != nil {
		b.onReady() // fire after polling confirmed — used by handoff to signal old process
	}

	offset := 0
	for {
		cfg := tgbotapi.NewUpdate(offset)
		cfg.Timeout = 30
		updates, err := b.api.GetUpdates(cfg)
		if err != nil {
			if strings.Contains(err.Error(), "Conflict") {
				// Another instance is polling the same token.
				// Exit so systemd restarts us; killPreviousInstance() will then
				// terminate the other instance before we start polling again.
				log.Printf("[bot] Conflict — 다른 인스턴스가 polling 중. 5초 후 재시작.")
				time.Sleep(5 * time.Second)
				os.Exit(1)
			}
			log.Printf("[bot] getUpdates 실패: %v — 3초 후 재시도", err)
			time.Sleep(3 * time.Second)
			continue
		}

		for _, update := range updates {
			if update.UpdateID+1 > offset {
				offset = update.UpdateID + 1
			}
			if update.Message == nil || update.Message.From == nil {
				continue
			}
			userID := update.Message.From.ID
			username := update.Message.From.UserName
			if !b.isAllowed(userID, username) {
				log.Printf("[bot] denied user %d (%s)", userID, username)
				continue
			}
			chatID := update.Message.Chat.ID

			// Attachments take priority over text — download then dispatch with caption.
			if b.hasAttachment(update.Message) {
				go b.handleAttachment(chatID, update.Message)
				continue
			}

			text := strings.TrimSpace(update.Message.Text)
			if text == "" {
				continue
			}
			// Rate-limit free-text messages only (commands are cheap, workers are expensive).
			if !strings.HasPrefix(text, "!") && !b.rateLimiter.Allow(userID) {
				_ = b.Send(chatID, "⚠️ 요청이 너무 많습니다. 잠시 후 다시 시도해 주세요.")
				log.Printf("[bot] rate-limited user %d", userID)
				continue
			}
			if strings.HasPrefix(text, "!") {
				b.handleCommand(chatID, text)
				continue
			}
			b.dispatchText(chatID, text)
		}
	}
}

// dispatchText routes a free-text message through the Manager.
// Up to cfg.MaxWorkers can run in parallel; extras are queued.
func (b *Bot) dispatchText(chatID int64, text string) {
	b.dispatch(queuedMsg{chatID: chatID, text: text})
}

// dispatchScheduledTask runs a pre-scheduled task bypassing Manager LLM routing.
// Up to cfg.MaxWorkers can run in parallel; extras are queued.
func (b *Bot) dispatchScheduledTask(chatID int64, text string) {
	_ = b.Send(chatID, "⏰ 예약 작업 실행 중: "+truncate(text, 60))
	b.dispatch(queuedMsg{chatID: chatID, text: text, isTask: true})
}

// dispatch is the shared entry point for both text and scheduled-task dispatches.
// It acquires a worker slot (or queues the message) then runs the appropriate handler.
func (b *Bot) dispatch(msg queuedMsg) {
	b.mu.Lock()
	if b.activeCount >= b.cfg.MaxWorkers {
		b.queue = append(b.queue, msg)
		pos := len(b.queue)
		maxW := b.cfg.MaxWorkers
		b.mu.Unlock()
		if !msg.isTask {
			_ = b.Send(msg.chatID, fmt.Sprintf(
				"📋 대기열 추가 (%d번째) — 동시 처리 중 %d/%d. !cancel 로 취소 가능.",
				pos, maxW, maxW))
		} else {
			log.Printf("[scheduler] 예약 작업 대기열 추가 (%d번째)", pos)
		}
		return
	}
	b.workerSeq++
	wid := b.workerSeq
	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(b.cfg.TimeoutMinutes)*time.Minute,
	)
	b.activeCount++
	b.cancels[wid] = cancel
	b.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[bot] panic recovered (wid=%d): %v", wid, r)
				_ = b.Send(msg.chatID, "⚠️ 내부 오류가 발생했습니다.")
			}
			cancel()
			b.mu.Lock()
			b.activeCount--
			delete(b.cancels, wid)
			var next *queuedMsg
			if len(b.queue) > 0 {
				n := b.queue[0]
				next = &n
				b.queue = b.queue[1:]
			}
			b.mu.Unlock()
			if next != nil {
				if !next.isTask {
					_ = b.Send(next.chatID, "▶️ 대기 중이던 요청을 시작합니다.")
				}
				b.dispatch(*next)
			}
		}()

		if msg.isTask {
			b.manager.HandleScheduledTask(ctx, msg.chatID, msg.text, b)
		} else {
			b.manager.Handle(ctx, msg.chatID, msg.text, b)
		}

		switch ctx.Err() {
		case context.DeadlineExceeded:
			_ = b.Send(msg.chatID, "⏱ 타임아웃으로 작업을 중단했습니다.")
		case context.Canceled:
			_ = b.Send(msg.chatID, "🛑 작업이 취소되었습니다.")
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
		workers := b.manager.DescribeActiveWorkers()
		var msg string
		if workers == "실행 중인 작업 없음" {
			msg = b.manager.describeActive()
		} else {
			msg = workers + "\n" + b.manager.describeActive()
		}
		b.mu.Lock()
		active := b.activeCount
		qLen := len(b.queue)
		b.mu.Unlock()
		if active > 0 {
			msg += fmt.Sprintf("\n⚡ 동시 실행: %d/%d", active, b.cfg.MaxWorkers)
		}
		if qLen > 0 {
			msg += fmt.Sprintf("\n📋 대기 중: %d개", qLen)
		}
		msg += "\n🔧 백엔드: " + strings.ToUpper(b.manager.Backend())
		_ = b.Send(chatID, msg)
	case "!project":
		b.handleProject(chatID, text, fields)
	case "!chat":
		b.handleChat(chatID, text, fields)
	case "!update":
		b.mu.Lock()
		active := b.activeCount
		b.mu.Unlock()
		if active > 0 {
			_ = b.Send(chatID, "⏳ 작업 중에는 업데이트할 수 없습니다. !cancel 후 다시 시도하세요.")
			return
		}
		b.handleUpdate(chatID)
	case "!task":
		b.handleTask(chatID, text, fields)
	case "!remind":
		b.handleRemind(chatID, text, fields)
	case "!cron":
		b.handleCron(chatID, text, fields)
	case "!history":
		b.handleHistory(chatID, fields)
	case "!backend":
		b.handleBackend(chatID, fields)
	case "!user":
		b.handleUser(chatID, fields)
	case "!parallel":
		b.handleParallel(chatID, text)
	default:
		_ = b.Send(chatID, "알 수 없는 명령입니다. !help 를 참고하세요.")
	}
}

func (b *Bot) cancel(chatID int64) {
	b.mu.Lock()
	fns := make([]context.CancelFunc, 0, len(b.cancels))
	for _, fn := range b.cancels {
		fns = append(fns, fn)
	}
	b.mu.Unlock()
	if len(fns) == 0 {
		_ = b.Send(chatID, "취소할 작업이 없습니다.")
		return
	}
	for _, fn := range fns {
		fn()
	}
	_ = b.Send(chatID, fmt.Sprintf("🛑 %d개 작업 취소 요청됨.", len(fns)))
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
		fmt.Fprintf(&sb, "\n• %s%s\n  %s\n", name, marker, p.Path)
		if len(p.Conversations) == 0 {
			sb.WriteString("  (대화 없음)\n")
		}
		for _, id := range sortedConvIDs(p.Conversations) {
			c := p.Conversations[id]
			cm := ""
			if name == active.Project && id == active.ConversationID {
				cm = " ⭐"
			}
			fmt.Fprintf(&sb, "  [%s] %s%s\n", id, c.Title, cm)
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
	fmt.Fprintf(&sb, "💬 %s 대화 목록\n", project)
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
func (b *Bot) handleRemind(chatID int64, _ string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !remind <시간> [task] <메시지>  |  !remind list  |  !remind cancel <id>\n시간 예) 30m, 2h, 1d — 단위: m(분) h(시간) d(일)\n예) !remind 30m 배포 확인, !remind 2h task 서버 상태 확인해줘")
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
			remaining := time.Until(r.FireAt)
			var timeStr string
			if remaining < 0 {
				timeStr = "즉시 실행 예정"
			} else {
				timeStr = remaining.Round(time.Second).String() + " 후"
			}
			fmt.Fprintf(&sb, "[%s] %s — %s\n", r.ID, timeStr, r.Prompt)
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
		t := &Task{
			ID:        newTaskID(),
			ChatID:    chatID,
			Prompt:    msg,
			FireAt:    fireAt,
			Status:    "pending",
			IsTask:    isTask,
			Label:     "알림: " + msg,
			CreatedAt: time.Now(),
		}
		if err := b.scheduler.AddTask(t); err != nil {
			_ = b.Send(chatID, "⚠️ 알림 등록 실패: "+err.Error())
			return
		}
		kind := "알림"
		if isTask {
			kind = "Claude 작업"
		}
		_ = b.Send(chatID, fmt.Sprintf("✅ 알림 등록 [%s] — %s 후 (%s): %s", t.ID, dur.Round(time.Second), kind, msg))
	}
}

// handleCron processes !cron commands.
// Usage:
//
//	!cron add <schedule> <메시지>          — 반복 알림
//	!cron add <schedule> task <프롬프트>   — 반복 Claude 작업
//	!cron list                             — 목록
//	!cron remove <id>                      — 제거
func (b *Bot) handleCron(chatID int64, _ string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !cron add <주기> [task] <내용>  |  !cron list  |  !cron remove <id>\n주기 예) 30m, 2h, 1d, 1w, 매시간, 매일, 매주\n예) !cron add 1h 서버 상태 확인, !cron add 매일 task 오늘의 작업 요약해줘")
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
			nextAt := b.scheduler.NextFire(c.ID)
			var nextStr string
			if nextAt.IsZero() {
				nextStr = "계산 중..."
			} else {
				remaining := time.Until(nextAt)
				if remaining < 0 {
					nextStr = "즉시 실행 예정"
				} else {
					nextStr = remaining.Round(time.Second).String() + " 후"
				}
			}
			fmt.Fprintf(&sb, "[%s] %s (%s) — 다음: %s\n  %s\n", c.ID, c.Label, kind, nextStr, c.Prompt)
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

// handleTask processes !task commands — the unified scheduling interface.
//
// Subcommands:
//
//	!task add <cron|duration> [task] <prompt>
//	!task add <cron|duration> --script <script> [task] <prompt>
//	!task once <HH:MM|YYYY-MM-DD HH:MM> <message>
//	!task list [pending|paused|all]
//	!task pause|resume|cancel <id>
//	!task update <id> [--cron <expr>] [--prompt <text>] [--script <script>]
func (b *Bot) handleTask(chatID int64, _ string, fields []string) {
	if len(fields) < 2 {
		_ = b.Send(chatID, taskHelpText())
		return
	}
	switch fields[1] {
	case "help":
		_ = b.Send(chatID, taskHelpText())

	case "list":
		filter := "pending"
		if len(fields) >= 3 {
			filter = fields[2]
		}
		tasks := b.scheduler.ListTasks(filter)
		if len(tasks) == 0 {
			_ = b.Send(chatID, "등록된 작업이 없습니다. (필터: "+filter+")")
			return
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "📋 작업 목록 (%s):\n", filter)
		for _, t := range tasks {
			kind := "알림"
			if t.IsTask {
				kind = "작업"
			}
			schedule := t.CronExpr
			if schedule == "" {
				schedule = "일회성 " + t.FireAt.Format("2006-01-02 15:04")
			}
			next := b.scheduler.NextFire(t.ID)
			nextStr := ""
			if !next.IsZero() {
				remaining := time.Until(next)
				if remaining < 0 {
					nextStr = " → 즉시 실행 예정"
				} else {
					nextStr = fmt.Sprintf(" → %s 후", remaining.Round(time.Second))
				}
			} else if t.Status == "pending" && !t.FireAt.IsZero() {
				remaining := time.Until(t.FireAt)
				if remaining < 0 {
					nextStr = " → 즉시 실행 예정"
				} else {
					nextStr = fmt.Sprintf(" → %s 후", remaining.Round(time.Second))
				}
			}
			scriptMark := ""
			if t.Script != "" {
				scriptMark = " [스크립트]"
			}
			// Show full prompt line only when it's truncated in the label (>30 runes).
			promptSuffix := ""
			if len([]rune(t.Prompt)) > 30 {
				promptSuffix = "\n  ▶ " + truncate(t.Prompt, 80)
			}
			fmt.Fprintf(&sb, "[%s] %s (%s/%s)%s\n  %s%s%s\n",
				t.ID, t.Label, t.Status, kind, scriptMark, schedule, nextStr, promptSuffix)
		}
		_ = b.Send(chatID, sb.String())

	case "pause":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !task pause <id>")
			return
		}
		if err := b.scheduler.PauseTask(fields[2]); err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
		} else {
			_ = b.Send(chatID, "⏸ 작업 일시정지됨: "+fields[2])
		}

	case "resume":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !task resume <id>")
			return
		}
		if err := b.scheduler.ResumeTask(fields[2]); err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
		} else {
			_ = b.Send(chatID, "▶️ 작업 재개됨: "+fields[2])
		}

	case "cancel":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !task cancel <id>")
			return
		}
		if err := b.scheduler.CancelTask(fields[2]); err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
		} else {
			_ = b.Send(chatID, "✅ 작업 취소됨: "+fields[2])
		}

	case "update":
		// !task update <id> [--cron <expr>] [--prompt <text>] [--script <script>] [--depends-on <id,...>]
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !task update <id> [--cron <식>] [--prompt <텍스트>] [--script <스크립트>] [--depends-on <id,...>]")
			return
		}
		id := fields[2]
		cronExpr, prompt, script, depsRaw := parseFlags4(fields[3:], "--cron", "--prompt", "--script", "--depends-on")
		if cronExpr == "" && prompt == "" && script == "" && depsRaw == "" {
			_ = b.Send(chatID, "⚠️ 변경할 항목을 지정하세요.\n사용법: !task update <id> [--cron <식>] [--prompt <텍스트>] [--script <스크립트>] [--depends-on <id,...>]")
			return
		}
		if script != "" {
			if verr := validateScript(b.cfg, script); verr != nil {
				_ = b.Send(chatID, "⚠️ 스크립트 거부: "+verr.Error())
				return
			}
		}
		var deps []string
		if depsRaw != "" {
			deps = parseDependsOn(depsRaw)
		}
		if err := b.scheduler.UpdateTask(id, cronExpr, prompt, script, deps); err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
		} else {
			_ = b.Send(chatID, "✅ 작업 업데이트됨: "+id)
		}

	case "once":
		// !task once <HH:MM|YYYY-MM-DD HH:MM> <message>
		if len(fields) < 4 {
			_ = b.Send(chatID, "사용법: !task once <HH:MM|YYYY-MM-DD HH:MM> <메시지>")
			return
		}
		fireAt, msgStart, err := parseOnceDatetime(fields[2:])
		if err != nil {
			_ = b.Send(chatID, "⚠️ 시각 형식 오류: "+err.Error())
			return
		}
		msg := strings.Join(fields[2+msgStart:], " ")
		if msg == "" {
			_ = b.Send(chatID, "⚠️ 메시지를 입력해주세요.")
			return
		}
		t, err := b.scheduler.AddReminder(chatID, msg, fireAt)
		if err != nil {
			_ = b.Send(chatID, "⚠️ 등록 실패: "+err.Error())
			return
		}
		dayLabel := ""
		now := time.Now()
		if fireAt.Year() == now.Year() && fireAt.YearDay() == now.YearDay() {
			dayLabel = " (오늘)"
		} else if fireAt.Sub(now) < 48*time.Hour {
			dayLabel = " (내일)"
		}
		_ = b.Send(chatID, fmt.Sprintf("✅ 일회성 등록 [%s] — %s%s에 실행\n  %s",
			t.ID, fireAt.Format("2006-01-02 15:04"), dayLabel, msg))

	case "add":
		// !task add <cron|duration> [--script <script>] [task] <prompt>
		if len(fields) < 4 {
			_ = b.Send(chatID, "사용법: !task add <주기> [task] <프롬프트>\n주기: 30m, 2h, 1d, 1w, 매시간, 매일, 매주, 또는 5-field cron\n예) !task add 매일 task 오늘 요약해줘\n    !task add 0 9 * * 1-5 task 주식 확인")
			return
		}
		cronExpr, script, dependsOn, isTask, prompt, err := parseTaskAddArgs(fields[2:])
		if err != nil {
			_ = b.Send(chatID, "⚠️ "+err.Error())
			return
		}
		if script != "" {
			if verr := validateScript(b.cfg, script); verr != nil {
				_ = b.Send(chatID, "⚠️ 스크립트 거부: "+verr.Error())
				return
			}
		}
		kind := "알림"
		if isTask {
			kind = "Claude 작업"
		}
		t := &Task{
			ID:        newTaskID(),
			ChatID:    chatID,
			Prompt:    prompt,
			Script:    script,
			CronExpr:  cronExpr,
			DependsOn: dependsOn,
			Status:    "pending",
			IsTask:    isTask,
			Label:     truncate(prompt, 30),
			CreatedAt: time.Now(),
		}
		if err := b.scheduler.AddTask(t); err != nil {
			_ = b.Send(chatID, "⚠️ 등록 실패: "+err.Error())
			return
		}
		scriptNote := ""
		if script != "" {
			scriptNote = " [스크립트 사전확인 있음]"
		}
		_ = b.Send(chatID, fmt.Sprintf("✅ 작업 등록 [%s] %s (%s)%s\n  %s",
			t.ID, cronExpr, kind, scriptNote, prompt))

	default:
		_ = b.Send(chatID, "알 수 없는 !task 하위 명령. !task help 참조")
	}
}

// parseTaskAddArgs parses fields after "!task add".
// Returns (cronExpr, script, dependsOn, isTask, prompt, error).
// Supports: 5-field cron tokens, duration shorthand, --script, --depends-on flags, task keyword.
func parseTaskAddArgs(args []string) (cronExpr, script string, dependsOn []string, isTask bool, prompt string, err error) {
	if len(args) == 0 {
		return "", "", nil, false, "", fmt.Errorf("인수가 부족합니다")
	}

	// Extract --script and --depends-on flags from args before parsing cron/prompt
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--script":
			if i+1 >= len(args) {
				return "", "", nil, false, "", fmt.Errorf("--script 플래그에 값이 필요합니다")
			}
			script = args[i+1]
			i++
		case "--depends-on":
			if i+1 >= len(args) {
				return "", "", nil, false, "", fmt.Errorf("--depends-on 플래그에 값이 필요합니다")
			}
			dependsOn = parseDependsOn(args[i+1])
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	args = rest

	if len(args) == 0 {
		return "", "", nil, false, "", fmt.Errorf("cron 식 또는 주기를 입력하세요")
	}

	// Determine cron expression
	var cronEnd int
	if len(args) >= 5 && allCronFields(args[0:5]) {
		// 5-field cron: "0 9 * * 1-5"
		cronExpr = strings.Join(args[0:5], " ")
		cronEnd = 5
	} else if args[0] == "@every" && len(args) >= 2 {
		cronExpr = "@every " + args[1]
		cronEnd = 2
	} else {
		// Duration shorthand: 30m, 2h, daily, etc.
		dur, _, pErr := ParseSchedule(args[0])
		if pErr != nil {
			return "", "", nil, false, "", fmt.Errorf("주기 형식 오류 (%q): %v\n예) 30m, 2h, daily, 또는 5-field cron (0 9 * * 1-5)", args[0], pErr)
		}
		cronExpr = durationToCron(dur)
		cronEnd = 1
	}

	remaining := args[cronEnd:]
	if len(remaining) == 0 {
		return "", "", nil, false, "", fmt.Errorf("프롬프트가 없습니다")
	}

	// Optional "task" keyword
	if remaining[0] == "task" {
		isTask = true
		remaining = remaining[1:]
	}

	if len(remaining) == 0 {
		return "", "", nil, false, "", fmt.Errorf("프롬프트가 없습니다")
	}
	prompt = strings.Join(remaining, " ")
	return cronExpr, script, dependsOn, isTask, prompt, nil
}

// allCronFields returns true if all 5 tokens look like valid cron expression fields.
func allCronFields(tokens []string) bool {
	if len(tokens) < 5 {
		return false
	}
	for _, t := range tokens[:5] {
		for _, c := range t {
			if (c < '0' || c > '9') && c != '*' && c != '/' && c != '-' && c != ',' && c != '?' {
				return false
			}
		}
		if t == "" {
			return false
		}
	}
	return true
}

// parseOnceDatetime parses "HH:MM" or "YYYY-MM-DD HH:MM" from the start of tokens.
// Returns (fireAt, tokensConsumed, error).
func parseOnceDatetime(tokens []string) (time.Time, int, error) {
	if len(tokens) == 0 {
		return time.Time{}, 0, fmt.Errorf("시각 없음")
	}
	now := time.Now()
	// Try "HH:MM"
	if t, err := time.ParseInLocation("15:04", tokens[0], time.Local); err == nil {
		fireAt := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if fireAt.Before(now) {
			fireAt = fireAt.Add(24 * time.Hour)
		}
		return fireAt, 1, nil
	}
	// Try "YYYY-MM-DD HH:MM" (2 tokens)
	if len(tokens) >= 2 {
		combined := tokens[0] + " " + tokens[1]
		if t, err := time.ParseInLocation("2006-01-02 15:04", combined, time.Local); err == nil {
			if t.Before(time.Now()) {
				return time.Time{}, 0, fmt.Errorf("과거 날짜입니다: %s", combined)
			}
			return t, 2, nil
		}
	}
	return time.Time{}, 0, fmt.Errorf("%q — HH:MM 또는 YYYY-MM-DD HH:MM 형식으로 입력하세요", tokens[0])
}

// parseFlags4 extracts up to 4 named flag values from tokens.
func parseFlags4(tokens []string, flag1, flag2, flag3, flag4 string) (v1, v2, v3, v4 string) {
	known := map[string]*string{flag1: &v1, flag2: &v2, flag3: &v3, flag4: &v4}
	i := 0
	for i < len(tokens) {
		dest, ok := known[tokens[i]]
		if !ok {
			i++
			continue
		}
		i++
		var parts []string
		for i < len(tokens) {
			if _, isFlag := known[tokens[i]]; isFlag {
				break
			}
			parts = append(parts, tokens[i])
			i++
		}
		if len(parts) > 0 {
			*dest = strings.Join(parts, " ")
		}
	}
	return
}

// parseDependsOn splits a comma-separated list of task IDs.
func parseDependsOn(raw string) []string {
	var out []string
	for _, id := range strings.Split(raw, ",") {
		if id := strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// parseFlags extracts up to 3 named flag values from tokens.
// A flag's value spans from the flag to the next recognized flag (or end of tokens),
// so "--cron 0 9 * * 1-5 --prompt foo" correctly captures "0 9 * * 1-5" as the cron value.
func parseFlags(tokens []string, flag1, flag2, flag3 string) (v1, v2, v3 string) {
	known := map[string]*string{flag1: &v1, flag2: &v2, flag3: &v3}
	i := 0
	for i < len(tokens) {
		dest, ok := known[tokens[i]]
		if !ok {
			i++
			continue
		}
		i++
		var parts []string
		for i < len(tokens) {
			if _, isFlag := known[tokens[i]]; isFlag {
				break
			}
			parts = append(parts, tokens[i])
			i++
		}
		if len(parts) > 0 {
			*dest = strings.Join(parts, " ")
		}
	}
	return
}

func taskHelpText() string {
	return strings.TrimSpace(`
📋 !task — 통합 스케줄 관리

등록:
!task add <주기> [task] <프롬프트>
  주기: 30m, 2h, 1d, 1w, 매시간, 매일, 매주, 또는 5-field cron (0 9 * * 1-5)
  task 키워드 있으면 Claude 작업, 없으면 알림
  예) !task add 매일 task 오늘 요약해줘
  예) !task add daily task 오늘 요약해줘
  예) !task add 0 9 * * 1-5 task 주식 확인

스크립트 사전확인:
!task add <주기> --script <bash_expr> [task] <프롬프트>
  스크립트가 {"wakeAgent":true} 반환할 때만 실행

일회성:
!task once <HH:MM|YYYY-MM-DD HH:MM> <메시지>
  예) !task once 09:00 아침 회의 준비해줘
  예) !task once 2026-06-12 14:30 배포 확인해줘

관리:
!task list [pending|paused|all]
!task pause <id>      — 일시정지
!task resume <id>     — 재개
!task cancel <id>     — 취소
!task update <id> [--cron <식>] [--prompt <텍스트>] [--script <스크립트>]
`)
}

// hasAttachment returns true if the message contains a downloadable file.
func (b *Bot) hasAttachment(msg *tgbotapi.Message) bool {
	return len(msg.Photo) > 0 || msg.Document != nil || msg.Video != nil ||
		msg.Audio != nil || msg.Voice != nil
}

// handleAttachment downloads the attached file, saves it to ~/.teleclaude/attachments/,
// and dispatches a combined prompt (caption + file path) to Claude.
func (b *Bot) handleAttachment(chatID int64, msg *tgbotapi.Message) {
	caption := strings.TrimSpace(msg.Caption)

	fileID, ext := attachFileInfo(msg)
	if fileID == "" {
		if caption != "" {
			b.dispatchText(chatID, caption)
		}
		return
	}

	savePath, err := b.downloadAttachment(fileID, ext)
	if err != nil {
		log.Printf("[bot] attachment download failed: %v", err)
		_ = b.Send(chatID, "⚠️ 첨부파일 다운로드 실패: "+err.Error())
		return
	}

	prompt := caption
	if prompt == "" {
		prompt = "첨부파일을 분석해줘"
	}
	prompt = prompt + "\n\n[첨부파일: " + savePath + "]"
	b.dispatchText(chatID, prompt)
}

// attachFileInfo extracts the Telegram file ID and extension for the first attachment found.
func attachFileInfo(msg *tgbotapi.Message) (fileID, ext string) {
	if len(msg.Photo) > 0 {
		// Use the last (highest-resolution) photo size.
		return msg.Photo[len(msg.Photo)-1].FileID, ".jpg"
	}
	if msg.Document != nil {
		ext := filepath.Ext(msg.Document.FileName)
		if ext == "" {
			ext = extFromMIME(msg.Document.MimeType)
		}
		return msg.Document.FileID, ext
	}
	if msg.Video != nil {
		return msg.Video.FileID, ".mp4"
	}
	if msg.Audio != nil {
		return msg.Audio.FileID, ".mp3"
	}
	if msg.Voice != nil {
		return msg.Voice.FileID, ".ogg"
	}
	return "", ""
}

// downloadAttachment fetches a Telegram file by ID and saves it to the attachments directory.
func (b *Bot) downloadAttachment(fileID, ext string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".teleclaude", "attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("첨부파일 디렉터리 생성 실패: %w", err)
	}

	tgFile, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("Telegram 파일 정보 조회 실패: %w", err)
	}
	url := tgFile.Link(b.api.Token)

	dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dlCancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("요청 생성 실패: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("파일 다운로드 실패: %w", err)
	}
	defer resp.Body.Close()

	saveName := fmt.Sprintf("%d%s", time.Now().UnixMilli(), ext)
	savePath := filepath.Join(dir, saveName)
	f, err := os.Create(savePath)
	if err != nil {
		return "", fmt.Errorf("파일 저장 실패: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("파일 쓰기 실패: %w", err)
	}
	log.Printf("[bot] attachment saved: %s", savePath)
	return savePath, nil
}

// extFromMIME returns a file extension guess from a MIME type.
func extFromMIME(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "application/zip":
		return ".zip"
	default:
		return ".bin"
	}
}

// handleHistory processes !history commands — date-based conversation log viewer.
//
//	!history                          — today's log for active project
//	!history list [project|all]       — list available dates (all = all projects)
//	!history <YYYY-MM-DD>             — specific date, active project
//	!history <project>                — today's log for named project
//	!history <project> <YYYY-MM-DD>   — specific project + date
func (b *Bot) handleHistory(chatID int64, fields []string) {
	active := b.store.GetActive()
	defaultProject := active.Project

	if len(fields) >= 2 && fields[1] == "list" {
		// "!history list all" → list all projects that have history
		if len(fields) >= 3 && fields[2] == "all" {
			projects, err := ListHistoryProjects()
			if err != nil {
				_ = b.Send(chatID, "⚠️ 히스토리 프로젝트 목록 조회 실패: "+err.Error())
				return
			}
			if len(projects) == 0 {
				_ = b.Send(chatID, "📅 기록된 히스토리가 없습니다.")
				return
			}
			_ = b.Send(chatID, "📅 히스토리가 있는 프로젝트:\n"+strings.Join(projects, "\n"))
			return
		}

		project := defaultProject
		if len(fields) >= 3 {
			project = fields[2]
		}
		if project == "" {
			_ = b.Send(chatID, "활성 프로젝트가 없습니다. !history list <프로젝트명> 또는 !history list all")
			return
		}
		dates, err := ListHistoryDates(project)
		if err != nil {
			_ = b.Send(chatID, "⚠️ 히스토리 목록 조회 실패: "+err.Error())
			return
		}
		if len(dates) == 0 {
			_ = b.Send(chatID, "📅 "+project+": 기록된 날짜 없음")
			return
		}
		_ = b.Send(chatID, "📅 "+project+" 히스토리 날짜:\n"+strings.Join(dates, "\n"))
		return
	}

	// Parse: !history [project] [YYYY-MM-DD]
	project := defaultProject
	date := time.Now().Format("2006-01-02")
	for _, arg := range fields[1:] {
		if len(arg) == 10 && arg[4] == '-' && arg[7] == '-' {
			if _, err := time.Parse("2006-01-02", arg); err != nil {
				_ = b.Send(chatID, "⚠️ 날짜 형식 오류: "+arg+" (YYYY-MM-DD 사용)")
				return
			}
			date = arg
		} else {
			project = arg
		}
	}

	if project == "" {
		_ = b.Send(chatID, "활성 프로젝트가 없습니다.\n!history list all 로 기록이 있는 프로젝트를 확인하거나 !history <프로젝트명> 형식으로 사용하세요.")
		return
	}

	content, err := ReadHistory(project, date)
	if err != nil {
		_ = b.Send(chatID, "⚠️ 히스토리 조회 실패: "+err.Error())
		return
	}
	if content == "" {
		_ = b.Send(chatID, fmt.Sprintf("📅 %s / %s: 기록 없음", project, date))
		return
	}
	// Telegram limit is 4096 chars; header "📅 <project> / <date>:\n" can be ~60 chars.
	const maxContent = 3900
	if len([]rune(content)) > maxContent {
		runes := []rune(content)
		content = string(runes[:maxContent]) + "\n...(잘림)"
	}
	_ = b.Send(chatID, fmt.Sprintf("📅 %s / %s:\n%s", project, date, content))
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
	active := b.activeCount
	b.mu.Unlock()
	if active > 0 {
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

// handleParallel dispatches multiple independent prompts concurrently.
// Syntax: !parallel <prompt1> | <prompt2> | ...
// Each |-separated prompt becomes its own worker; responses arrive independently.
func (b *Bot) handleParallel(chatID int64, text string) {
	rest := strings.TrimSpace(strings.TrimPrefix(text, "!parallel"))
	if rest == "" {
		_ = b.Send(chatID, "사용법: !parallel <프롬프트1> | <프롬프트2> | ...\n예) !parallel 테스트 작성해줘 | 문서 업데이트해줘")
		return
	}
	parts := strings.Split(rest, "|")
	var prompts []string
	for _, p := range parts {
		if p := strings.TrimSpace(p); p != "" {
			prompts = append(prompts, p)
		}
	}
	if len(prompts) == 0 {
		_ = b.Send(chatID, "⚠️ 유효한 프롬프트가 없습니다.")
		return
	}
	// Cap to MaxWorkers to prevent unbounded resource / rate-limit bypass.
	maxP := b.cfg.MaxWorkers
	if maxP < 1 {
		maxP = 1
	}
	if len(prompts) > maxP {
		_ = b.Send(chatID, fmt.Sprintf("⚠️ !parallel 최대 %d개까지 허용됩니다 (%d개 입력됨). 앞의 %d개만 실행합니다.", maxP, len(prompts), maxP))
		prompts = prompts[:maxP]
	}
	if len(prompts) == 1 {
		b.dispatchText(chatID, prompts[0])
		return
	}
	_ = b.Send(chatID, fmt.Sprintf("🔀 %d개 병렬 작업 시작합니다...", len(prompts)))
	for _, p := range prompts {
		b.dispatchText(chatID, p)
	}
}

// removeInt64 returns a copy of ids without the given value.
func removeInt64(ids []int64, v int64) []int64 {
	out := ids[:0:0]
	for _, id := range ids {
		if id != v {
			out = append(out, id)
		}
	}
	return out
}

// handleUser manages the runtime allow-list: !user add <id> | remove <id> | list
func (b *Bot) handleUser(chatID int64, fields []string) {
	if b.userStore == nil {
		_ = b.Send(chatID, "⚠️ UserStore를 사용할 수 없습니다.")
		return
	}
	if len(fields) < 2 {
		_ = b.Send(chatID, "사용법: !user add <id> | !user remove <id> | !user list")
		return
	}
	switch fields[1] {
	case "add":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !user add <telegram_user_id>")
			return
		}
		var id int64
		if _, err := fmt.Sscanf(fields[2], "%d", &id); err != nil {
			_ = b.Send(chatID, "⚠️ 잘못된 사용자 ID (숫자 입력 필요): "+fields[2])
			return
		}
		if err := b.userStore.Add(id); err != nil {
			_ = b.Send(chatID, "⚠️ 저장 실패: "+err.Error())
			return
		}
		_ = b.Send(chatID, fmt.Sprintf("✅ 사용자 추가됨: %d", id))
	case "remove":
		if len(fields) < 3 {
			_ = b.Send(chatID, "사용법: !user remove <telegram_user_id>")
			return
		}
		var id int64
		if _, err := fmt.Sscanf(fields[2], "%d", &id); err != nil {
			_ = b.Send(chatID, "⚠️ 잘못된 사용자 ID: "+fields[2])
			return
		}
		if b.cfg.IsAllowed(id) {
			_ = b.Send(chatID, "⚠️ config.txt AllowedUserIDs에 있는 사용자는 !user remove로 제거할 수 없습니다.")
			return
		}
		// Lockout guard: refuse if removing this ID would leave no allowed users.
		runtimeAfter := b.userStore.List()
		runtimeAfter = removeInt64(runtimeAfter, id)
		if len(b.cfg.AllowedUserIDs) == 0 && len(b.cfg.AllowedUsernames) == 0 && len(runtimeAfter) == 0 {
			_ = b.Send(chatID, "⚠️ 이 사용자를 제거하면 허용된 사용자가 없어져 봇이 잠깁니다. 먼저 다른 사용자를 추가하세요.")
			return
		}
		if err := b.userStore.Remove(id); err != nil {
			_ = b.Send(chatID, "⚠️ 저장 실패: "+err.Error())
			return
		}
		_ = b.Send(chatID, fmt.Sprintf("🗑 사용자 제거됨: %d", id))
	case "list":
		var sb strings.Builder
		sb.WriteString("👥 허용된 사용자:\n")
		sb.WriteString("  [config] IDs: ")
		for i, id := range b.cfg.AllowedUserIDs {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%d", id)
		}
		if len(b.cfg.AllowedUsernames) > 0 {
			sb.WriteString("\n  [config] 유저네임: @" + strings.Join(b.cfg.AllowedUsernames, ", @"))
		}
		runtimeIDs := b.userStore.List()
		if len(runtimeIDs) > 0 {
			sb.WriteString("\n  [runtime] IDs: ")
			for i, id := range runtimeIDs {
				if i > 0 {
					sb.WriteString(", ")
				}
				fmt.Fprintf(&sb, "%d", id)
			}
		} else {
			sb.WriteString("\n  [runtime] 없음")
		}
		_ = b.Send(chatID, sb.String())
	default:
		_ = b.Send(chatID, "사용법: !user add <id> | !user remove <id> | !user list")
	}
}

func helpText() string {
	return strings.TrimSpace(`
🤖 teleclaude — 폰에서 PC의 Claude를 자연어로 쓰세요.

그냥 말하세요. 예) "myapp 로그인 버그 이어서 보자", "voice 서버에 헬스체크 추가해줘"
→ 어느 프로젝트의 어느 대화인지 알아서 찾아 작업합니다.
사진·파일 첨부: 그냥 보내면 Claude가 분석합니다.

명령어:
!project add <이름> <경로>   프로젝트 등록
!project remove <이름>       프로젝트 제거
!project list                프로젝트·대화 목록
!chat new [제목]             현재 프로젝트에 새 대화
!chat list                   현재 프로젝트의 대화 목록
!chat use <id>               대화 수동 전환
!status                      실행 중 작업 + 활성 대화 + 백엔드
!cancel                      진행 중 작업 취소

병렬 작업:
!parallel <p1> | <p2> | ...  여러 프롬프트를 동시에 Claude에 전달

스케줄 (통합):
!task add <주기|cron> [task] <내용>         반복 작업/알림 등록
  주기: 30m 2h 1d 1w 매시간 매일 매주
  --depends-on <id,...>   다른 작업 완료 후 실행 (DAG)
!task once <HH:MM|YYYY-MM-DD HH:MM> <내용>  일회성 알림
!task list [pending|paused|all]             목록
!task pause|resume|cancel <id>              관리
!task update <id> --cron|--prompt|--script|--depends-on <값>
!task help                                  상세 도움말

히스토리:
!history [프로젝트] [YYYY-MM-DD]      대화 기록 조회
!history list [프로젝트|all]          날짜 목록 (all = 전체 프로젝트)

사용자 관리:
!user add <id>               런타임 허용 사용자 추가 (재시작 후에도 유지)
!user remove <id>            런타임 허용 사용자 제거
!user list                   허용 사용자 목록 (config + runtime)

기타:
!remind <시간> <메시지>      일회성 알림 (구버전 호환)
!cron add|list|remove        반복 작업 (구버전 호환)
!backend [claude|codex]      AI 백엔드 전환
!update                      새 버전 빌드 & 자동 재시작
!help                        이 도움말
`)
}
