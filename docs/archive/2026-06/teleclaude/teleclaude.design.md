---
template: design
version: 1.3
feature: teleclaude
date: 2026-06-02
author: tyranno1223
project: teleclaude
version_project: 0.1.0
---

# teleclaude Design Document

> **Summary**: Telegram 봇 1개 + Manager(Claude 경량)·Worker(Claude `--resume`) 2계층으로, 자연어 메시지를 프로젝트·대화로 라우팅해 로컬 작업을 수행하는 Go 단일 .exe (아키텍처 C: 실용 균형).
>
> **Project**: teleclaude
> **Version**: 0.1.0
> **Author**: tyranno1223
> **Date**: 2026-06-02
> **Status**: Draft
> **Planning Doc**: [teleclaude.plan.md](../01-plan/features/teleclaude.plan.md)

---

## Context Anchor

| Key | Value |
|-----|-------|
| **WHY** | 폰에서 자연어로 "어느 프로젝트의 어느 대화"를 골라 PC의 Claude를 이어 쓰고 싶다 |
| **WHO** | 본인(단독 사용자). allowlist된 Telegram user ID만 |
| **RISK** | ① `--dangerously-skip-permissions` 임의 실행 ② Manager 오라우팅 ③ 메시지당 Claude 2회 호출 비용 |
| **SUCCESS** | 자연어 → Manager 정확 라우팅 → Worker가 해당 디렉토리에서 작업·회신, 대화별 맥락 분리 |
| **SCOPE** | MVP: Manager+Worker+대화저장소+프로젝트레지스트리+/cancel / 후속: 서비스화·토픽 UX·로컬전반제어 |

---

## 1. Overview

### 1.1 Design Goals

- 자연어 메시지를 정확한 (프로젝트, 대화)로 라우팅하고, 애매하면 되물어 오작동을 막는다.
- 대화별 독립 `--resume` 세션으로 맥락을 분리 유지한다.
- 실제 `claude` 호출 없이 라우팅·저장소·출력 로직을 단위 테스트할 수 있게 인터페이스로 격리한다.
- Windows 단일 .exe로 빌드/배포가 단순해야 한다.

### 1.2 Design Principles

- **Manager-Worker 분리**: 라우팅(판단)과 실행(작업)을 분리.
- **인터페이스 경계 최소화(C안)**: `ClaudeClient`, `StoreRepo` 2개만 추상화. 나머지는 구체 타입.
- **방어적 파싱**: Manager JSON·Worker stream-json은 실패해도 폴백.
- **단일 책임 파일**: 파일 1개 = 책임 1개.

---

## 2. Architecture Options

### 2.0 Architecture Comparison

| Criteria | A: Minimal | B: Clean | C: Pragmatic |
|----------|:-:|:-:|:-:|
| Approach | 플랫 단일 패키지 | 다중 패키지+전면 인터페이스 | 단일 main + 핵심 인터페이스 2개 |
| New Files | ~6 | ~14 | ~8 |
| Complexity | Low | High | Medium |
| Maintainability | Medium | High | High |
| Testability | Low | High | High |
| Effort | Low | High | Medium |

**Selected**: **Option C** — **Rationale**: 단일 사용자 Windows 도구로 B는 과함, A는 `claude` 의존으로 테스트 불가. C는 `ClaudeClient`/`StoreRepo` 2개 인터페이스로 핵심 로직을 테스트 가능하게 하면서 단일 .exe 단순성을 유지.

### 2.1 Component Diagram

```
                         teleclaude.exe (package main)
┌───────────────────────────────────────────────────────────────────┐
│  bot.go (Presentation)                                            │
│   - long polling, auth(allowlist), 명령 디스패치, FIFO 큐          │
│        │ text / command                                           │
│        ▼                                                          │
│  manager.go (Application)                                          │
│   - 라우팅 오케스트레이션: ClaudeClient.Route() 호출              │
│   - clarify(되묻기), 제목/요약 갱신                                │
│        │ RouteDecision {project, conv, action}                    │
│        ▼                                                          │
│  store.go (Infra, StoreRepo 구현)  ◀── 대화 저장소 조회/갱신       │
│        │ conv.SessionID, project.Path                             │
│        ▼                                                          │
│  runner.go (Infra, ClaudeClient 구현)                             │
│   - Worker: claude -p --output-format stream-json --resume        │
│   - cwd=project.Path, --dangerously-skip-permissions, cancel      │
│        │ StreamEvent stream                                       │
│        ▼                                                          │
│  relay.go (Presentation)  - 4096자 분할, typing, 라우팅 표기      │
└───────────────────────────────────────────────────────────────────┘
        ▲                                   │
        │ Telegram Bot API (long poll)      │ os/exec → claude CLI(local)
   [폰 Telegram]                       [claude.exe (Node)]
```

