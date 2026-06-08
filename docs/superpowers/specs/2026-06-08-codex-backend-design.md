# Codex 백엔드 통합 설계

**날짜**: 2026-06-08  
**상태**: 승인됨

---

## 개요

Teleclaude 봇에 OpenAI Codex CLI(`codex exec`) 백엔드를 추가한다.  
`!backend codex` / `!backend claude` 명령어로 런타임에 전체 백엔드를 전환한다.  
`ClaudeClient` 인터페이스는 변경하지 않는다.

---

## 아키텍처

```
Telegram
  └─ Bot
       ├─ !backend codex/claude ──→ Manager.SetBackend()
       └─ 메시지 ──────────────────→ Manager.Handle()
                                        ├─ client.Route()   ← claudeRunner | codexRunner
                                        └─ client.Run()     ← claudeRunner | codexRunner
```

`Manager.client`를 전체 교체하는 방식.  
`ClaudeClient` 인터페이스(`Route` + `Run`)를 `codexRunner`가 완전히 구현한다.  
Codex는 라우팅도 포함해 전부 Codex로 처리한다.

---

## 신규 파일: `runner_codex.go`

### 타입

```go
type codexRunner struct {
    codexPath string
    cfg       *Config
}

func NewCodexRunner(codexPath string, cfg *Config) *codexRunner
```

### `Route()` 구현

라우팅 JSON 스키마를 임시 파일로 기록 후 `--output-schema`에 전달.

```
codex exec
  --ignore-user-config
  --skip-git-repo-check
  --dangerously-bypass-approvals-and-sandbox
  --output-schema /tmp/teleclaude_route_<pid>.json
  --json
  -o /tmp/teleclaude_route_out_<pid>.txt
  "routing prompt"
```

- 임시 파일은 함수 종료 시 `defer os.Remove()` 로 정리
- `-o` 파일에서 최종 응답 읽기 → `RouteDecision` JSON 파싱
- `--json` JSONL 스트림은 session_id 추출용으로도 활용
- 파싱 실패 시 `unmarshalDecision()` 공유 헬퍼 재사용

### `Run()` 구현

**새 세션** (`req.Resume == false`):
```
codex exec
  -C <workDir>
  --dangerously-bypass-approvals-and-sandbox
  --ignore-user-config
  --skip-git-repo-check
  --json
  -o /tmp/teleclaude_run_out_<pid>_<uuid>.txt
  -m <model>
  "prompt"
```

**세션 재개** (`req.Resume == true`):
```
codex exec resume <SESSION_ID>
  -C <workDir>
  --dangerously-bypass-approvals-and-sandbox
  --ignore-user-config
  --skip-git-repo-check
  --json
  -o /tmp/teleclaude_run_out_<pid>_<uuid>.txt
  -m <model>
  "prompt"
```

#### 세션 ID 추출

`--json` 플래그로 출력되는 JSONL 이벤트를 파싱해 `session_id` 필드를 추출한다.  
이벤트 형식 후보 (구현 시 실제 출력 확인 후 확정):

```json
{"type":"session_configured","session_id":"<uuid>", ...}
{"type":"session_started","id":"<uuid>", ...}
```

파싱 전략:
1. 각 라인을 `map[string]any`로 디코드
2. `session_id` 또는 `id` 키에서 UUID 형식 문자열 추출
3. 신규 세션에서만 수행 (재개 시에는 이미 SessionID 보유)

#### 최종 응답

`-o` 파일 내용을 읽어 `RunResult.Text`로 반환.

### `exec()` 헬퍼

Claude runner와 동일한 구조:
- `exec.CommandContext(ctx, codexPath, args...)`
- `cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }`
- stdout/stderr 분리 캡처

---

## `types.go` 변경

`Conversation` 구조체에 `Backend` 필드 추가:

```go
type Conversation struct {
    // ... 기존 필드 ...
    Backend string `json:"backend,omitempty"` // "claude" | "codex" | "" (기존 = claude)
}
```

빈 값(`""`)은 `"claude"`로 취급 (하위 호환).

---

## `config.go` 변경

```go
type Config struct {
    // ... 기존 필드 ...
    CodexPath  string // "" = auto-detect
    CodexModel string // "" = "o4-mini" (default)
}
```

