# teleclaude (Go Native) — 승인된 설계안

작성일: 2026-06-02 · 상태: 승인됨 (brainstorming 완료) · 다음: PDCA

## 목표
- Telegram을 "Claude 사용 환경"으로 사용: 폰에서 Claude에게 명령 → 내 Windows PC에 설치된 `claude` CLI가 수행 → 결과를 Telegram으로 회신.
- 작업 디렉토리에서 코딩/작업 수행. 향후 OpenClaw식 로컬 머신 전반 제어로 확장.
- Go 네이티브 단일 `.exe`, Windows 네이티브 (WSL/Docker/tmux 불필요).

## 배경 / 경쟁 지형 (조사 결과 요약)
- `kirikov/teleclaude`: 웹 터미널 공유(Telegram 아님) — 무관.
- 공식 Telegram 플러그인(`telegram@claude-plugins-official`): Bun MCP, **세션/토큰당 1프로젝트** — 멀티프로젝트 갭(공식 issue #37173).
- RichardAtCT/claude-code-telegram (Python): 봇 1개로 `/repo`·`/cd` 멀티프로젝트 — Windows 미검증. **명령체계 레퍼런스.**
- Clautel (Node, 유료): Manager+Worker, 프로젝트당 봇 토큰 1개.
- 다수 솔루션이 tmux(Unix) 의존 또는 Windows 네이티브 미지원.
- **빈 갭 = 무료 + Windows 네이티브 단일 .exe + 봇1개 멀티프로젝트 + 상시 서비스** → 본 프로젝트의 명분.

## 구동 방식 결정: 접근 A (메시지당 1회 실행)
- `claude -p "<프롬프트>" --output-format stream-json --resume <세션id>` (cwd=WORKDIR).
- 대화 연속성은 `--resume`로 유지. 프로세스는 매 턴 종료 → 크래시/재시작에 강함, Windows에서 안정적.
- (대안 B PTY/ConPTY, C API직접은 복잡/과대 → 기각)

## MVP 범위
포함:
- Telegram 봇 1개, **allowlist된 user ID만** 접근.
- 텍스트 메시지 → `claude` CLI 실행(도구 권한 부여 → 로컬 작업 가능) → 응답 회신(4096자 분할).
- 작업 디렉토리 1개 고정. 포그라운드 실행으로 검증.

제외(이후 단계):
- 멀티프로젝트 `/project`·`/cd` 전환, 프로젝트별 세션 영속.
- Windows Service 상시화 (clawdbot-service 패턴 재사용).
- 모델 선택, 파일 업로드, 음성, 미리보기, OpenClaw식 로컬 머신 전반 제어.

## 아키텍처 (MVP)
```
[Telegram] --long polling--> [teleclaude.exe (Go)]
  ├ auth: allowlist user ID
  ├ queue: 명령 직렬화(1개씩)
  ├ runner: claude -p ... --output-format stream-json --resume <sid> (cwd=WORKDIR)
  └ relay: 출력 4096자 분할 전송 + typing/진행표시
        --> [claude CLI(로컬)] Bash·파일 도구로 실제 작업
```

## 컴포넌트 (Go 파일)
| 파일 | 책임 |
|---|---|
| main.go | 진입점, 설정 로드, 봇 시작, `run` 명령 |
| config.go | 토큰·allowlist·WORKDIR·claude 경로 로드 |
| bot.go | Telegram 폴링, 인증 필터, 디스패치 |
| runner.go | claude 실행, 세션ID 관리(--resume), stream-json 캡처 |
| relay.go | 출력 포맷·4096자 분할·typing/진행표시 |
| session.go | 세션ID 저장/조회 (MVP: 단일 세션) |

## 데이터 흐름
1. 폰 텍스트 전송 → 2. user ID allowlist 확인(아니면 무시) → 3. 큐 적재(처리중이면 "⏳ 처리 중") → 4. runner가 `claude -p` 실행(첫 실행 시 session_id 추출·저장, 이후 --resume) → 5. relay가 stream-json 이벤트를 Telegram 전송, 최종 결과 분할 → 6. 에러는 친절 메시지 + 로그.

## 보안 / 권한 (MVP)
- 엄격한 allowlist (내 Telegram user ID만).
- 로컬 작업을 폰에서 승인 UI 없이 하려면 `--dangerously-skip-permissions` 필요. 본인 PC + 단독 allowlist 전제로 MVP 채택. 위험성(무엇이든 실행 가능) 명시, WORKDIR 기준 운용.

## 설정 파일 `%USERPROFILE%\.teleclaude\config.txt`
```
TELEGRAM_BOT_TOKEN=...
ALLOWED_USER_IDS=123456789
WORKDIR=C:\Project\88.MyProject\...
CLAUDE_PATH=claude   # 비우면 자동 탐지
```

## 기술 스택
- Go 1.22+, 단일 정적 .exe
- go-telegram-bot-api/v5 (long polling)
- stdlib os/exec, encoding/json
- 출력은 MVP에서 plain text (MarkdownV2 이스케이프 회피)

## 실행 모드
- MVP: 포그라운드 `teleclaude run` → 안정화 후 Windows Service (clawdbot-service 패턴 재사용)

## 테스트
- 단위: config 파싱, auth 필터, 출력 분할, stream-json 파싱(목)
- 수동: 폰에서 메시지 → 작업 수행 확인

## 전제 조건
- PC에 `claude` CLI 설치 및 로그인 완료.
- BotFather로 Telegram 봇 토큰 발급, 본인 user ID 확인(@userinfobot).
