# Phase 0 — YAML Config + Migration + Hot-Reload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** teleclaude 설정을 `config.txt`(key=value)에서 `config.yaml`(섹션)로 전환하고, 기존 config.txt를 자동 마이그레이션하며, fsnotify로 설정 변경을 무재시작 적용한다.

**Architecture:** YAML은 `yamlConfig` 중간 구조체로 파싱 후 기존 `Config`로 평탄화(검증 로직 재사용). 런타임 설정은 `ConfigHolder`(`atomic.Pointer[Config]`)로 보관하고, Bot/Manager/claudeRunner는 holder에서 매 사용 시 스냅샷(`x.cfg()`)을 읽는다. fsnotify가 `~/.teleclaude` 디렉토리를 감시해 `config.yaml` 변경 시 재로드→검증→holder.Set→적용 콜백.

**Tech Stack:** Go 1.25, `gopkg.in/yaml.v3`, `github.com/fsnotify/fsnotify`, 표준 `sync/atomic`.

## Global Constraints

- 언어/런타임: Go 1.25, **CGO 없이** (순수 Go 의존성만).
- 기존 `Config` 구조체 필드는 유지(소스 포맷만 변경). `validate()` 재사용.
- 하위호환: `config.txt` 파서/관련 테스트는 **삭제 금지**(마이그레이션 경로로 유지).
- 설정 파일 위치: `%USERPROFILE%/~$HOME/.teleclaude/config.yaml` (`dataDir()` 사용).
- 모든 신규 공개 동작은 단위 테스트 동반. `go test ./...`, `go vet ./...`, `gofmt` 클린 유지.
- 커밋 메시지 말미: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`

---

## File Structure

| 파일 | 책임 |
|------|------|
| `types.go` (수정) | `Config`에 `ScreenControl bool`, `ScreenPresetsFile string` 추가 |
| `config_yaml.go` (신규) | `yamlConfig` 구조체, `configToYAML`/`yamlToConfig`, `marshalConfigYAML` |
| `config.go` (수정) | `LoadConfig`: yaml 우선 로드 + txt 마이그레이션. 기존 key=value 파서 유지 |
| `confighold.go` (신규) | `ConfigHolder`(atomic.Pointer[Config]) + `Get/Set` |
| `confighot.go` (신규) | fsnotify watcher(디렉토리 감시+디바운스) + reload + apply 콜백 |
| `security.go` (수정) | `RateLimiter.SetLimit(int)` 추가 |
| `setup.go` (수정) | `writeConfigFile` → YAML 출력 |
| `main.go` (수정) | holder 생성, watcher 기동, 컴포넌트에 holder 전달 |
| `bot.go` (수정) | `cfg *Config` → `cfgh *ConfigHolder` + `cfg()` 접근자, `b.cfg.`→`b.cfg().`, rate limiter 갱신 연결 |
| `manager.go` (수정) | `cfg *Config` → holder + `cfg()` 접근자, `m.cfg.`→`m.cfg().` |
| `runner.go` (수정) | `cfg *Config` → holder + `cfg()` 접근자 |
| 관련 `*_test.go` (수정) | 생성자/필드 접근을 holder에 맞게 갱신 |

---

## Task 1: Config에 screen_control 필드 + YAML 변환 계층

**Files:**
- Modify: `types.go` (Config 구조체)
- Create: `config_yaml.go`
- Test: `config_yaml_test.go`

**Interfaces:**
- Produces: `yamlConfig` struct; `func yamlToConfig(y *yamlConfig) *Config`; `func configToYAML(c *Config) *yamlConfig`; `func marshalConfigYAML(c *Config) ([]byte, error)`; `func unmarshalConfigYAML(b []byte) (*Config, error)`
- Consumes: 기존 `Config`(types.go), `validate()`(config.go)

- [ ] **Step 1: go.mod에 yaml 의존성 추가**

Run: `go get gopkg.in/yaml.v3@v3.0.1`
Expected: go.mod에 `gopkg.in/yaml.v3 v3.0.1` 추가됨

- [ ] **Step 2: Config에 screen_control 필드 추가**

`types.go`의 Config 구조체에 두 줄 추가 (AllowedUsernames 다음):
```go
	AllowedUsernames      []string // Telegram usernames (without @) allowed to use the bot
	ScreenControl         bool     // screen-control MCP 활성화 (Windows). 기본 false
	ScreenPresetsFile     string   // 좌표 프리셋 파일 경로. 빈 값이면 ~/.teleclaude/presets.json
