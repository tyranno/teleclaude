package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// teleclaude — Telegram ↔ Claude agent for Windows (MVP).
// Design Ref: §11 — wiring/assembly + claude health check.

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "run":
		var configPath, handoffFile, notifyChat string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--handoff-ready":
				if i+1 < len(args) {
					handoffFile = args[i+1]
					i++
				}
			case "--notify-chat":
				if i+1 < len(args) {
					notifyChat = args[i+1]
					i++
				}
			default:
				configPath = args[i]
			}
		}
		if err := run(configPath, handoffFile, notifyChat); err != nil {
			log.Fatalf("fatal: %v", err)
		}
	case "setup":
		var override string
		if len(args) > 1 {
			override = args[1]
		}
		path := override
		if path == "" {
			p, e := defaultConfigPath()
			if e != nil {
				log.Fatal(e)
			}
			path = p
		}
		if err := RunSetup(path); err != nil {
			log.Fatalf("설정 마법사 중단: %v", err)
		}
	case "version", "--version", "-v":
		fmt.Println("teleclaude 0.1.0")
	default:
		fmt.Println("usage: teleclaude [run [config-path]] | setup [config-path] | version")
	}
}

// killCompetingInstances terminates other teleclaude.exe processes (not self).
// Only called on normal startup, not during handoff (where old process self-exits via ready signal).
func killCompetingInstances() {
	selfPID := strconv.Itoa(os.Getpid())
	out, err := exec.Command("taskkill", "/F",
		"/FI", "IMAGENAME eq teleclaude.exe",
		"/FI", "PID ne "+selfPID,
	).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err == nil && !strings.Contains(strings.ToLower(msg), "no tasks") {
		log.Printf("[main] terminated competing teleclaude instance(s)")
		time.Sleep(600 * time.Millisecond) // let Telegram polling clear before we start ours
	}
}

// selfRename renames the running exe to teleclaude.exe (used after handoff).
// Windows allows renaming a running executable; retries until old process releases the name.
func selfRename() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	target := filepath.Join(filepath.Dir(exe), "teleclaude.exe")
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		if err := os.Rename(exe, target); err == nil {
			log.Printf("[main] self-renamed to teleclaude.exe")
			return
		}
	}
	log.Printf("[main] self-rename skipped")
}

func run(configOverride, handoffReadyFile, notifyChat string) error {
	cfgPath := configOverride
	if cfgPath == "" {
		p, err := defaultConfigPath()
		if err != nil {
			return err
		}
		cfgPath = p
	}

	// In normal startup (not handoff), terminate any competing instance first.
	// In handoff mode the old process exits via ready-signal, so we skip this.
	if handoffReadyFile == "" {
		killCompetingInstances()
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		// No (or incomplete) config → run the interactive wizard, then reload.
		if !isInteractive() {
			return fmt.Errorf("%w\n대화형 터미널에서 `teleclaude setup`을 먼저 실행하세요 (%s)", err, cfgPath)
		}
		fmt.Println("⚙️  설정이 없거나 불완전합니다. 설정 마법사를 시작합니다.")
		if serr := RunSetup(cfgPath); serr != nil {
			return fmt.Errorf("설정 마법사 중단: %w", serr)
		}
		cfg, err = LoadConfig(cfgPath)
		if err != nil {
			return err
		}
	}

	claudePath, err := findClaude(cfg.ClaudePath)
	if err != nil {
		return err
	}
	log.Printf("[main] claude: %s", claudePath)
	if err := claudeHealthCheck(claudePath); err != nil {
		return fmt.Errorf("claude 헬스체크 실패: %w", err)
	}

	dir, err := dataDir()
	if err != nil {
		return err
	}
	store := NewFileStore(filepath.Join(dir, "store.json"))
	if err := store.Load(); err != nil {
		return fmt.Errorf("대화 저장소 로드 실패: %w", err)
	}

	runner := NewClaudeRunner(claudePath, cfg)
	manager := NewManager(runner, store, cfg)

	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return fmt.Errorf("텔레그램 봇 초기화 실패: %w", err)
	}
	log.Printf("[main] allowlist: %v, manager=%s, worker=%q", cfg.AllowedUserIDs, cfg.ManagerModel, cfg.WorkerModel)

	// Scheduler: reminders + cron jobs
	sched := NewScheduler(filepath.Join(dir, "schedule.json"))
	if err := sched.Load(); err != nil {
		log.Printf("[main] scheduler load warning: %v", err)
	}

	bot := NewBot(api, cfg, store, manager, sched)

	// Wire scheduler send/dispatch after bot is created
	sched.SetSend(func(chatID int64, text string) { _ = bot.Send(chatID, text) })
	sched.SetDispatch(func(chatID int64, text string) { bot.dispatchText(chatID, text) })
	manager.SetScheduler(sched)
	go sched.Run()

	// Handoff mode: signal old process AFTER polling starts (GetUpdatesChan).
	// This ensures no polling gap between old exit and new start.
	// Also sends a Telegram notification so user knows handoff succeeded.
	if handoffReadyFile != "" {
		var notifyChatID int64
		if notifyChat != "" {
			notifyChatID, _ = strconv.ParseInt(notifyChat, 10, 64)
		}
		bot.onReady = func() {
			if werr := os.WriteFile(handoffReadyFile, []byte("ready"), 0600); werr != nil {
				log.Printf("[main] handoff signal failed: %v", werr)
			} else {
				log.Printf("[main] handoff: signaled ready — polling active")
			}
			if notifyChatID != 0 {
				_ = bot.Send(notifyChatID, "✅ 새 버전 활성화됨! (백그라운드 실행 중)")
			}
			go selfRename()
		}
	}

	bot.Run() // blocks
	return nil
}

// claudeHealthCheck verifies the claude CLI responds.
func claudeHealthCheck(claudePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, claudePath, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, string(out))
	}
	log.Printf("[main] claude version: %s", string(out))
	return nil
}