> `runner.go`는 `ClaudeClient` 인터페이스를 구현하며 Manager 라우팅(`Route`)과 Worker 실행(`Run`) 둘 다 담당(둘 다 `claude` CLI 호출, 모델/옵션만 다름).

### 2.2 Data Flow

```
Telegram msg
 → bot: auth + enqueue + typing
 → [명령(/project,/chat,/cancel)면 직접 처리]
 → [일반 텍스트면] manager.Route(메시지 + 대화목록 요약)
      → action=clarify → 되묻기 메시지 전송 후 응답 대기
      → action=resume  → store에서 conv.SessionID 조회
      → action=new     → store에 새 conv 생성(SessionID="")
 → runner.Run(cwd=project.Path, prompt=메시지, sessionID=conv.SessionID, resume=conv.Started)
      → claude --output-format json (단일 envelope) → relay가 4096 분할 전송
      → envelope.result 텍스트 확보 (session_id는 우리가 --session-id로 선지정)
 → store 갱신(SessionID, lastActivity), manager가 title/summary 갱신
 → relay: 라우팅 표기("📂 project · 💬 conv") + 최종 결과
```

### 2.3 Dependencies

| Component | Depends On | Purpose |
|-----------|-----------|---------|
| bot | manager, store, relay, Config | 입력 처리·디스패치 |
| manager | ClaudeClient, StoreRepo | 라우팅 판단·요약 |
| runner (ClaudeClient) | os/exec, Config | claude CLI 구동 |
| store (StoreRepo) | encoding/json, os | 대화 저장소 영속 |
| relay | telegram bot api | 출력 전송 |

외부: `go-telegram-bot-api/v5`, 표준 라이브러리, 로컬 `claude` CLI.

---

## 3. Data Model

### 3.1 핵심 타입 (Domain)

```go
// 설정
type Config struct {
    TelegramBotToken string
    AllowedUserIDs   []int64
    ManagerModel     string // 기본 "haiku"
    WorkerModel      string // "" = claude 기본
    ClaudePath       string // "" = 자동탐지
    TimeoutMinutes   int    // 기본 10
}

// 대화(한 주제 = 하나의 claude 세션)
type Conversation struct {
    ID           string    `json:"id"`
    Title        string    `json:"title"`
    Summary      string    `json:"summary"`
    SessionID    string    `json:"sessionId"` // "" = 아직 미시작(첫 턴에 생성·저장)
    LastActivity time.Time `json:"lastActivity"`
}

// 프로젝트(디스크 경로 + 여러 대화)
type Project struct {
    Path          string                   `json:"path"`
    Conversations map[string]*Conversation `json:"conversations"`
}

// 저장소 루트 (별도 저장소: store.json)
type StoreData struct {
    Projects map[string]*Project `json:"projects"`
    Active   ActiveRef           `json:"active"` // 폴백/수동전환용 활성 포인터
}
type ActiveRef struct {
    Project        string `json:"project"`
    ConversationID string `json:"conversationId"`
}
```

### 3.2 Manager 라우팅 입출력

```go
type RouteRequest struct {
    Message  string            // 사용자 자연어 메시지
    Projects []ProjectSummary  // 등록 프로젝트 + 각 대화 {id,title,summary}
    Active   ActiveRef         // 현재 활성 (있으면 연속성 힌트)
}

type RouteDecision struct {
    Project        string  `json:"project"`        // 등록된 프로젝트명 (없으면 "")
    ConversationID string  `json:"conversationId"` // 기존 대화 id (없으면 "")
    Action         string  `json:"action"`         // "resume" | "new" | "clarify"
    NewTitle       string  `json:"newTitle"`       // action=new 시 제목
    Clarify        string  `json:"clarify"`        // action=clarify 시 되물을 문장
    Confidence     float64 `json:"confidence"`     // 0.0~1.0
}
```

