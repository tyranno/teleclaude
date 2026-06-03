---
template: plan
version: 1.3
feature: teleclaude
date: 2026-06-02
author: tyranno1223
project: teleclaude
version_project: 0.1.0
---

# teleclaude Planning Document

> **Summary**: 폰 Telegram 봇 1개에 **자연어로 명령**하면, **Manager(Claude)** 가 "어느 프로젝트의 어느 대화인지"를 판단해 라우팅하고, 해당 대화의 **Worker(Claude `--resume`)** 가 그 프로젝트 디렉토리에서 로컬 작업을 수행해 결과를 회신하는 Go 네이티브 단일 .exe 에이전트 (MVP).
>
> **Project**: teleclaude
> **Version**: 0.1.0
> **Author**: tyranno1223
> **Date**: 2026-06-02
> **Status**: Draft

---

## Executive Summary

| Perspective | Content |
|-------------|---------|
| **Problem** | 폰에서 PC의 Claude를 쓰고 싶고, **여러 프로젝트 × 프로젝트마다 여러 대화 주제**를 슬래시 명령 외우지 않고 **말로 자연스럽게** 골라 이어가고 싶다. 기존 솔루션은 tmux/Docker 의존·Windows 미지원이거나 명령어 전환만 지원. |
| **Solution** | Go 단일 .exe가 Telegram 수신 → **Manager(Claude, 경량 모델)** 가 대화 저장소를 보고 자연어 의도를 해석해 {프로젝트, 대화(이어가기/새로)} 라우팅(애매하면 되묻기) → **Worker(Claude `--resume`)** 가 프로젝트 디렉토리에서 작업 → 회신. 대화는 별도 저장소에 영속. |
| **Function/UX Effect** | "myapp의 로그인 문제 이어서 얘기하자" 라고 말하면 알아서 그 대화로 연결. 프로젝트별·주제별 맥락이 분리 유지. OpenClaw식 사용감을 Windows 네이티브 + 자연어 라우팅으로. |
| **Core Value** | 명령 암기 없이 **말로** 내 PC의 여러 프로젝트·여러 대화를 조종하는 개인 Claude 에이전트. Node/Docker/tmux 비의존(단 `claude` CLI 필요). |

---

## Context Anchor

| Key | Value |
|-----|-------|
| **WHY** | 폰에서 자연어로 "어느 프로젝트의 어느 대화"를 골라 PC의 Claude를 이어 쓰고 싶다 (기존 솔루션의 명령어-only·Unix 의존 갭 해소) |
| **WHO** | 본인(단독 사용자). allowlist된 Telegram user ID만 |
| **RISK** | ① `--dangerously-skip-permissions` 임의 로컬 실행 ② Manager 오라우팅으로 잘못된 대화/프로젝트에서 작업 ③ 메시지당 Claude 2회 호출 비용 |
| **SUCCESS** | 자연어 메시지 → Manager가 올바른 프로젝트·대화로 라우팅 → Worker가 해당 디렉토리에서 작업·회신, 대화별 맥락 분리 유지 |
| **SCOPE** | MVP: Manager(Claude 라우팅)+Worker(--resume)+대화저장소+프로젝트레지스트리+/cancel / 후속: Windows 서비스화·토픽 UX·로컬전반제어 |

---

## 1. Overview

### 1.1 Purpose

폰(Telegram)을 Claude 사용 환경으로 만든다. 사용자가 **자연어로** 명령하면, **Manager(Claude)** 가 대화 저장소를 참고해 "어느 프로젝트의 어느 대화"인지 판단(필요시 되묻기)하고, 그 대화의 **Worker(Claude `--resume`)** 가 해당 프로젝트 디렉토리에서 코딩/파일/명령 작업을 수행한 뒤 결과를 회신한다. 프로젝트별·대화 주제별로 맥락이 분리 유지된다.

### 1.2 Background

