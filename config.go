package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Design Ref: §4.2, §8.3 — config.txt (key=value) parsing + claude path auto-detect.

// dataDir returns %USERPROFILE%\.teleclaude (created if missing).
func dataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".teleclaude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// defaultConfigPath returns the standard config.txt location.
func defaultConfigPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.txt"), nil
}

// defaultYAMLPath returns ~/.teleclaude/config.yaml.
func defaultYAMLPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// LoadOrMigrate loads config.yaml from dir; if absent but config.txt exists,
// migrates it to config.yaml (and renames the txt to .bak). Returns the path used.
func LoadOrMigrate(dir string) (*Config, string, error) {
	yamlPath := filepath.Join(dir, "config.yaml")
	txtPath := filepath.Join(dir, "config.txt")

	if b, err := os.ReadFile(yamlPath); err == nil {
		cfg, perr := unmarshalConfigYAML(b)
		return cfg, yamlPath, perr
	}
	// No YAML — try migrating from txt.
	if _, err := os.Stat(txtPath); err == nil {
		cfg, lerr := LoadConfig(txtPath)
		if lerr != nil {
			return nil, txtPath, lerr
		}
		out, merr := marshalConfigYAML(cfg)
		if merr != nil {
			return nil, txtPath, merr
		}
		if werr := os.WriteFile(yamlPath, out, 0o600); werr != nil {
			return nil, txtPath, werr
		}
		if rerr := os.Rename(txtPath, txtPath+".bak"); rerr != nil {
			log.Printf("[config] config.txt → .bak 이름변경 실패(무시): %v", rerr)
		}
		log.Printf("[config] config.txt → config.yaml 마이그레이션 완료 (txt는 .bak로 보존)")
		return cfg, yamlPath, nil
	}
	return nil, yamlPath, fmt.Errorf("설정 파일이 없습니다: %s 또는 %s", yamlPath, txtPath)
}

// LoadConfig parses a key=value config file. Lines starting with # are comments.
// If path ends in .yaml/.yml it is parsed as YAML instead.
func LoadConfig(path string) (*Config, error) {
	if strings.HasSuffix(strings.ToLower(path), ".yaml") || strings.HasSuffix(strings.ToLower(path), ".yml") {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("설정 파일 열기 실패 (%s): %w", path, err)
		}
		return unmarshalConfigYAML(b)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("설정 파일 열기 실패 (%s): %w", path, err)
	}
	defer f.Close()

	cfg := &Config{
		ManagerModel:    "haiku",
		TimeoutMinutes:  10,
		ManagerAlways:   true,
		MaxWorkers:      3,
		RateLimitPerMin: 20,
		AllowScripts:    false,
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if err := applyConfigKV(cfg, key, val); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyConfigKV(cfg *Config, key, val string) error {
	switch strings.ToUpper(key) {
	case "TELEGRAM_BOT_TOKEN":
		cfg.TelegramBotToken = val
	case "ALLOWED_USER_IDS":
		ids, err := parseUserIDs(val)
		if err != nil {
			return err
		}
		cfg.AllowedUserIDs = ids
	case "MANAGER_MODEL":
		if val != "" {
			cfg.ManagerModel = val
		}
	case "WORKER_MODEL":
		cfg.WorkerModel = val
	case "CLAUDE_PATH":
		cfg.ClaudePath = val
	case "CLAUDE_CODE_OAUTH_TOKEN":
		cfg.ClaudeOauthToken = val
	case "TIMEOUT_MINUTES":
		if val != "" {
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return fmt.Errorf("TIMEOUT_MINUTES는 양의 정수여야 합니다: %q", val)
			}
			cfg.TimeoutMinutes = n
		}
	case "MANAGER_ALWAYS":
		cfg.ManagerAlways = parseBool(val, true)
	case "CODEX_PATH":
		cfg.CodexPath = val
	case "CODEX_MODEL":
		cfg.CodexModel = val
	case "CODEX_MANAGER_MODEL":
		cfg.CodexManagerModel = val
	case "DEFAULT_BACKEND":
		cfg.DefaultBackend = strings.ToLower(val)
	case "MAX_WORKERS":
		if val != "" {
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return fmt.Errorf("MAX_WORKERS는 양의 정수여야 합니다: %q", val)
			}
			cfg.MaxWorkers = n
		}
	case "RATE_LIMIT_PER_MIN":
		if val != "" {
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return fmt.Errorf("RATE_LIMIT_PER_MIN는 0 이상 정수여야 합니다: %q", val)
			}
			cfg.RateLimitPerMin = n
		}
	case "ALLOW_SCRIPTS":
		cfg.AllowScripts = parseBool(val, false)
	case "ALLOWED_SCRIPT_COMMANDS":
		for _, cmd := range strings.Split(val, ",") {
			if c := strings.TrimSpace(cmd); c != "" {
				cfg.AllowedScriptCommands = append(cfg.AllowedScriptCommands, c)
			}
		}
	case "ALLOWED_USERNAMES":
		for _, u := range strings.Split(val, ",") {
			if name := strings.TrimPrefix(strings.TrimSpace(u), "@"); name != "" {
				cfg.AllowedUsernames = append(cfg.AllowedUsernames, name)
			}
		}
	}
	return nil
}

func parseUserIDs(val string) ([]int64, error) {
	var ids []int64
	for _, p := range strings.Split(val, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ALLOWED_USER_IDS에 잘못된 값: %q", p)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseBool(val string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}

const maxAllowedWorkers = 50

func (c *Config) validate() error {
	if c.TelegramBotToken == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN이 설정되지 않았습니다")
	}
	if len(c.AllowedUserIDs) == 0 {
		return fmt.Errorf("ALLOWED_USER_IDS가 비어 있습니다 (보안상 최소 1개 필요)")
	}
	if c.DefaultBackend != "" && c.DefaultBackend != "claude" && c.DefaultBackend != "codex" {
		return fmt.Errorf("DEFAULT_BACKEND는 'claude' 또는 'codex'여야 합니다: %q", c.DefaultBackend)
	}
	if c.MaxWorkers > maxAllowedWorkers {
		return fmt.Errorf("MAX_WORKERS는 최대 %d까지 허용됩니다 (입력값: %d)", maxAllowedWorkers, c.MaxWorkers)
	}
	return nil
}

// IsAllowed reports whether the given Telegram user ID may use the bot.
func (c *Config) IsAllowed(userID int64) bool {
	return slices.Contains(c.AllowedUserIDs, userID)
}

// IsAllowedByUsername reports whether the given Telegram username (without @) may use the bot.
// Returns false when username is empty.
func (c *Config) IsAllowedByUsername(username string) bool {
	if username == "" {
		return false
	}
	return slices.Contains(c.AllowedUsernames, username)
}

// findClaude resolves the claude CLI path: explicit > PATH > platform-specific locations.
func findClaude(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("CLAUDE_PATH가 존재하지 않습니다: %s", explicit)
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, c := range findClaudeOS(home) {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("claude CLI를 찾을 수 없습니다. PATH에 추가하거나 CLAUDE_PATH를 설정하세요")
}

// findCodex returns the codex CLI path (explicit override or PATH lookup).
// Returns ("", nil) if not installed — codex is optional.
func findCodex(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("codex 경로 없음: %s", explicit)
		}
		return explicit, nil
	}
	p, err := exec.LookPath("codex")
	if err != nil {
		return "", nil // not installed — not an error
	}
	return p, nil
}