### 3.3 Worker 실행 입출력 (json envelope)

> Do-phase 개선(환경 확인): `--session-id <uuid>`로 세션ID를 우리가 선지정 → 출력에서
> session_id를 파싱할 필요가 없고, `--output-format json` 단일 envelope가 stream-json
> 라인 파싱보다 견고하므로 채택. (Worker는 작업 단위 회신; 실시간 토큰 스트리밍은 후속)

```go
type RunRequest struct {
    Prompt    string
    WorkDir   string // project.Path
    SessionID string // 대화별 UUID (store가 생성)
    Resume    bool   // false → --session-id(첫 턴), true → --resume(이후)
    Model     string // WorkerModel
}
type RunResult struct {
    Text    string // envelope.result
    IsError bool   // envelope.is_error
}

// claude --output-format json 단일 결과 envelope (사용 필드만 방어적 파싱)
type claudeEnvelope struct {
    Type      string `json:"type"`
    Subtype   string `json:"subtype"`
    Result    string `json:"result"`
    IsError   bool   `json:"is_error"`
    SessionID string `json:"session_id"`
}
```

### 3.4 store.json 예시

```json
{
  "active": { "project": "myapp", "conversationId": "1" },
  "projects": {
    "myapp": {
      "path": "C:\\Project\\myapp",
      "conversations": {
        "1": { "id":"1","title":"로그인 버그","summary":"세션 만료 처리 수정","sessionId":"abc-123","lastActivity":"2026-06-02T10:00:00Z" }
      }
    }
  }
}
```

---

## 4. 내부 인터페이스 / 외부 호출 계약

### 4.1 인터페이스 (C안 경계 2개)

```go
type ClaudeClient interface {
    Route(ctx context.Context, req RouteRequest) (RouteDecision, error)
    Run(ctx context.Context, req RunRequest) (RunResult, error)
}

type StoreRepo interface {
    Load() error
    Save() error
    ListProjects() map[string]*Project
    AddProject(name, path string) error
    RemoveProject(name string) error
    GetProject(name string) (*Project, bool)
    NewConversation(project, title string) (*Conversation, error)
    GetConversation(project, convID string) (*Conversation, bool)
    UpdateConversation(project string, c *Conversation) error
    SetActive(project, convID string) error
    GetActive() ActiveRef
}
```

### 4.2 claude CLI 호출 계약

**Manager (라우팅, 도구 없음, 프로젝트 무관 cwd):**
```
claude -p "<routing system prompt + 대화목록 + 사용자메시지>" \
       --model <ManagerModel> --output-format json --json-schema <RouteDecision schema>
```
- `--json-schema`로 구조화 출력 강제. ⚠️ **실측(통합검증)**: 검증된 객체는 `.result`가 아니라 envelope의 별도 필드 **`structured_output`** 에 담긴다(`.result`엔 산문이 올 수 있음). 따라서 파싱 우선순위 = `structured_output` → `.result`(펜스/산문 폴백) → stdout 첫 `{...}`.
- 파싱 실패/빈 결과 → 폴백(§6.3).
- 라우팅 프롬프트 규칙: "반드시 등록된 프로젝트명 중에서만 선택. 모르면 action=clarify. 새 주제면 action=new + newTitle. 확실히 기존 대화 연속이면 action=resume + conversationId. JSON만 출력."

**Worker (실행, 도구 허용, cwd=프로젝트):**
```
claude -p "<user message>" --output-format json \
       --dangerously-skip-permissions \
       [--model <WorkerModel>] (--session-id <UUID> | --resume <UUID>)
   (cwd = project.Path)
```
- 세션ID는 대화 생성 시 store가 발급한 **UUID**. 첫 턴은 `--session-id <UUID>`, 이후 턴은 `--resume <UUID>` (RunRequest.Resume로 분기).
- 단일 json envelope의 `.result`에서 최종 텍스트, `.is_error`에서 에러 여부 추출. (출력에서 session_id 파싱 불필요)

### 4.3 Telegram 명령 (UI 계약)