```

- [ ] **Step 3: 실패하는 테스트 작성 (YAML 라운드트립)**

Create `config_yaml_test.go`:
```go
package main

import "testing"

func TestYAMLRoundTrip(t *testing.T) {
	c := &Config{
		TelegramBotToken: "123:ABC",
		AllowedUserIDs:   []int64{111, 222},
		AllowedUsernames: []string{"alice"},
		ManagerModel:     "haiku",
		WorkerModel:      "sonnet",
		ManagerAlways:    false,
		ClaudePath:       "",
		ClaudeOauthToken: "sk-ant-oat01-X",
		DefaultBackend:   "claude",
		CodexModel:       "o4-mini",
		TimeoutMinutes:   10,
		MaxWorkers:       3,
		RateLimitPerMin:  20,
		AllowScripts:     false,
		ScreenControl:    true,
		ScreenPresetsFile: "",
	}
	b, err := marshalConfigYAML(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalConfigYAML(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.TelegramBotToken != c.TelegramBotToken ||
		len(got.AllowedUserIDs) != 2 || got.AllowedUserIDs[1] != 222 ||
		got.WorkerModel != "sonnet" || got.ManagerAlways != false ||
		got.ClaudeOauthToken != "sk-ant-oat01-X" || got.DefaultBackend != "claude" ||
		got.MaxWorkers != 3 || got.RateLimitPerMin != 20 || got.ScreenControl != true {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestYAMLDefaults(t *testing.T) {
	// Minimal YAML → defaults applied, validate passes.
	y := []byte("telegram:\n  bot_token: t\n  allowed_user_ids: [1]\n")
	got, err := unmarshalConfigYAML(y)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManagerModel != "haiku" || got.TimeoutMinutes != 10 || got.MaxWorkers != 3 ||
		got.RateLimitPerMin != 20 || got.ManagerAlways != true {
		t.Errorf("defaults wrong: %+v", got)
	}
}
```

- [ ] **Step 4: 테스트 실패 확인**

Run: `go test ./... -run 'TestYAML' -v`
Expected: FAIL (marshalConfigYAML/unmarshalConfigYAML undefined)

- [ ] **Step 5: config_yaml.go 구현**

Create `config_yaml.go`:
```go
package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlConfig is the on-disk YAML shape. Pointers/omitempty keep output tidy and
// let us detect "unset" so defaults apply.
type yamlConfig struct {
	Telegram struct {
		BotToken         string   `yaml:"bot_token"`
		AllowedUserIDs   []int64  `yaml:"allowed_user_ids"`
		AllowedUsernames []string `yaml:"allowed_usernames"`
	} `yaml:"telegram"`
	Models struct {
		Manager       string `yaml:"manager"`
		Worker        string `yaml:"worker"`
		ManagerAlways *bool  `yaml:"manager_always"`
	} `yaml:"models"`
	Claude struct {
		Path       string `yaml:"path"`
		OauthToken string `yaml:"oauth_token"`
	} `yaml:"claude"`
	Backend struct {
		Default           string `yaml:"default"`
		CodexPath         string `yaml:"codex_path"`
		CodexModel        string `yaml:"codex_model"`
		CodexManagerModel string `yaml:"codex_manager_model"`
	} `yaml:"backend"`
	Runtime struct {
		TimeoutMinutes  *int `yaml:"timeout_minutes"`
		MaxWorkers      *int `yaml:"max_workers"`
		RateLimitPerMin *int `yaml:"rate_limit_per_min"`
	} `yaml:"runtime"`
	Scripts struct {
		Allow           bool     `yaml:"allow"`
		AllowedCommands []string `yaml:"allowed_commands"`
	} `yaml:"scripts"`
	ScreenControl struct {
		Enabled     bool   `yaml:"enabled"`
		PresetsFile string `yaml:"presets_file"`
	} `yaml:"screen_control"`
}

// defaults mirror config.go LoadConfig defaults.
func yamlToConfig(y *yamlConfig) *Config {
	c := &Config{
		ManagerModel:    "haiku",
		TimeoutMinutes:  10,
		ManagerAlways:   true,
		MaxWorkers:      3,
		RateLimitPerMin: 20,
		AllowScripts:    false,
	}
	c.TelegramBotToken = y.Telegram.BotToken
	c.AllowedUserIDs = y.Telegram.AllowedUserIDs
	for _, u := range y.Telegram.AllowedUsernames {
		if name := strings.TrimPrefix(strings.TrimSpace(u), "@"); name != "" {
			c.AllowedUsernames = append(c.AllowedUsernames, name)
		}
	}
	if y.Models.Manager != "" {
		c.ManagerModel = y.Models.Manager
	}
	c.WorkerModel = y.Models.Worker
	if y.Models.ManagerAlways != nil {
		c.ManagerAlways = *y.Models.ManagerAlways
	}
	c.ClaudePath = y.Claude.Path
	c.ClaudeOauthToken = y.Claude.OauthToken
	c.DefaultBackend = strings.ToLower(y.Backend.Default)
	c.CodexPath = y.Backend.CodexPath
	c.CodexModel = y.Backend.CodexModel
	c.CodexManagerModel = y.Backend.CodexManagerModel
	if y.Runtime.TimeoutMinutes != nil {
		c.TimeoutMinutes = *y.Runtime.TimeoutMinutes
	}
	if y.Runtime.MaxWorkers != nil {
		c.MaxWorkers = *y.Runtime.MaxWorkers
	}
	if y.Runtime.RateLimitPerMin != nil {
		c.RateLimitPerMin = *y.Runtime.RateLimitPerMin
	}
	c.AllowScripts = y.Scripts.Allow
	c.AllowedScriptCommands = y.Scripts.AllowedCommands
	c.ScreenControl = y.ScreenControl.Enabled
	c.ScreenPresetsFile = y.ScreenControl.PresetsFile
	return c
}

func configToYAML(c *Config) *yamlConfig {
	y := &yamlConfig{}
	y.Telegram.BotToken = c.TelegramBotToken
	y.Telegram.AllowedUserIDs = c.AllowedUserIDs
	y.Telegram.AllowedUsernames = c.AllowedUsernames
	y.Models.Manager = c.ManagerModel
	y.Models.Worker = c.WorkerModel
	ma := c.ManagerAlways
	y.Models.ManagerAlways = &ma
	y.Claude.Path = c.ClaudePath
	y.Claude.OauthToken = c.ClaudeOauthToken
	y.Backend.Default = c.DefaultBackend
	y.Backend.CodexPath = c.CodexPath
	y.Backend.CodexModel = c.CodexModel
	y.Backend.CodexManagerModel = c.CodexManagerModel
	tm, mw, rl := c.TimeoutMinutes, c.MaxWorkers, c.RateLimitPerMin
	y.Runtime.TimeoutMinutes = &tm
	y.Runtime.MaxWorkers = &mw
	y.Runtime.RateLimitPerMin = &rl
	y.Scripts.Allow = c.AllowScripts
	y.Scripts.AllowedCommands = c.AllowedScriptCommands
	y.ScreenControl.Enabled = c.ScreenControl
	y.ScreenControl.PresetsFile = c.ScreenPresetsFile
	return y
}

func marshalConfigYAML(c *Config) ([]byte, error) {
	return yaml.Marshal(configToYAML(c))
}

// unmarshalConfigYAML parses YAML, applies defaults, and validates.
func unmarshalConfigYAML(b []byte) (*Config, error) {
	var y yamlConfig
	if err := yaml.Unmarshal(b, &y); err != nil {
		return nil, fmt.Errorf("config.yaml 파싱 실패: %w", err)
	}
	c := yamlToConfig(&y)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}
```

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./... -run 'TestYAML' -v && go vet ./...`
Expected: PASS

- [ ] **Step 7: 커밋**

```bash
gofmt -w types.go config_yaml.go config_yaml_test.go
git add go.mod go.sum types.go config_yaml.go config_yaml_test.go
git commit -m "feat(config): YAML schema + Config<->YAML conversion + screen_control fields

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: LoadConfig YAML 우선 + config.txt 자동 마이그레이션 + 마법사 YAML 출력

**Files:**
- Modify: `config.go` (`LoadConfig`, add `defaultYAMLPath`, `migrateTxtToYAML`)
- Modify: `setup.go` (`writeConfigFile` → YAML)
- Modify: `setup_test.go`, `iter27_test.go` (writeConfigFile은 YAML을 쓰므로 LoadConfig로 검증 — 시그니처 동일, 통과해야 함)
- Test: `config_migrate_test.go`

**Interfaces:**
- Consumes: `unmarshalConfigYAML`, `marshalConfigYAML` (Task 1), 기존 key=value 파서 `applyConfigKV`/`parseUserIDs` (config.go, 유지)
- Produces: `LoadConfig(path string) (*Config, error)` — path가 `.yaml`이면 YAML, `.txt`/기타면 기존 파서. `func LoadOrMigrate(dir string) (*Config, string, error)` — yaml 우선, 없으면 txt→yaml 마이그레이션, 사용된 최종 경로 반환.

- [ ] **Step 1: 실패 테스트 작성 (마이그레이션)**

Create `config_migrate_test.go`:
```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrMigrate_FromTxt(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "config.txt")
	os.WriteFile(txt, []byte("TELEGRAM_BOT_TOKEN=123:ABC\nALLOWED_USER_IDS=42\nWORKER_MODEL=sonnet\n"), 0o600)

	cfg, used, err := LoadOrMigrate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramBotToken != "123:ABC" || cfg.WorkerModel != "sonnet" {
		t.Errorf("cfg = %+v", cfg)
	}
	if filepath.Base(used) != "config.yaml" {
		t.Errorf("used = %s, want config.yaml", used)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Error("config.yaml not created")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.txt.bak")); err != nil {
		t.Error("config.txt.bak not created")
	}
}

func TestLoadOrMigrate_YAMLWins(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("TELEGRAM_BOT_TOKEN=TXT\nALLOWED_USER_IDS=1\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("telegram:\n  bot_token: YAML\n  allowed_user_ids: [1]\n"), 0o600)
	cfg, _, err := LoadOrMigrate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramBotToken != "YAML" {
		t.Errorf("expected yaml to win, got %q", cfg.TelegramBotToken)
	}
}

func TestLoadOrMigrate_NeitherExists(t *testing.T) {
	if _, _, err := LoadOrMigrate(t.TempDir()); err == nil {
		t.Fatal("expected error when no config exists")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./... -run TestLoadOrMigrate -v`
Expected: FAIL (LoadOrMigrate undefined)

- [ ] **Step 3: config.go에 YAML 경로 + 마이그레이션 구현**

`config.go` 상단 import에 `"os"`, `"path/filepath"` 이미 있음. `LoadConfig` 함수 바로 위/아래에 추가:
```go
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
		_ = os.Rename(txtPath, txtPath+".bak")
		log.Printf("[config] config.txt → config.yaml 마이그레이션 완료 (txt는 .bak로 보존)")
		return cfg, yamlPath, nil
	}
	return nil, yamlPath, fmt.Errorf("설정 파일이 없습니다: %s 또는 %s", yamlPath, txtPath)
}
```
(config.go에 `"log"` import 추가 필요 — 없으면 추가.)

- [ ] **Step 4: 마이그레이션 테스트 통과 확인**

Run: `go test ./... -run TestLoadOrMigrate -v`
Expected: PASS

- [ ] **Step 5: setup.go writeConfigFile를 YAML로 출력**

`setup.go`의 `writeConfigFile` 본문 교체 (시그니처 유지: `func writeConfigFile(path, token string, userID int64, claudeToken string) error`):
```go
func writeConfigFile(path, token string, userID int64, claudeToken string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cfg := &Config{
		TelegramBotToken: token,
		AllowedUserIDs:   []int64{userID},
		ManagerModel:     "haiku",
		TimeoutMinutes:   10,
		ManagerAlways:    true,
		MaxWorkers:       3,
		RateLimitPerMin:  20,
		ClaudeOauthToken: claudeToken,
		DefaultBackend:   "claude",
	}
	out, err := marshalConfigYAML(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
```

- [ ] **Step 6: 마법사가 쓰는 경로를 .yaml로 바꾸기**

`main.go`의 setup 분기와 `run()`에서 `defaultConfigPath()`(→config.txt) 대신 `defaultYAMLPath()` 사용하도록 변경. setup_test.go의 `TestWriteConfigFile_*`는 `LoadConfig(path)`로 검증하므로, **YAML 경로 파일을 LoadConfig가 읽도록** Step 7에서 LoadConfig를 확장한다.

- [ ] **Step 7: LoadConfig가 .yaml 확장자면 YAML 파서 사용**

`config.go`의 `LoadConfig` 시작부에 분기 추가:
```go
func LoadConfig(path string) (*Config, error) {
	if strings.HasSuffix(strings.ToLower(path), ".yaml") || strings.HasSuffix(strings.ToLower(path), ".yml") {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("설정 파일 열기 실패 (%s): %w", path, err)
		}
		return unmarshalConfigYAML(b)
	}
	// ... 기존 key=value 파싱 그대로 ...
```

- [ ] **Step 8: 기존 setup 테스트 갱신 (YAML 경로 사용)**

`setup_test.go`의 두 테스트에서 파일명을 `config.yaml`로:
```go
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
```
(`TestWriteConfigFile_RoundTrip`, `TestWriteConfigFile_WithOauthToken` 둘 다.) `iter27_test.go`의 `TestWriteConfigFile_CreatesFile`는 출력이 YAML이므로 검증을 `LoadConfig`로 교체:
```go
func TestWriteConfigFile_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeConfigFile(path, "testtoken:123", 42, ""); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramBotToken != "testtoken:123" || cfg.AllowedUserIDs[0] != 42 {
		t.Errorf("cfg = %+v", cfg)
	}
}
```

- [ ] **Step 9: 전체 테스트 + vet 통과 확인**

Run: `go test ./... 2>&1 | tail -3 && go vet ./...`
Expected: PASS, clean

- [ ] **Step 10: 커밋**

```bash
gofmt -w config.go setup.go main.go setup_test.go iter27_test.go config_migrate_test.go
git add -A
git commit -m "feat(config): YAML-first LoadOrMigrate (auto-migrate config.txt) + wizard writes YAML

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: ConfigHolder + 컴포넌트 스레딩 + RateLimiter.SetLimit

**Files:**
- Create: `confighold.go`
- Test: `confighold_test.go`
- Modify: `security.go` (`SetLimit`), `bot.go`, `manager.go`, `runner.go`, `main.go`
- Modify: `manager_test.go`, `bot_parallel_test.go`, 기타 `NewManager`/`NewBot`/`NewClaudeRunner` 호출 테스트

**Interfaces:**
- Produces: `type ConfigHolder struct{...}`; `func NewConfigHolder(*Config) *ConfigHolder`; `func (h *ConfigHolder) Get() *Config`; `func (h *ConfigHolder) Set(*Config)`; `func (r *RateLimiter) SetLimit(int)`
- Consumes: `Config`(types.go)
- Produces (signature changes):
  - `NewBot(api *tgbotapi.BotAPI, cfgh *ConfigHolder, store StoreRepo, manager *Manager, scheduler *Scheduler, userStore *UserStore) *Bot`
  - `NewManager(claude ClaudeClient, codex ClaudeClient, store StoreRepo, cfgh *ConfigHolder) *Manager`
  - `NewClaudeRunner(claudePath string, cfgh *ConfigHolder) *claudeRunner`
  - methods `func (b *Bot) cfg() *Config`, `func (m *Manager) cfg() *Config`, `func (r *claudeRunner) cfg() *Config`

- [ ] **Step 1: 실패 테스트 작성 (holder + SetLimit)**

Create `confighold_test.go`:
```go
package main

import (
	"sync"
	"testing"
)

func TestConfigHolder_GetSet(t *testing.T) {
	h := NewConfigHolder(&Config{WorkerModel: "a"})
	if h.Get().WorkerModel != "a" {
		t.Fatal("initial get")
	}
	h.Set(&Config{WorkerModel: "b"})
	if h.Get().WorkerModel != "b" {
		t.Fatal("after set")
	}
}

func TestConfigHolder_ConcurrentRace(t *testing.T) {
	h := NewConfigHolder(&Config{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = h.Get() }()
		go func() { defer wg.Done(); h.Set(&Config{MaxWorkers: 1}) }()
	}
	wg.Wait()
}

func TestRateLimiter_SetLimit(t *testing.T) {
	r := NewRateLimiter(1)
	if !r.Allow(7) {
		t.Fatal("first allowed")
	}
	if r.Allow(7) {
		t.Fatal("second should be blocked at limit 1")
	}
	r.SetLimit(0) // unlimited
	if !r.Allow(7) {
		t.Fatal("after SetLimit(0) should be unlimited")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./... -run 'TestConfigHolder|TestRateLimiter_SetLimit' -v`
Expected: FAIL (undefined)

- [ ] **Step 3: confighold.go 구현**

```go
package main

import "sync/atomic"

// ConfigHolder holds the live *Config and allows lock-free reads with atomic swap.
type ConfigHolder struct {
	p atomic.Pointer[Config]
}

func NewConfigHolder(c *Config) *ConfigHolder {
	h := &ConfigHolder{}
	h.p.Store(c)
	return h
}

func (h *ConfigHolder) Get() *Config { return h.p.Load() }
func (h *ConfigHolder) Set(c *Config) { h.p.Store(c) }
```

- [ ] **Step 4: security.go에 SetLimit 추가**

`RateLimiter`에 추가 (Allow의 maxPerMin 읽기를 락 안에서 하도록 함께 보정):
```go
// SetLimit updates the per-minute cap live (0 = unlimited).
func (r *RateLimiter) SetLimit(maxPerMin int) {
	r.mu.Lock()
	r.maxPerMin = maxPerMin
	r.mu.Unlock()
}
```
그리고 `Allow`의 첫 줄 `if r.maxPerMin <= 0 {` 을 락 이후로 이동:
```go
func (r *RateLimiter) Allow(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxPerMin <= 0 {
		return true
	}
	now := time.Now()
	// ... 기존 로직 그대로 ...
```

- [ ] **Step 5: 컴포넌트 시그니처/필드 변경 + cfg() 접근자**

`bot.go`:
- 구조체 필드 `cfg *Config` → `cfgh *ConfigHolder`
- `NewBot(... cfg *Config ...)` → `NewBot(... cfgh *ConfigHolder ...)`; 본문 `cfg:` 대입 → `cfgh: cfgh`, `rateLimiter: NewRateLimiter(cfg.RateLimitPerMin)` → `NewRateLimiter(cfgh.Get().RateLimitPerMin)`
- 접근자 추가: `func (b *Bot) cfg() *Config { return b.cfgh.Get() }`
- 본문 전역 치환: `b.cfg.` → `b.cfg().`

`manager.go`:
- 필드 `cfg *Config` → `cfgh *ConfigHolder`; `NewManager(..., cfg *Config)` → `..., cfgh *ConfigHolder`; 본문 `cfg: cfg` → `cfgh: cfgh`
- 접근자: `func (m *Manager) cfg() *Config { return m.cfgh.Get() }`
- 치환: `m.cfg.` → `m.cfg().`

`runner.go`:
- 필드 `cfg *Config` → `cfgh *ConfigHolder`; `NewClaudeRunner(claudePath string, cfg *Config)` → `..., cfgh *ConfigHolder`; 본문 `cfg: cfg` → `cfgh: cfgh`
- 접근자: `func (r *claudeRunner) cfg() *Config { return r.cfgh.Get() }`
- 치환: `r.cfg.` → `r.cfg().`; 단 nil 체크 한 곳:
  ```go
  if c := r.cfg(); c != nil && c.ClaudeOauthToken != "" {
      cmd.Env = append(os.Environ(), "CLAUDE_CODE_OAUTH_TOKEN="+c.ClaudeOauthToken)
  }
  ```

> 치환은 기계적: `grep -n "\.cfg\." *.go`로 확인 후 각 파일에서 `.cfg.`→`.cfg().`. (validateScript(b.cfg, ...) 호출은 `validateScript(b.cfg(), ...)`로.)

- [ ] **Step 6: main.go에서 holder 생성 + 컴포넌트에 전달**

`run()`에서 `cfg, err := LoadConfig(...)` 부분을 `cfg, cfgPath, err := LoadOrMigrate(dir)` 흐름으로 바꾸고(또는 yaml 경로 LoadConfig), holder 생성:
```go
	holder := NewConfigHolder(cfg)
```
이후 생성자 호출을 holder로:
```go
	runner := NewClaudeRunner(claudePath, holder)
	manager := NewManager(runner, codexClient, store, holder)
	bot := NewBot(api, holder, store, manager, scheduler, userStore)
```
(claudePath는 최초 cfg.ClaudePath로 1회 결정 — 유지.)

- [ ] **Step 7: 테스트 호출부 갱신**

`manager_test.go` `mgrFixture`: `NewManager(fc, nil, st, cfg)` → `NewManager(fc, nil, st, NewConfigHolder(cfg))`. `TestManager_NoProjects_Guides`도 동일.
`bot_parallel_test.go`: `b.cfg.MaxWorkers` → `b.cfg().MaxWorkers` (2곳), Bot 생성 시 `NewConfigHolder(cfg)` 전달.
기타 `NewBot`/`NewClaudeRunner`/`NewManager` 호출 테스트 동일 패턴으로 수정 (`grep -rn "NewBot(\|NewManager(\|NewClaudeRunner(" *_test.go`).

- [ ] **Step 8: 빌드/테스트/레이스 통과 확인**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -3 && go test -race -run 'TestConfigHolder' ./...`
Expected: 모두 PASS

- [ ] **Step 9: 커밋**

```bash
gofmt -w .
git add -A
git commit -m "refactor(config): ConfigHolder(atomic) threaded through bot/manager/runner + RateLimiter.SetLimit

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: fsnotify Hot-Reload Watcher + 적용 콜백

**Files:**
- Create: `confighot.go`
- Test: `confighot_test.go`
- Modify: `main.go` (watcher 기동), `go.mod` (fsnotify)

**Interfaces:**
- Consumes: `ConfigHolder`(Task 3), `LoadConfig`(yaml), `RateLimiter.SetLimit`(Task 3)
- Produces:
  - `type ReloadHooks struct { OnRateLimit func(int); OnTokenChanged func(); OnScreenControl func(bool); Notify func(string) }`
  - `func applyReload(old, new *Config, hooks ReloadHooks)` — 변경 비교 후 훅 호출 (테스트 가능, fsnotify 불필요)
  - `func WatchConfig(path string, holder *ConfigHolder, hooks ReloadHooks) (stop func(), err error)` — fsnotify 디렉토리 감시

- [ ] **Step 1: go.mod에 fsnotify 추가**

Run: `go get github.com/fsnotify/fsnotify@latest`
Expected: go.mod에 fsnotify 추가

- [ ] **Step 2: 실패 테스트 작성 (applyReload 정책)**

Create `confighot_test.go`:
```go
package main

import "testing"

func TestApplyReload_RateLimitChanged(t *testing.T) {
	old := &Config{RateLimitPerMin: 20, TelegramBotToken: "t"}
	nw := &Config{RateLimitPerMin: 5, TelegramBotToken: "t"}
	var gotLimit = -999
	applyReload(old, nw, ReloadHooks{OnRateLimit: func(n int) { gotLimit = n }})
	if gotLimit != 5 {
		t.Errorf("OnRateLimit got %d, want 5", gotLimit)
	}
}

func TestApplyReload_TokenChanged(t *testing.T) {
	old := &Config{TelegramBotToken: "A"}
	nw := &Config{TelegramBotToken: "B"}
	called := false
	applyReload(old, nw, ReloadHooks{OnTokenChanged: func() { called = true }})
	if !called {
		t.Error("OnTokenChanged should fire on token change")
	}
}

func TestApplyReload_ScreenControlToggle(t *testing.T) {
	old := &Config{ScreenControl: false}
	nw := &Config{ScreenControl: true}
	var got *bool
	applyReload(old, nw, ReloadHooks{OnScreenControl: func(b bool) { got = &b }})
	if got == nil || *got != true {
		t.Error("OnScreenControl should fire true")
	}
}

func TestApplyReload_NoChange_NoHooks(t *testing.T) {
	c := &Config{TelegramBotToken: "t", RateLimitPerMin: 20}
	applyReload(c, &Config{TelegramBotToken: "t", RateLimitPerMin: 20}, ReloadHooks{
		OnRateLimit:    func(int) { t.Error("rate hook should not fire") },
		OnTokenChanged: func() { t.Error("token hook should not fire") },
	})
}
```

- [ ] **Step 3: 실패 확인**

Run: `go test ./... -run TestApplyReload -v`
Expected: FAIL (applyReload/ReloadHooks undefined)

- [ ] **Step 4: confighot.go 구현 (applyReload + WatchConfig)**

```go
package main

import (
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ReloadHooks are invoked by applyReload when specific fields change.
type ReloadHooks struct {
	OnRateLimit     func(int)  // new rate limit
	OnTokenChanged  func()     // bot token changed (needs restart)
	OnScreenControl func(bool) // screen_control.enabled toggled
	Notify          func(string)
}

// applyReload compares old vs new config and fires the relevant hooks.
func applyReload(old, nw *Config, h ReloadHooks) {
	if old.RateLimitPerMin != nw.RateLimitPerMin && h.OnRateLimit != nil {
		h.OnRateLimit(nw.RateLimitPerMin)
	}
	if old.TelegramBotToken != nw.TelegramBotToken && h.OnTokenChanged != nil {
		h.OnTokenChanged()
	}
	if old.ScreenControl != nw.ScreenControl && h.OnScreenControl != nil {
		h.OnScreenControl(nw.ScreenControl)
	}
}

// WatchConfig watches the config file's directory and hot-reloads on change.
// Returns a stop func. Editor atomic-saves (temp+rename) are handled by watching
// the directory and filtering for the config file name; events are debounced.
func WatchConfig(path string, holder *ConfigHolder, hooks ReloadHooks) (func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	done := make(chan struct{})
	go func() {
		var timer *time.Timer
		reload := func() {
			cfg, err := LoadConfig(path)
			if err != nil {
				log.Printf("[config] reload 실패: %v (이전 설정 유지)", err)
				if hooks.Notify != nil {
					hooks.Notify("⚠️ 설정 reload 실패: " + err.Error() + " — 이전 설정 유지")
				}
				return
			}
			old := holder.Get()
			holder.Set(cfg)
			applyReload(old, cfg, hooks)
			log.Printf("[config] reload 적용됨")
			if hooks.Notify != nil {
				hooks.Notify("⚙️ 설정이 reload되었습니다")
			}
		}
		for {
			select {
			case <-done:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != name {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(300*time.Millisecond, reload)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[config] watcher 오류: %v", err)
			}
		}
	}()

	return func() { close(done); _ = w.Close() }, nil
}
```

- [ ] **Step 5: applyReload 테스트 통과 확인**

Run: `go test ./... -run TestApplyReload -v`
Expected: PASS

- [ ] **Step 6: main.go에서 watcher 기동**

`run()`에서 holder 생성 직후(봇 시작 전) 추가. `bot`/`rateLimiter`는 이미 생성돼 있어야 하므로 bot 생성 이후에 둔다:
```go
	hooks := ReloadHooks{
		OnRateLimit:    func(n int) { bot.rateLimiter.SetLimit(n) },
		OnTokenChanged: func() { log.Printf("[config] 봇 토큰 변경 감지 — 적용하려면 재시작 필요") },
		OnScreenControl: func(on bool) {
			log.Printf("[config] screen_control=%v (M1에서 처리)", on) // M1 훅 연결 지점
		},
		Notify: func(msg string) {
			for _, id := range holder.Get().AllowedUserIDs {
				_ = bot.Send(id, msg)
			}
		},
	}
	if stop, werr := WatchConfig(cfgPath, holder, hooks); werr != nil {
		log.Printf("[config] hot-reload 비활성(워처 시작 실패): %v", werr)
	} else {
		defer stop()
	}
```
(`cfgPath`는 yaml 경로여야 함 — Task 2의 LoadOrMigrate가 반환한 경로 사용.)

- [ ] **Step 7: 빌드/테스트/레이스 통과 확인**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -3 && go test -race -run 'TestConfigHolder|TestApplyReload' ./...`
Expected: 모두 PASS

- [ ] **Step 8: 수동 통합 확인(선택, Windows/NanoPi)**

config.yaml의 `runtime.rate_limit_per_min` 변경 → 로그 "[config] reload 적용됨" + 텔레그램 "⚙️ 설정이 reload되었습니다" 확인. `telegram.bot_token` 변경 → "재시작 필요" 로그 확인.

- [ ] **Step 9: 커밋**

```bash
gofmt -w confighot.go confighot_test.go main.go
git add -A
git commit -m "feat(config): fsnotify hot-reload (dir watch + debounce) with apply hooks

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review 결과

- **Spec 커버리지**: YAML 스키마(Task1) · 마이그레이션(Task2) · hot-reload/fsnotify(Task4) · atomic holder/동시성(Task3) · screen_control 섹션 예약(Task1 필드 + Task4 훅) · setup YAML 출력(Task2) — 모두 태스크로 매핑됨.
- **재시작 필요 항목**(bot_token): Task4 OnTokenChanged 훅으로 안내 처리.
- **타입 일관성**: `ConfigHolder.Get/Set`, `cfg()` 접근자, `NewBot/NewManager/NewClaudeRunner(... *ConfigHolder)`, `RateLimiter.SetLimit`, `ReloadHooks`/`applyReload`/`WatchConfig` 시그니처 태스크 간 일치 확인.
- **하위호환**: config.txt 파서·테스트 유지(마이그레이션 경로). 기존 setup 테스트는 .yaml 경로로 갱신.

## 비고 (다음 단계)
- **M1**: `OnScreenControl` 훅에 화면제어 MCP 자동 기동/중지 연결 + 화면 도구(Win32) + 프리셋.
- **M2**: 자율 메뉴 스윕 + 패킷 캡처.
