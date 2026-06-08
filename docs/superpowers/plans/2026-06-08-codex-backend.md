# Codex 백엔드 통합 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `!backend codex` / `!backend claude` 명령어로 런타임에 전체 AI 백엔드를 전환한다.

**Architecture:** `ClaudeClient` 인터페이스를 `codexRunner`가 완전 구현. `Manager.client`를 런타임에 교체하는 방식. Codex JSONL의 `thread_id`를 세션 ID로 사용하고, `codex exec resume <thread_id>`로 세션을 재개한다.

**Tech Stack:** Go 1.21+, `codex exec` CLI (v0.131.0+), `codex exec resume` for session continuity

---

## 파일 맵

| 파일 | 역할 |
|---|---|
| `runner_codex.go` | **신규** — `codexRunner` struct, `Route()`, `Run()`, `exec()`, JSONL 파싱 |
| `runner_codex_test.go` | **신규** — JSONL session ID 추출, route 파싱 단위 테스트 |
| `types.go` | `Conversation.Backend string` 필드 추가 |
| `config.go` | `Config.CodexPath`, `Config.CodexModel`, `findCodex()` 추가 |
| `manager.go` | `sync.RWMutex`, `SetBackend()`, `Backend()`, 불일치 감지, `NewManager` 시그니처 변경 |
| `bot.go` | `!backend` 핸들러, `!help` 업데이트 |
| `main.go` | `findCodex()` 호출, `codexRunner` 생성, `NewManager` 호출 변경 |
| `manager_test.go` | `NewManager` 호출 시그니처 수정 |
| `integration_test.go` | `NewManager` 호출 시그니처 수정 |

---

## Task 1: `Conversation.Backend` 필드 + Config 필드

**Files:**
- Modify: `types.go`
- Modify: `config.go`

- [ ] **Step 1: `types.go`에 `Backend` 필드 추가**

[types.go](types.go) 의 `Conversation` 구조체:

```go
type Conversation struct {
	ID             string             `json:"id"`
	Title          string             `json:"title"`
	Summary        string             `json:"summary"`
	SessionID      string             `json:"sessionId"`
	Started        bool               `json:"started"`
	LastActivity   time.Time          `json:"lastActivity"`
	History        []ConversationTurn `json:"history"`
	ParentID       string             `json:"parentId,omitempty"`
	ChildID        string             `json:"childId,omitempty"`
	IsContinuation bool               `json:"isContinuation,omitempty"`
	Backend        string             `json:"backend,omitempty"` // "claude"|"codex"|"" (""=claude)
}
```

- [ ] **Step 2: `config.go`에 `CodexPath`, `CodexModel` 필드 추가**

[config.go](config.go) 의 `Config` 구조체:

```go
type Config struct {
	TelegramBotToken string
	AllowedUserIDs   []int64
	ManagerModel     string
	WorkerModel      string
	ClaudePath       string
	TimeoutMinutes   int
	ManagerAlways    bool
	CodexPath        string // "" = auto-detect
	CodexModel       string // "" = "o4-mini"
}
```

- [ ] **Step 3: `LoadConfig()`에 파싱 추가**

[config.go](config.go) 의 `LoadConfig()` 내 `switch key` 블록에 추가:

```go
case "codex_path":
    cfg.CodexPath = val
case "codex_model":
    cfg.CodexModel = val
```

- [ ] **Step 4: `findCodex()` 함수 추가**

[config.go](config.go) 에서 `findClaude()` 바로 아래에 추가:

```go
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
```

- [ ] **Step 5: 빌드 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go build ./...
```

Expected: 오류 없음

- [ ] **Step 6: 커밋**

```
git add types.go config.go
git commit -m "feat(codex): Conversation.Backend 필드 + Config CodexPath/CodexModel + findCodex()"
```

---

## Task 2: `runner_codex_test.go` — 파싱 단위 테스트 (TDD)

**Files:**
- Create: `runner_codex_test.go`

JSONL 파싱과 라우트 결과 파싱 함수를 테스트 먼저 작성한다.

- [ ] **Step 1: 테스트 파일 생성**

```go
package main

import (
	"encoding/json"
	"testing"
)