| 입력 | 동작 |
|------|------|
| (일반 텍스트) | Manager 라우팅 → Worker 실행 |
| `/project add <name> <path>` | 프로젝트 등록(경로 존재 검증) |
| `/project remove <name>` | 프로젝트 제거 |
| `/project list` | 프로젝트 + 각 대화 목록, 활성 표시 |
| `/chat new [title]` | 활성 프로젝트에 새 대화 생성·활성화 |
| `/chat list` | 활성 프로젝트의 대화 목록 |
| `/chat use <id>` | 활성 대화 전환(수동 보정) |
| `/cancel` | 진행 중 Worker 종료 |
| `/status` | 현재 활성 프로젝트·대화 표시 |
| `/help`, `/start` | 사용법 안내 |

### 4.4 라우팅 최적화 (선택)

`MANAGER_ALWAYS`(기본 true): 모든 일반 텍스트를 Manager로 라우팅(정확성 우선).
false면: 활성 대화가 있고 최근(<N분) 활동이며 명시적 전환 어구가 없으면 Manager 생략하고 활성 대화 유지(토큰 절약). MVP 기본은 true.

---

## 5. UI/UX (Telegram 상호작용)

### 5.1 대표 플로우

```
사용자: "myapp 로그인 문제 이어서 보자"
봇(typing) → Manager: {project:myapp, conversationId:1, action:resume}
봇: 📂 myapp · 💬 로그인 버그 (이어가기)
   <Worker 작업 결과...>

사용자: "voice 서버에 헬스체크 엔드포인트 새로 만들자"
봇 → Manager: {project:voicesvr, action:new, newTitle:"헬스체크 엔드포인트"}
봇: 📂 voicesvr · 💬 헬스체크 엔드포인트 (새 대화)
   <Worker 작업 결과...>

사용자: "그거 다시 보자"  (애매)
봇 → Manager: {action:clarify, clarify:"어느 대화일까요? 1)로그인 버그 2)헬스체크"}
봇: 🤔 어느 대화일까요? 1) 로그인 버그  2) 헬스체크 엔드포인트
```

### 5.2 상태 표시 규칙

- 작업 시작: typing 액션 + "⏳ 처리 중…"
- 응답: 첫 줄에 라우팅 표기(📂 프로젝트 · 💬 대화 + (이어가기/새 대화))
- 4096자 초과: 여러 메시지로 분할
- 처리 중 새 메시지: "⏳ 이전 작업 처리 중입니다. /cancel 로 취소" (FIFO 큐)

---

## 6. Error Handling

### 6.1 에러 분류·처리

| 상황 | 처리 |
|------|------|
| 미허용 user ID | 무응답(로그만) |
| `claude` 미설치/미탐지 | 시작 시 헬스체크 실패 안내, 명령 시 친절 메시지 |
| Manager JSON 파싱 실패 | §6.3 폴백 |
| Worker 비정상 종료/에러 | 에러 요약 회신 + 로그, 데몬 생존 |
| 타임아웃 초과 | Worker kill + "⏱ 타임아웃" 안내 |
| `/cancel` | 진행 프로세스 kill + "🛑 취소됨" |
| 네트워크(폴링) 단절 | 백오프 재연결 |
| 잘못된 명령 인자 | 사용법 안내 |

### 6.2 사용자 메시지 형식

```
⚠️ 작업 실패: <간단 원인>
(자세한 내용은 로그 참조)
```

### 6.3 Manager 폴백 정책

1. JSON 파싱 실패 → 활성 대화(`GetActive`)가 있으면 그 대화로 resume.
2. 활성 대화도 없으면 → `/chat list` 안내 + "어느 프로젝트/대화에서 할까요?" 되묻기.
3. 라우팅된 project가 미등록명 → clarify로 강등.

---

## 7. Security Considerations

- [ ] allowlist(user ID) 외 전면 차단
- [ ] Worker는 등록된 project.Path를 cwd로만 실행 (임의 경로 금지)
- [ ] `/project add` 경로는 존재 검증 + 절대경로화
- [ ] `--dangerously-skip-permissions` 위험 README 명시, 토큰/저장소 파일 권한 주의
- [ ] Manager는 도구 없이 텍스트 판단만(부작용 없음)
- [ ] 봇 토큰·세션ID 로그 마스킹