`findCodex(override string) (string, error)` 추가:
- `override != ""` → 해당 경로 사용
- `where codex` (Windows `exec.LookPath("codex")`) 자동 탐지
- 미설치 시 빈 문자열 반환 (오류 아님 — codex 선택적)

`LoadConfig()` 파싱에 `codex_path`, `codex_model` 키 추가.

---

## `manager.go` 변경

### 필드 추가

```go
type Manager struct {
    client      ClaudeClient
    backendName string        // "claude" | "codex"
    backendMu   sync.RWMutex
    claudeClient ClaudeClient // 항상 보존 (claude로 복귀 시 사용)
    codexClient  ClaudeClient // nil이면 codex 불가
    // ... 기존 필드 ...
}
```

### 신규 메서드

```go
// SetBackend switches the active backend. Returns error if codex not available.
func (m *Manager) SetBackend(name string) error

// Backend returns the current backend name ("claude" | "codex").
func (m *Manager) Backend() string
```

### `Handle()` 내 백엔드 불일치 처리

`runWorker` 진입 전:
- `conversation.Backend != m.Backend()` 이면 → action을 `"new"`로 강제
- 사용자에게 알림: `"⚠️ 백엔드 변경으로 새 대화를 시작합니다. [Codex]"`

`runWorker` 내 새 대화 생성 시 `Conversation.Backend = m.Backend()` 기록.

---

## `bot.go` 변경

### `!backend` 명령어 핸들러 추가

```
!backend            → 현재 백엔드 표시
!backend claude     → Claude로 전환
!backend codex      → Codex로 전환 (미설치 시 오류)
```

```go
case "!backend":
    b.handleBackend(chatID, fields)
```

응답 예시:
```
!backend        → "현재 백엔드: Claude"
!backend codex  → "✅ 백엔드 전환됨: Claude → Codex"
!backend codex  → "⚠️ Codex가 설치되어 있지 않습니다."  (미설치 시)
```

`!help` 출력에 `!backend` 항목 추가.

---

## `main.go` 변경

```go
// Codex는 선택적 — 없어도 봇 정상 동작
codexPath, _ := findCodex(cfg.CodexPath)
var codexClient ClaudeClient
if codexPath != "" {
    codexClient = NewCodexRunner(codexPath, cfg)
    log.Printf("[main] codex: %s", codexPath)
}

claudeClient := NewClaudeRunner(claudePath, cfg)
manager := NewManager(claudeClient, codexClient, store, cfg)
```

`NewManager` 시그니처 변경: `codexClient ClaudeClient` 파라미터 추가 (nil 허용).

---

## 에러 처리

| 상황 | 동작 |
|---|---|
| Codex 미설치 시 `!backend codex` | `⚠️ Codex가 설치되어 있지 않습니다.` |
| Codex `exec` 실패 | `⚠️ Codex 실행 실패: <error>` (Claude와 동일 패턴) |
| `--output-schema` 지원 안 될 경우 | JSONL stdout 직접 파싱으로 폴백 |
| 세션 ID 추출 실패 | 매 턴 새 세션으로 처리 (history를 prompt에 포함) |
| 백엔드 전환 중 진행 중인 작업 있을 때 | `⚠️ 작업 중에는 백엔드를 전환할 수 없습니다.` |

---

## 테스트 시나리오

1. `!backend` — 현재 상태 표시
2. `!backend codex` → 전환 메시지 확인
3. 메시지 전송 → "새 대화 시작" 알림 + Codex 응답
4. 추가 메시지 → Codex 세션 재개 확인
5. `!backend claude` → Claude 복귀
6. 메시지 전송 → "새 대화 시작" 알림 + Claude 응답
7. `!backend codex` (Codex 미설치 환경) → 오류 메시지

---

## 변경 파일 요약

| 파일 | 종류 |
|---|---|
| `runner_codex.go` | 신규 |
| `types.go` | `Conversation.Backend` 필드 추가 |
| `config.go` | `CodexPath`, `CodexModel`, `findCodex()` |
| `manager.go` | `SetBackend()`, `Backend()`, mutex, 불일치 처리 |
| `bot.go` | `!backend` 핸들러, `!help` 업데이트 |
| `main.go` | codex 초기화, `NewManager` 시그니처 |
| `manager_test.go` | `NewManager` 호출 시그니처 수정 |
| `integration_test.go` | `NewManager` 호출 시그니처 수정 |