- 공식 Telegram 플러그인: 세션/토큰당 1프로젝트, Bun 의존, Windows 비공식, 멀티프로젝트는 공식 이슈(#37173) 갭.
- RichardAtCT(Python `/repo`·`/cd`), Clautel(Node 유료, Manager/Worker, 프로젝트당 봇토큰), ccbot/ccgram(tmux) 등은 Unix 중심 또는 명령어 전환 only.
- 본 도구의 차별점: **Manager를 Claude로 지능화**하여 슬래시 명령 없이 **자연어로 프로젝트·대화를 라우팅**. Manager·Worker 둘 다 Claude. Windows 네이티브 단일 .exe.
- 사용자는 OpenClaw(Node)+clawdbot-service(Go Windows 서비스 래퍼) 운영 경험 보유.

### 1.3 Related Documents

- 승인된 설계 메모: `docs/02-design/2026-06-02-teleclaude-go-design.md`
- 레퍼런스: Clautel(Manager/Worker), RichardAtCT(명령체계), clawdbot-service(Windows 서비스 감독)

---

## 2. Scope

### 2.1 In Scope (MVP)

**기반**
- [ ] Telegram 봇 1개, long-polling 수신
- [ ] allowlist된 user ID만 허용 (그 외 무시)
- [ ] 명령 직렬화(1번에 1개) + 처리중 안내(typing/진행표시)
- [ ] `/cancel` — 진행 중인 Worker 작업 취소(프로세스 종료)
- [ ] 기본 타임아웃 초과 시 중단 + 알림
- [ ] 에러/실패를 사용자 친화 메시지로 회신 + 로그
- [ ] 설정 파일 로드 + claude 경로 자동탐지

**프로젝트 레지스트리**
- [ ] `/project add <name> <path>`, `/project remove <name>`, `/project list` (디스크 경로 등록은 명시적)

**대화 저장소 (별도 저장소)**
- [ ] 프로젝트 → 여러 대화 → {sessionId, title, summary, lastActivity} 영속화
- [ ] 백업 수동 명령: `/chat list`, `/chat use <id>`, `/chat new` (Manager 보정용)

**Manager (Claude 라우팅)**
- [ ] 자연어 메시지 해석 → {project, conversation(resume 기존/new), title} 라우팅 결정 (경량 모델, 구조화 출력)
- [ ] 대화 저장소(목록/제목/요약)를 컨텍스트로 사용
- [ ] 애매하면 사용자에게 되묻기(확인/선택), 확실하면 자동 라우팅
- [ ] 라우팅 결과를 회신에 표기 (예: "📂 myapp · 💬 로그인버그")
- [ ] Worker 턴 후 대화 제목/요약 갱신

**Worker (Claude 실행)**
- [ ] 선택된 대화의 `--resume <sid>`로 프로젝트 디렉토리에서 `claude -p ... --output-format stream-json` 실행
- [ ] 로컬 작업 가능하도록 도구 권한 부여 (`--dangerously-skip-permissions`)
- [ ] 새 대화면 새 세션 생성 후 sessionId 저장
- [ ] stream-json 파싱 → Telegram 회신 (4096자 분할)

### 2.2 Out of Scope (후속 단계)

- Windows Service 상시화 (clawdbot-service 패턴 재사용)
- Telegram 토픽(포럼) 기반 대화창 UX
- 모델 선택 UI / 파일 업로드 / 음성 / 미리보기
- OpenClaw식 로컬 머신 전반 제어 확장
- 다중 사용자 / 그룹 채팅
- 자연어로 프로젝트 디스크 경로 추가(보안상 /project add 명시 유지)

---

## 3. Requirements

### 3.1 Functional Requirements

| ID | Requirement | Priority | Status |
|----|-------------|----------|--------|
| FR-01 | Telegram long-polling으로 메시지 수신 | High | Pending |
| FR-02 | allowlist user ID 인증 필터 (미허용은 무시) | High | Pending |
| FR-03 | 명령 직렬화 + 처리중 메시지 표시(typing/진행) | Medium | Pending |
| FR-04 | `/cancel`로 진행 중 Worker 프로세스 종료 | Medium | Pending |
| FR-05 | 기본 타임아웃 초과 시 작업 중단 + 알림 | Medium | Pending |
| FR-06 | 에러/실패를 사용자 친화 메시지로 회신 + 로그 | Medium | Pending |
| FR-07 | 설정 파일 로드(토큰/allowlist/모델/CLAUDE_PATH) + claude 경로 자동탐지 | High | Pending |
| FR-08 | 프로젝트 레지스트리: `/project add <name> <path>`, `/project remove <name>`, `/project list` | High | Pending |
| FR-09 | 대화 저장소: 프로젝트→여러 대화→{sessionId,title,summary,lastActivity} 영속화 | High | Pending |
| FR-10 | 백업 수동 명령: `/chat list`, `/chat use <id>`, `/chat new` | Medium | Pending |
| FR-M1 | Manager: 자연어 메시지 → {project, conversation(resume/new), title} 라우팅 결정(경량 모델, 구조화 출력) | High | Pending |
| FR-M2 | Manager: 대화 저장소(목록/제목/요약)를 라우팅 컨텍스트로 사용 | High | Pending |
| FR-M3 | Manager: 애매하면 되묻기(확인/선택), 확실하면 자동 라우팅 | High | Pending |
| FR-M4 | Manager: 라우팅 결과를 회신에 표기(프로젝트·대화) | Medium | Pending |
| FR-M5 | Manager: Worker 턴 후 대화 제목/요약 갱신 | Medium | Pending |
| FR-W1 | Worker: 선택 대화의 `--resume <sid>`로 프로젝트 디렉토리에서 claude 실행(cwd=project path) | High | Pending |
| FR-W2 | Worker: `--dangerously-skip-permissions`로 로컬 작업 도구 권한 부여 | High | Pending |
| FR-W3 | Worker: 새 대화면 새 세션 생성 후 sessionId 저장 | High | Pending |
| FR-W4 | Worker: `--output-format stream-json` 파싱 → 4096자 분할 회신 | High | Pending |

### 3.2 Non-Functional Requirements

| Category | Criteria | Measurement Method |
|----------|----------|-------------------|
| Portability | Windows 10/11 네이티브 단일 .exe, WSL/Docker/tmux 비의존 | 클린 Windows에서 단일 exe 실행 |
| Dependency | 외부 런타임 무의존 (단 `claude` CLI 설치·로그인 전제) | exe 단독 배포 후 동작 확인 |
| Security | allowlist 외 차단 + Worker WORKDIR 한정 운용 | 비허용 ID 무응답 확인 |
| Cost/Token | Manager는 경량 모델·최소 컨텍스트(대화목록+메시지)로 Worker 대비 오버헤드 최소 | 토큰 사용량 관찰 |
| Latency | 라우팅 1회 왕복 추가 허용, typing으로 체감 보완 | 수동 관찰 |
| Routing Accuracy | 명확한 의도는 자동 정확 라우팅, 애매는 되묻기로 오작동 방지 | 시나리오 테스트 |
| Reliability | Worker/Manager 비정상 종료 시 데몬 생존·다음 명령 처리 | 강제종료 주입 테스트 |

---

## 4. Success Criteria

### 4.1 Definition of Done

- [ ] 자연어 메시지 → Manager가 올바른 프로젝트·대화로 라우팅 → Worker가 해당 디렉토리에서 작업·회신 (핵심 루프)
- [ ] 한 프로젝트 안의 서로 다른 대화 주제가 **각각 독립 세션(`--resume`)으로 분리** 유지됨
- [ ] 애매한 의도일 때 Manager가 되물어 오라우팅을 방지함
- [ ] 로컬 파일 생성/수정/명령 실행이 수행됨 (로컬 작업 검증)
- [ ] 봇 1개로 여러 프로젝트 등록·사용됨
- [ ] allowlist 외 사용자 차단, `/cancel` 동작, 4096자 분할 전송
- [ ] 단위 테스트 작성·통과, README(BotFather 토큰 발급/설정/실행) 작성

### 4.2 Quality Criteria

- [ ] `go vet` / `gofmt` 클린, `go build` 성공(단일 exe)
- [ ] 핵심 로직(config 파싱, auth, 라우팅 JSON 파싱, 대화저장소 CRUD, 출력 분할, stream-json 파싱) 단위 테스트 커버

---

## 5. Risks and Mitigation

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|------------|
| `--dangerously-skip-permissions` 임의 명령 실행 보안 노출 | High | Medium | 엄격 allowlist, WORKDIR 한정, 설정파일 권한 관리, 위험 명시 |
| **Manager 오라우팅** → 잘못된 대화/프로젝트에서 작업 | High | Medium | 애매시 되묻기, 라우팅 결과 회신 표기, `/chat use`·`/project` 수동 보정, 라우팅은 등록된 프로젝트 내에서만 |
| **메시지당 Claude 2회 호출** 비용/지연 | Medium | High | Manager 경량 모델(Haiku) + 최소 컨텍스트, 명백한 후속은 직전 대화 유지로 라우팅 생략 가능 |
| Manager 구조화 출력 파싱 실패 | Medium | Medium | JSON 지시 + 방어적 파싱, 실패 시 직전 활성 대화 폴백 또는 사용자에게 선택 요청 |
| `claude` stream-json 포맷 버전차 | Medium | Medium | result/sessionId 방어적 파싱, 실패 시 raw 텍스트 폴백 |
| Telegram long-polling 네트워크 단절 | Medium | Medium | 재연결/백오프, 에러 로그 |
| 긴 작업으로 멈춤처럼 보임 | Medium | High | typing/진행표시, 타임아웃, `/cancel` |
| Windows 경로/한글 인코딩 | Medium | Medium | UTF-8 처리, filepath 사용, 수동 검증 |
| `claude` 미설치/미로그인 | High | Low | 시작 시 경로 탐지·헬스체크, 안내 메시지 |

---

## 6. Impact Analysis

### 6.1 Changed Resources

| Resource | Type | Change Description |
|----------|------|--------------------|
| 신규 Go 모듈 `teleclaude` | New project | 신규 생성. 기존 코드 변경 없음 |
| `%USERPROFILE%\.teleclaude\` | Config/State dir | 설정·프로젝트 레지스트리·대화 저장소 보관 |

### 6.2 Current Consumers

신규 프로젝트로 기존 소비자 없음. 기존 OpenClaw 봇과 충돌 방지를 위해 **별도 Telegram 봇 토큰** 사용 권장.

### 6.3 Verification

- [ ] 별도 봇 토큰으로 기존 OpenClaw 봇과 분리 확인
- [ ] `claude` CLI 단독 동작 영향 없음 확인

---

## 7. Architecture Considerations

### 7.1 Project Level Selection

| Level | Characteristics | Selected |
|-------|-----------------|:--------:|
| **Starter** | 단순 단일 목적 도구 | ☐ |
| **Dynamic** | 다중 컴포넌트(Manager/Worker/Store) 협력, 외부 프로세스 통합 | ☑ (컴포넌트 분리·라우팅 로직으로 Starter 초과) |
| **Enterprise** | 엄격 레이어/MSA | ☐ |

> Manager-Worker 분리 + 대화 저장소 + 라우팅 로직으로 단순 도구를 넘어 Dynamic급 구조.

### 7.2 Key Architectural Decisions

| Decision | Options | Selected | Rationale |
|----------|---------|----------|-----------|
| 언어/런타임 | Go / Node / Python | **Go** | 단일 정적 exe, Windows 네이티브, 런타임 무의존 |
| 패턴 | Manager-Worker(둘 다 Claude) / 단일 | **Manager-Worker** | 자연어 라우팅 + 대화별 세션 분리 |
| Worker 구동 | 메시지당 1회(A) / 상시 PTY / API직접 | **A (claude -p)** | 단순·견고·Windows 안정, --resume 연속성 |
| Manager 모델 | Haiku / Sonnet / Opus | **Haiku(경량)** | 라우팅은 가벼움, 비용·지연 최소 |
| Worker 모델 | 기본/지정 | **설정값(기본 inherit, 추후 Sonnet/Opus)** | 실제 작업 품질 |
| 라우팅 출력 | JSON 지시 파싱 / 함수호출 | **`claude -p --model <haiku> --output-format json` + JSON 파싱** | CLI로 구조화 결과 확보 |
| 대화 저장소 | JSON 파일 / SQLite | **JSON 파일(MVP)** → SQLite(후속) | 단일 사용자, 단순. 규모 커지면 전환 |
| Telegram | go-telegram-bot-api/v5 | **go-telegram-bot-api/v5** | 성숙·long-polling·NAT 뒤 동작 |
| Telegram 출력 | plain / MarkdownV2 | **plain(MVP)** | 이스케이프 버그 회피 |
| 설정 | txt key=value | **txt key=value** | clawdbot-service 패턴, 단순 |

### 7.3 Folder Structure Preview

```
teleclaude/
  main.go         # 진입점, run 명령, 설정 로드, 봇 시작
  config.go       # 설정 파싱, claude 경로 탐지, 모델 설정
  bot.go          # Telegram 폴링, 인증 필터, 명령 디스패치(/project,/chat,/cancel), 큐
  manager.go      # Claude 라우팅 호출, 의도 해석, 되묻기, 제목/요약 갱신
  store.go        # 대화 저장소(별도): 프로젝트 레지스트리 + 프로젝트→대화→세션, 영속(JSON)
  runner.go       # Worker: claude 실행/취소, cwd=프로젝트, --resume, stream-json 캡처
  relay.go        # 출력 파싱, 4096자 분할, typing
  *_test.go       # 단위 테스트
  go.mod
  README.md
```

데이터 흐름:
```
Telegram msg → bot(auth,queue) → manager(Claude 라우팅: project+conv)
  → [애매] 사용자에게 되묻기
  → [확정] store에서 conv.sessionId 조회 → runner(Worker claude --resume, cwd=path)
  → relay(stream-json→Telegram) → manager(제목/요약 갱신, store 저장)
```

---

## 8. Convention Prerequisites

### 8.1 Existing Project Conventions

- [ ] CLAUDE.md: 없음(신규) → 표준 Go 컨벤션
- [x] Go 표준: `gofmt`, `go vet`

### 8.2 Conventions to Define/Verify

| Category | Current State | To Define | Priority |
|----------|---------------|-----------|:--------:|
| Naming | missing | Go 표준 | High |
| Folder structure | missing | 단일 패키지 main (7.3) | High |
| Error handling | missing | error 반환 + 사용자 친화 변환 | Medium |
| Config/Store | missing | `.teleclaude/` 설정·저장소 스키마 | High |
| Routing prompt | missing | Manager 시스템 프롬프트·JSON 스키마 고정 | High |

### 8.3 Configuration & Storage

설정 `%USERPROFILE%\.teleclaude\config.txt`:

| Key | Purpose | Required |
|-----|---------|:--------:|
| `TELEGRAM_BOT_TOKEN` | 봇 토큰 (BotFather 발급) | ☑ |
| `ALLOWED_USER_IDS` | 허용 user ID(쉼표, @userinfobot으로 확인) | ☑ |
| `MANAGER_MODEL` | Manager 라우팅 모델(기본 haiku) | ☐ |
| `WORKER_MODEL` | Worker 작업 모델(비우면 claude 기본) | ☐ |
| `CLAUDE_PATH` | claude 실행 경로(비우면 자동탐지) | ☐ |
| `TIMEOUT_MINUTES` | 명령 타임아웃(기본 10) | ☐ |

대화 저장소 `%USERPROFILE%\.teleclaude\store.json` (앱이 관리):

```json
{
  "projects": {
    "myapp": {
      "path": "C:\\Project\\myapp",
      "conversations": {
        "1": { "title": "로그인 버그", "summary": "세션 만료 처리 수정 중", "sessionId": "abc-123", "lastActivity": "2026-06-02T10:00:00Z" },
        "2": { "title": "결제 모듈", "summary": "PG 연동 설계", "sessionId": "def-456", "lastActivity": "2026-06-02T09:00:00Z" }
      }
    },
    "voicesvr": { "path": "C:\\Project\\88.MyProject\\voice-chat-server", "conversations": {} }
  }
}
```

> 디스크 경로 등록은 보안상 `/project add`로 명시적으로만. Manager는 **등록된 프로젝트 내에서만** 라우팅.

---

## 9. Next Steps

1. [ ] Design 문서 작성 (`/pdca design teleclaude`) — Manager 프롬프트·라우팅 JSON 스키마·저장소 스키마 확정
2. [ ] 구현 (`/pdca do teleclaude`)
3. [ ] Gap 분석 (`/pdca analyze teleclaude`)

---

## Version History

| Version | Date | Changes | Author |
|---------|------|---------|--------|
| 0.1 | 2026-06-02 | Initial draft (brainstorming 승인 기반) | tyranno1223 |
| 0.2 | 2026-06-02 | 멀티프로젝트(봇1개 전환·프로젝트별 세션) MVP 포함, BotFather 확정 | tyranno1223 |
| 0.3 | 2026-06-02 | Manager-Worker(둘 다 Claude) + 자연어 라우팅 + 대화 저장소(프로젝트→여러 대화) 도입, MVP 전면 개정 | tyranno1223 |