---

## 8. Test Plan (Go)

> 웹 L1-L3 대신 Go 테스트 레벨로 매핑. 코드+테스트=1세트(Do 단계 동시 작성).

### 8.1 Test Scope

| Type | Target | Tool | Phase |
|------|--------|------|-------|
| Unit | config 파싱, store CRUD/영속, 라우팅 JSON 파싱, stream-json 파싱, 4096자 분할, auth 필터 | go test | Do |
| Component(mock) | manager 라우팅 흐름(ClaudeClient/StoreRepo 목), clarify 분기, 폴백 | go test + mock | Do |
| Integration(manual) | 실제 봇+claude로 핵심 루프(라우팅→작업→회신), 멀티프로젝트·멀티대화, /cancel | 수동(폰) | Check |

### 8.2 Unit 시나리오 (예)

| # | 대상 | 입력 | 기대 |
|---|------|------|------|
| 1 | config | 정상 txt | 토큰/ID/모델 파싱, 기본값 적용 |
| 2 | config | AllowedUserIDs 누락 | 에러 |
| 3 | store | AddProject(존재경로) | 등록·Save·재Load 일치 |
| 4 | store | NewConversation x2 | 고유 ID 증가, map 보존 |
| 5 | routing parse | `.result`에 잡텍스트+JSON | RouteDecision 정확 추출 |
| 6 | routing parse | JSON 깨짐 | 에러 → 폴백 트리거 |
| 7 | stream-json | system/init 라인 | session_id 캡처 |
| 8 | stream-json | assistant/result 라인 | 텍스트 누적 |
| 9 | chunk | 5000자 | 4096+나머지 2메시지 |
| 10 | auth | 미허용 ID | 거부 |

### 8.3 Component(mock) 시나리오

| # | 시나리오 | 기대 |
|---|----------|------|
| 1 | Route=resume | 해당 conv.SessionID로 Run 호출 |
| 2 | Route=new | 새 conv 생성, Run 후 SessionID 저장 |
| 3 | Route=clarify | Run 미호출, 되묻기 메시지 |
| 4 | Route JSON 실패 | 활성 대화 폴백 또는 되묻기 |

### 8.4 Seed/Fixture

| Fixture | 내용 |
|--------|------|
| sample config.txt | 토큰(더미)/ID/haiku |
| sample store.json | 프로젝트 2 + 대화 3 |
| json envelope 샘플 | result/is_error/session_id 포함 결과 객체 (정상·에러) |

---

## 9. Clean Architecture (Go, Option C)

### 9.1 Layer Structure

| Layer | Responsibility | File |
|-------|---------------|------|
| Presentation | Telegram I/O, 출력 포맷 | `bot.go`, `relay.go` |
| Application | 라우팅 오케스트레이션, 명령 핸들러 | `manager.go`, (bot 내 핸들러) |
| Domain | 타입(Config/Project/Conversation/Route*), 인터페이스 | `types.go` |
| Infrastructure | claude CLI, 저장소 영속 | `runner.go`(ClaudeClient), `store.go`(StoreRepo) |

### 9.2 Dependency Rule

```
Presentation(bot,relay) → Application(manager) → Domain(types,interfaces)
                                  └→ Infrastructure(runner,store) → Domain
규칙: Infra/Presentation은 Domain 인터페이스에만 의존, 서로 직접 의존 금지
```

### 9.3 This Feature's Assignment

| Component | Layer | File |
|-----------|-------|------|
| Telegram 폴링/디스패치 | Presentation | bot.go |
| 출력 분할/표기 | Presentation | relay.go |
| 라우팅/요약 오케스트레이션 | Application | manager.go |
| 도메인 타입·인터페이스 | Domain | types.go |
| Worker/Manager claude 호출 | Infrastructure | runner.go |
| 대화 저장소 | Infrastructure | store.go |

---

## 10. Coding Convention (Go)

### 10.1 Naming

| 대상 | 규칙 | 예 |
|------|------|----|
| 파일 | lower_snake 또는 단어 | `runner.go`, `store.go` |
| Exported | PascalCase | `RouteDecision`, `ClaudeClient` |
| unexported | camelCase | `parseRoute`, `chunkText` |
| 상수 | CamelCase/UPPER | `defaultTimeoutMin` |