// TestExtractThreadID: JSONL 스트림에서 thread_id 추출
func TestExtractThreadID(t *testing.T) {
	jsonl := `{"type":"thread.started","thread_id":"abc-123"}
{"type":"turn.started"}
{"type":"agent_reasoning","content":"thinking"}
{"type":"agent_message","content":"hello"}`

	got := extractThreadID(jsonl)
	if got != "abc-123" {
		t.Errorf("extractThreadID = %q, want %q", got, "abc-123")
	}
}

func TestExtractThreadID_Missing(t *testing.T) {
	// 이벤트에 thread_id 없을 때 빈 문자열 반환
	jsonl := `{"type":"turn.started"}
{"type":"agent_message","content":"hello"}`

	got := extractThreadID(jsonl)
	if got != "" {
		t.Errorf("extractThreadID = %q, want empty", got)
	}
}

func TestParseCodexOutput_Plain(t *testing.T) {
	// -o 파일 내용이 plain text인 경우
	content := "  hello world  \n"
	got := parseCodexOutput(content)
	if got != "hello world" {
		t.Errorf("parseCodexOutput = %q, want %q", got, "hello world")
	}
}

func TestParseCodexRouteDecision(t *testing.T) {
	// --output-schema 결과: JSON 객체
	raw := `{"action":"new","project":"myapp","newTitle":"새 기능"}`
	got, err := parseCodexRouteDecision(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "new" || got.Project != "myapp" || got.NewTitle != "새 기능" {
		t.Errorf("unexpected decision: %+v", got)
	}
}

func TestCodexModel(t *testing.T) {
	// CodexModel 설정 없을 때 기본값 반환
	cfg := &Config{}
	if codexDefaultModel(cfg) != "o4-mini" {
		t.Error("expected o4-mini default")
	}
	cfg.CodexModel = "o3"
	if codexDefaultModel(cfg) != "o3" {
		t.Error("expected o3")
	}
}

// helper: JSON marshal/unmarshal round-trip for RouteDecision
func TestRouteDecisionJSON(t *testing.T) {
	dec := RouteDecision{Action: "resume", Project: "p1", ConversationID: "c1"}
	b, _ := json.Marshal(dec)
	var got RouteDecision
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Action != dec.Action || got.Project != dec.Project {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: 테스트 실행 — 실패 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go test -run "TestExtractThreadID|TestParseCodex|TestCodexModel" -v ./...
```

Expected: FAIL — `extractThreadID`, `parseCodexOutput`, `parseCodexRouteDecision`, `codexDefaultModel` undefined

- [ ] **Step 3: 커밋 (failing tests)**

```
git add runner_codex_test.go
git commit -m "test(codex): JSONL 파싱 + 라우트 파싱 단위 테스트 추가 (RED)"
```

---

## Task 3: `runner_codex.go` — 파싱 헬퍼 구현

**Files:**
- Create: `runner_codex.go`

- [ ] **Step 1: `runner_codex.go` 생성 — struct + 헬퍼 함수**

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// codexRunner implements ClaudeClient backed by the local codex CLI.
// Codex JSONL sessions are identified by thread_id from the thread.started event.
type codexRunner struct {
	codexPath string
	cfg       *Config
}

// NewCodexRunner builds a ClaudeClient backed by the local codex CLI.
func NewCodexRunner(codexPath string, cfg *Config) *codexRunner {
	return &codexRunner{codexPath: codexPath, cfg: cfg}
}

// codexDefaultModel returns the configured model or the default "o4-mini".
func codexDefaultModel(cfg *Config) string {
	if cfg.CodexModel != "" {
		return cfg.CodexModel
	}
	return "o4-mini"
}

// extractThreadID scans JSONL lines for the thread_id from a thread.started event.
// Returns "" if not found.
func extractThreadID(jsonl string) string {
	for _, line := range strings.Split(jsonl, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev["type"] == "thread.started" {
			if tid, ok := ev["thread_id"].(string); ok && tid != "" {
				return tid
			}
		}
	}
	return ""
}

// parseCodexOutput trims whitespace from the -o file content.
func parseCodexOutput(content string) string {
	return strings.TrimSpace(content)
}

// parseCodexRouteDecision parses a RouteDecision from the codex output string.
// Tries direct unmarshal first, then firstJSONObject extraction.
func parseCodexRouteDecision(s string) (RouteDecision, error) {
	if dec, ok := unmarshalDecision(s); ok {
		return dec, nil
	}
	return RouteDecision{}, fmt.Errorf("codex 라우팅 JSON 파싱 실패: %q", s)
}
```

- [ ] **Step 2: 테스트 실행 — 통과 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go test -run "TestExtractThreadID|TestParseCodex|TestCodexModel" -v ./...
```

Expected: PASS (5 tests)

- [ ] **Step 3: 커밋**

```
git add runner_codex.go
git commit -m "feat(codex): codexRunner 파싱 헬퍼 구현 (GREEN)"
```

---

## Task 4: `runner_codex.go` — `exec()`, `Route()`, `Run()` 구현

**Files:**
- Modify: `runner_codex.go`

- [ ] **Step 1: `exec()` 헬퍼 추가**

`runner_codex.go` 에 추가:

```go
// exec runs codex with process-tree cancellation (Windows-aware).
func (r *codexRunner) exec(ctx context.Context, dir string, args []string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, r.codexPath, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}
```

- [ ] **Step 2: `Route()` 구현 추가**

```go
// Route asks Codex to classify the user message and return a routing decision.
// Uses --output-schema for structured JSON output and --ephemeral (no session needed).
func (r *codexRunner) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	// Write route schema to a temp file (codex requires a file path, not inline JSON).
	schemaFile := filepath.Join(os.TempDir(), fmt.Sprintf("teleclaude_route_schema_%d.json", os.Getpid()))
	if err := os.WriteFile(schemaFile, []byte(routeJSONSchema), 0600); err != nil {
		return RouteDecision{}, fmt.Errorf("codex route schema 임시 파일 생성 실패: %w", err)
	}
	defer os.Remove(schemaFile)

	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("teleclaude_route_out_%d.txt", os.Getpid()))
	defer os.Remove(outFile)

	prompt := buildRoutePrompt(req)
	args := []string{
		"exec",
		"--ignore-user-config",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"--ephemeral",
		"--output-schema", schemaFile,
		"--json",
		"-o", outFile,
		"-m", codexDefaultModel(r.cfg),
		prompt,
	}

	home, _ := os.UserHomeDir()
	_, stderr, err := r.exec(ctx, home, args)
	if err != nil {
		return RouteDecision{}, fmt.Errorf("codex manager 호출 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}

	content, rerr := os.ReadFile(outFile)
	if rerr != nil {
		return RouteDecision{}, fmt.Errorf("codex route 결과 파일 읽기 실패: %w", rerr)
	}
	return parseCodexRouteDecision(string(content))
}
```

- [ ] **Step 3: `Run()` 구현 추가**

```go
// Run executes a worker turn via codex exec (new) or codex exec resume (existing thread).
func (r *codexRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("teleclaude_codex_%d_%s.txt", os.Getpid(), req.SessionID))
	defer os.Remove(outFile)

	model := req.Model
	if model == "" {
		model = codexDefaultModel(r.cfg)
	}

	var args []string
	if req.Resume && req.SessionID != "" {
		// Resume existing thread
		args = []string{
			"exec", "resume", req.SessionID,
			"--dangerously-bypass-approvals-and-sandbox",
			"--ignore-user-config",
			"--skip-git-repo-check",
			"--json",
			"-o", outFile,
			"-m", model,
			req.Prompt,
		}
	} else {
		// New thread
		args = []string{
			"exec",
			"-C", req.WorkDir,
			"--dangerously-bypass-approvals-and-sandbox",
			"--ignore-user-config",
			"--skip-git-repo-check",
			"--json",
			"-o", outFile,
			"-m", model,
			req.Prompt,
		}
	}

	stdout, stderr, err := r.exec(ctx, req.WorkDir, args)
	if err != nil {
		if ctx.Err() != nil {
			return RunResult{}, ctx.Err()
		}
		// Read output file even on non-zero exit — codex may still produce output.
		if content, rerr := os.ReadFile(outFile); rerr == nil && len(content) > 0 {
			return RunResult{Text: parseCodexOutput(string(content))}, nil
		}
		return RunResult{}, fmt.Errorf("codex worker 실행 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}

	// Extract thread_id for new sessions so the store can persist it.
	threadID := extractThreadID(stdout)

	content, rerr := os.ReadFile(outFile)
	if rerr != nil {
		return RunResult{}, fmt.Errorf("codex 결과 파일 읽기 실패: %w", rerr)
	}

	text := parseCodexOutput(string(content))
	result := RunResult{Text: text}
	// Smuggle thread_id back via a well-known prefix so manager can store it.
	// Manager strips this prefix before showing the response to the user.
	if !req.Resume && threadID != "" {
		result.SessionID = threadID
	}
	return result, nil
}
```

**Note:** `RunResult`에 `SessionID string` 필드를 추가해야 합니다 (다음 단계).

- [ ] **Step 4: `RunResult`에 `SessionID` 필드 추가**

[types.go](types.go):

```go
type RunResult struct {
	Text      string
	IsError   bool
	SessionID string // non-empty only on first codex turn (thread_id from JSONL)
}
```

- [ ] **Step 5: 빌드 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go build ./...
```

Expected: 오류 없음

- [ ] **Step 6: 커밋**

```
git add runner_codex.go types.go
git commit -m "feat(codex): Route() + Run() + exec() 구현, RunResult.SessionID 추가"
```

---

## Task 5: `manager.go` — `SetBackend()`, mutex, 불일치 감지

**Files:**
- Modify: `manager.go`

- [ ] **Step 1: `Manager` 구조체 확장**

[manager.go](manager.go) 의 `Manager` 구조체를 다음으로 교체:

```go
type Manager struct {
	client       ClaudeClient
	backendName  string       // "claude" | "codex"
	backendMu    sync.RWMutex
	claudeClient ClaudeClient // always preserved for switching back
	codexClient  ClaudeClient // nil if codex not available

	store        StoreRepo
	workerStatus WorkerStatusStore
	scheduler    *Scheduler
	cfg          *Config
}
```

`manager.go` import에 `"sync"` 추가.

- [ ] **Step 2: `NewManager` 시그니처 변경**

```go
func NewManager(claude ClaudeClient, codex ClaudeClient, store StoreRepo, cfg *Config) *Manager {
	return &Manager{
		client:       claude,
		backendName:  "claude",
		claudeClient: claude,
		codexClient:  codex,
		store:        store,
		workerStatus: NewMemoryWorkerStatusStore(),
		cfg:          cfg,
	}
}
```

- [ ] **Step 3: `SetBackend()` + `Backend()` 추가**

```go
// SetBackend switches the active AI backend. Returns error if the requested backend is unavailable.
func (m *Manager) SetBackend(name string) error {
	m.backendMu.Lock()
	defer m.backendMu.Unlock()
	switch name {
	case "claude":
		m.client = m.claudeClient
		m.backendName = "claude"
	case "codex":
		if m.codexClient == nil {
			return fmt.Errorf("Codex가 설치되어 있지 않습니다")
		}
		m.client = m.codexClient
		m.backendName = "codex"
	default:
		return fmt.Errorf("알 수 없는 백엔드: %s (claude | codex)", name)
	}
	return nil
}

// Backend returns the current backend name.
func (m *Manager) Backend() string {
	m.backendMu.RLock()
	defer m.backendMu.RUnlock()
	return m.backendName
}

// CodexAvailable reports whether codex is registered.
func (m *Manager) CodexAvailable() bool {
	return m.codexClient != nil
}
```

- [ ] **Step 4: `Handle()` 내 `client` 접근에 RLock 추가**

`Handle()` 메서드 상단 (첫 `decide()` 호출 직전)에 다음 패턴을 적용:

```go
func (m *Manager) Handle(ctx context.Context, chatID int64, text string, s MessageSender) {
	m.backendMu.RLock()
	currentBackend := m.backendName
	m.backendMu.RUnlock()
	// ... 나머지 기존 코드 ...
```

- [ ] **Step 5: `runWorker()` 진입 전 백엔드 불일치 감지 추가**

[manager.go](manager.go) 의 `Handle()` 내 `case ActionResume:` 블록에서 `runWorker` 호출 직전:

```go
case ActionResume:
    c, exists := m.store.GetConversation(dec.Project, dec.ConversationID)
    if !exists {
        _ = s.Send(chatID, "⚠️ 대화를 찾을 수 없습니다.")
        return
    }
    // Backend mismatch: force new conversation so we don't resume a
    // Claude session with Codex or vice versa.
    convBackend := c.Backend
    if convBackend == "" {
        convBackend = "claude"
    }
    if convBackend != currentBackend {
        _ = s.Send(chatID, fmt.Sprintf("⚠️ 백엔드 변경으로 새 대화를 시작합니다. [%s]", strings.ToUpper(currentBackend)))
        newConv, err := m.store.NewConversation(dec.Project, "새 대화 ("+currentBackend+")")
        if err != nil {
            _ = s.Send(chatID, "⚠️ 새 대화 생성 실패: "+err.Error())
            return
        }
        newConv.Backend = currentBackend
        _ = m.store.UpdateConversation(dec.Project, newConv)
        _ = m.store.SetActive(dec.Project, newConv.ID)
        m.runWorker(ctx, chatID, text, dec.Project, newConv, s)
        return
    }
    m.runWorker(ctx, chatID, text, dec.Project, c, s)
```

- [ ] **Step 6: `runWorker()` 내 신규 대화 생성 시 `Backend` 기록**

[manager.go](manager.go) 의 `runWorker()` 내 `NewConversation` 호출 후:

```go
newConv, err := m.store.NewConversation(project, dec.NewTitle)
if err != nil { ... }
newConv.Backend = m.Backend()  // ← 추가
_ = m.store.UpdateConversation(project, newConv)
```

그리고 `RunRequest` 생성 이후, 첫 worker 완료 시 `SessionID`를 갱신하는 위치에:

```go
// Codex returns the thread_id in result.SessionID on first turn.
if res.SessionID != "" && !workConv.Started {
    workConv.SessionID = res.SessionID
}
```

- [ ] **Step 7: 빌드 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go build ./...
```

Expected: `manager_test.go` 와 `integration_test.go` 에서 `NewManager` 호출 오류 예상.

- [ ] **Step 8: 테스트 파일의 `NewManager` 호출 수정**

[manager_test.go](manager_test.go) 와 [integration_test.go](integration_test.go) 에서 `NewManager(runner, store, cfg)` → `NewManager(runner, nil, store, cfg)` 로 변경.

- [ ] **Step 9: 빌드 + 기존 테스트 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go build ./... && go test ./... -count=1
```

Expected: 빌드 성공, 기존 테스트 모두 PASS

- [ ] **Step 10: 커밋**

```
git add manager.go manager_test.go integration_test.go
git commit -m "feat(codex): Manager.SetBackend()/Backend() + 백엔드 불일치 감지 + mutex"
```

---

## Task 6: `main.go` — Codex 초기화

**Files:**
- Modify: `main.go`

- [ ] **Step 1: `run()` 함수에 codex 초기화 추가**

[main.go](main.go) 의 `runner := NewClaudeRunner(claudePath, cfg)` 라인을 다음으로 교체:

```go
claudeClient := NewClaudeRunner(claudePath, cfg)

codexPath, _ := findCodex(cfg.CodexPath)
var codexClient ClaudeClient
if codexPath != "" {
    codexClient = NewCodexRunner(codexPath, cfg)
    log.Printf("[main] codex: %s", codexPath)
} else {
    log.Printf("[main] codex: 미설치 (선택적)")
}

manager := NewManager(claudeClient, codexClient, store, cfg)
```

기존 `manager := NewManager(runner, store, cfg)` 라인 제거.

- [ ] **Step 2: 빌드 확인**

```
cd c:\Project\88.MyProject\Teleclaude && go build ./...
```

Expected: 오류 없음

- [ ] **Step 3: 커밋**

```
git add main.go
git commit -m "feat(codex): main.go codex 초기화 + NewManager 시그니처 적용"
```

---

## Task 7: `bot.go` — `!backend` 명령어

**Files:**
- Modify: `bot.go`

- [ ] **Step 1: `dispatchCommand()` 에 `!backend` 케이스 추가**

[bot.go](bot.go) 의 `switch cmd` 블록에 추가 (예: `case "!update":` 앞):

```go
case "!backend":
    b.handleBackend(chatID, fields)
```

- [ ] **Step 2: `handleBackend()` 구현 추가**

```go
// handleBackend handles the !backend command for runtime AI backend switching.
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
```

- [ ] **Step 3: `!help` 출력에 `!backend` 항목 추가**

[bot.go](bot.go) 의 `handleHelp()` 또는 help 텍스트 상수에서:

```
!backend [claude|codex]      AI 백엔드 전환 (현재 상태 확인 또는 전환)
```

- [ ] **Step 4: 빌드 + 전체 테스트**

```
cd c:\Project\88.MyProject\Teleclaude && go build ./... && go test ./... -count=1
```

Expected: 빌드 성공, 모든 테스트 PASS

- [ ] **Step 5: 커밋**

```
git add bot.go
git commit -m "feat(codex): !backend 명령어 — 런타임 AI 백엔드 전환"
```

---

## Task 8: `manager_test.go` — SetBackend 단위 테스트

**Files:**
- Modify: `manager_test.go`

- [ ] **Step 1: `TestSetBackend` 테스트 추가**

[manager_test.go](manager_test.go) 에 추가:

```go
func TestSetBackend_Switch(t *testing.T) {
    store := newTestStore(t)
    claude := &fakeClaude{}
    codex := &fakeClaude{}
    m := NewManager(claude, codex, store, &Config{ManagerAlways: true})

    if m.Backend() != "claude" {
        t.Fatal("default backend should be claude")
    }

    if err := m.SetBackend("codex"); err != nil {
        t.Fatal(err)
    }
    if m.Backend() != "codex" {
        t.Error("expected codex after switch")
    }

    if err := m.SetBackend("claude"); err != nil {
        t.Fatal(err)
    }
    if m.Backend() != "claude" {
        t.Error("expected claude after switch back")
    }
}

func TestSetBackend_CodexUnavailable(t *testing.T) {
    store := newTestStore(t)
    claude := &fakeClaude{}
    m := NewManager(claude, nil, store, &Config{ManagerAlways: true})

    if err := m.SetBackend("codex"); err == nil {
        t.Error("expected error when codex not available")
    }
    if m.Backend() != "claude" {
        t.Error("backend should remain claude after failed switch")
    }
}
```

- [ ] **Step 2: 테스트 실행**

```
cd c:\Project\88.MyProject\Teleclaude && go test -run "TestSetBackend" -v ./...
```

Expected: PASS (2 tests)

- [ ] **Step 3: 커밋**

```
git add manager_test.go
git commit -m "test(codex): SetBackend 단위 테스트"
```

---

## Task 9: 최종 빌드 + exe 생성

- [ ] **Step 1: 전체 테스트**

```
cd c:\Project\88.MyProject\Teleclaude && go test ./... -count=1 -v 2>&1 | tail -20
```

Expected: 모든 테스트 PASS, FAIL 없음

- [ ] **Step 2: 릴리즈 빌드**

```
cd c:\Project\88.MyProject\Teleclaude && go build -o teleclaude.exe .
```

Expected: 오류 없음

- [ ] **Step 3: 최종 커밋**

```
git add teleclaude.exe
git commit -m "build: Codex 백엔드 통합 릴리즈 빌드"
```

---

## 동작 확인 체크리스트

`teleclaude.exe` 실행 후 Telegram에서:

1. `!backend` → `현재 백엔드: CLAUDE`
2. `!backend codex` → `✅ 백엔드 전환됨: CLAUDE → CODEX`
3. 메시지 전송 → `⚠️ 백엔드 변경으로 새 대화를 시작합니다. [CODEX]` + Codex 응답
4. 추가 메시지 → Codex 세션 재개 (thread_id 활용)
5. `!backend claude` → 전환
6. 메시지 → `⚠️ 백엔드 변경으로 새 대화를 시작합니다. [CLAUDE]` + Claude 응답
7. `!backend badname` → `사용법: !backend [claude|codex]`

**Codex 인증 필요 시:** `codex login` 실행 후 테스트

---

## 스펙 커버리지

| 스펙 항목 | 담당 Task |
|---|---|
| `ClaudeClient` 인터페이스 무변경 | Task 3-4 |
| `codexRunner.Route()` | Task 4 |
| `codexRunner.Run()` | Task 4 |
| `Conversation.Backend` 필드 | Task 1 |
| `Config.CodexPath/CodexModel` | Task 1 |
| `findCodex()` 자동탐지 | Task 1 |
| `Manager.SetBackend()/Backend()` | Task 5 |
| 백엔드 불일치 → 새 대화 + 알림 | Task 5 |
| `RunResult.SessionID` (thread_id) | Task 4 |
| `!backend` 명령어 | Task 7 |
| `!backend busy` 보호 | Task 7 |
| `NewManager` 시그니처 변경 | Task 5-6 |
| 테스트 파일 업데이트 | Task 5, 8 |
