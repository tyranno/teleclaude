package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
		var override string
		if len(args) > 1 {
			override = args[1]
		}
		if err := run(override); err != nil {
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

func run(configOverride string) error {
	cfgPath := configOverride
	if cfgPath == "" {
		p, err := defaultConfigPath()
		if err != nil {
			return err
		}
		cfgPath = p
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

	bot := NewBot(api, cfg, store, manager)
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