### 10.2 기타

- `gofmt`/`go vet` 통과 필수, 에러는 `fmt.Errorf("...: %w", err)` 래핑.
- 외부 입력(JSON/stream) 방어적 파싱, panic 금지(고루틴 recover).
- 동시성: 명령 처리 FIFO(워커 1개), `context.Context`로 취소/타임아웃.

### 10.3 This Feature's Conventions

| Item | Convention |
|------|-----------|
| 패키지 | 단일 `main` |
| 경계 | `ClaudeClient`, `StoreRepo` 인터페이스만 |
| 설정/저장소 | `%USERPROFILE%\.teleclaude\` |
| 로깅 | 표준 log + 민감정보 마스킹 |

---

## 11. Implementation Guide

### 11.1 File Structure

```
teleclaude/
  go.mod
  main.go        # 진입점, run, config 로드, 봇 시작, claude 헬스체크
  config.go      # config.txt 파싱, claude 경로 자동탐지
  types.go       # Domain 타입 + ClaudeClient/StoreRepo 인터페이스
  util.go        # newUUID(세션ID 발급), truncate(요약)
  bot.go         # 폴링, auth, 명령 디스패치(/project,/chat,/cancel,/status), 단일작업
  manager.go     # ClaudeClient.Route 오케스트레이션, clarify, 요약 갱신
  store.go       # StoreRepo(JSON) 구현, 프로젝트/대화 CRUD, 영속
  runner.go      # ClaudeClient 구현: Route(--json-schema)/Run(json envelope), cancel(killTree)
  relay.go       # 4096자 분할, typing, 라우팅 표기
  *_test.go      # 단위/목 테스트
  README.md      # BotFather 토큰/설정/실행 가이드
```

### 11.2 Implementation Order

1. [ ] `types.go` 도메인 타입·인터페이스
2. [ ] `config.go` + 테스트
3. [ ] `store.go` (JSON CRUD) + 테스트
4. [ ] `runner.go` Run(Worker, stream-json, --resume, cancel) + 파싱 테스트
5. [ ] `relay.go` 분할/표기 + 테스트
6. [ ] `manager.go` Route + clarify + 폴백 (목 테스트)
7. [ ] `bot.go` 폴링·auth·디스패치·큐 조립
8. [ ] `main.go` 조립 + 헬스체크
9. [ ] README + 수동 통합 검증

### 11.3 Session Guide

#### Module Map

| Module | Scope Key | Description | Est. Turns |
|--------|-----------|-------------|:---:|
| 코어 스켈레톤 | `module-1` | types, config, main, bot(폴링·auth·큐·/cancel) | 40-50 |
| 저장소+명령 | `module-2` | store(JSON CRUD), /project·/chat 핸들러 | 30-40 |
| 워커+릴레이 | `module-3` | runner.Run(stream-json,--resume,cancel), relay 분할 | 40-50 |
| 매니저 | `module-4` | runner.Route, manager 오케스트레이션, clarify, 요약 | 40-50 |
| 통합+문서 | `module-5` | 조립, README, 수동 검증 | 20-30 |

#### Recommended Session Plan

| Session | Phase | Scope | Turns |
|---------|-------|-------|:---:|
| 1 | Plan+Design | 전체 | 완료 |
| 2 | Do | `--scope module-1,module-2` | 50-60 |
| 3 | Do | `--scope module-3,module-4` | 60-70 |
| 4 | Do+Check | `--scope module-5` + 분석 | 40-50 |

---

## Version History

| Version | Date | Changes | Author |
|---------|------|---------|--------|
| 0.1 | 2026-06-02 | Initial design (Option C, Manager-Worker, 대화저장소) | tyranno1223 |
| 0.2 | 2026-06-02 | 구현 동기화(code=truth): Worker stream-json→`--output-format json` 단일 envelope + `--session-id`/`--resume` UUID(자체 발급), `ClaudeClient.Run` onEvent 제거, `RunRequest.Resume` 추가, `StreamEvent`/`RunResult.SessionID` 제거, Manager `--json-schema`, util.go·/status·MANAGER_ALWAYS 반영 | tyranno1223 |
