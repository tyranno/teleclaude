# teleclaude 설치 · 설정 가이드

폰 Telegram 봇 1개로 Windows PC의 **여러 프로젝트 · 여러 대화**를 **자연어로** 조종하는 Go 네이티브 Claude 에이전트.
Manager(Claude)가 "어느 프로젝트의 어느 대화인지" 라우팅하고, Worker(Claude `--resume`)가 해당 폴더에서 실제 작업을 수행합니다.

> 실측 환경: Windows 11, Go 1.25, claude CLI 2.1.159. 아래 절차는 실제 셋업·검증한 그대로입니다.

---

## 0. 동작 개요

```
[폰 Telegram] ──(long polling)──> [teleclaude.exe (Go, 단일 바이너리)]
   "myapp 로그인 버그 이어서 보자"        │ allowlist 인증 · 단일 작업 큐
                                         ▼
                       Manager(claude, haiku, --json-schema)
                         → {project: myapp, conv: 3, action: resume}
                                         ▼
                       대화 저장소(store.json: 프로젝트→대화→세션UUID)
                                         ▼
                       Worker(claude -p --output-format json
                              --session-id/--resume, cwd=프로젝트)
                         → 실제 파일/명령 작업 → 결과
                                         ▼
                       Telegram으로 4096자 분할 회신
```

- 메시지마다 `claude`를 **새 프로세스로** 띄웁니다(접근 A). 대화 연속성은 `--resume`로 유지됩니다.
- 각 spawn은 `--strict-mcp-config --setting-sources project,local`로 **격리**됩니다(아래 §8).

---

## ⚡ 빠른 시작 (설정 마법사 — 권장)

설정 파일을 손으로 만들 필요 없이, **실행하면 마법사가 잡아줍니다.**

```powershell
go build -o teleclaude.exe .
.\teleclaude.exe run     # 설정 없으면 마법사 자동 시작 (또는 .\teleclaude.exe setup)
```

마법사 순서:
1. **claude 로그인 확인** (유일한 사전 전제 — 안 되어 있으면 안내 후 중단)
2. **봇 토큰** 입력 → `getMe`로 즉시 검증 (BotFather 안내 포함)
3. **user ID 자동 감지** → 봇에게 메시지 1개 보내고 Enter (수동 입력도 가능)
4. **(선택) 첫 프로젝트** 폴더 등록
5. config 자동 저장 → 바로 실행

> 아래 §1~§6은 마법사를 안 쓰고 **수동으로** 할 때의 상세 절차입니다.

---

## 1. 사전 준비물

| 항목 | 확인 방법 |
|------|-----------|
| **claude CLI** 설치 + 로그인 | `claude --version` 동작. OAuth(claude.ai) 로그인 상태 |
| **Go** 1.22+ | `go version` |
| **Telegram 봇 토큰** | 아래 §2 |
| **본인 Telegram user ID** | 아래 §3 |

> claude CLI는 OAuth/keychain 로그인을 사용합니다. teleclaude는 그 로그인을 그대로 위임합니다(별도 API 키 불필요).

---

## 2. Telegram 봇 만들기 (BotFather)

1. 텔레그램에서 [@BotFather](https://t.me/BotFather) 열기 → `/newbot`
2. 봇 표시 이름 입력 → username 입력 (**반드시 `bot`으로 끝나야 함**, 예: `DwNoteDevBot`)
3. 발급된 **토큰** 복사 (형식: `8980469547:AAF-...`)

> ⚠️ 토큰은 비밀번호입니다. 노출되면 BotFather `/revoke`로 즉시 재발급하세요.
> 기존에 다른 봇(OpenClaw 등)이 있다면 **충돌 방지를 위해 새 봇**을 권장합니다.

토큰 확인:
```powershell
curl.exe -s "https://api.telegram.org/bot<토큰>/getMe"
# {"ok":true,"result":{"username":"DwNoteDevBot",...}} 면 정상
```

---

## 3. 본인 Telegram user ID 알아내기

방법 A — [@userinfobot](https://t.me/userinfobot)에게 아무 메시지나 보내면 숫자 ID를 알려줍니다.

방법 B — 봇에게 메시지 1개 보낸 뒤 getUpdates로 추출:
```powershell
# 1) 텔레그램에서 내 봇에게 "안녕" 전송
curl.exe -s "https://api.telegram.org/bot<토큰>/getUpdates"
# result[].message.from.id 가 내 user ID (예: 6723802240)
```

---

## 4. 설정 파일 `%USERPROFILE%\.teleclaude\config.txt`

```ini
TELEGRAM_BOT_TOKEN=8980469547:AAF-...        # 필수
ALLOWED_USER_IDS=6723802240                  # 필수, 쉼표로 여러 명 가능
# --- 선택 ---
MANAGER_MODEL=haiku        # 라우팅용 경량 모델 (기본 haiku, 저렴·빠름)
WORKER_MODEL=              # 작업용 모델. 비우면 claude 기본(보통 opus). 가벼운 테스트는 haiku
CLAUDE_PATH=              # 비우면 자동 탐지 (PATH → npm/.local/bin 등)
TIMEOUT_MINUTES=10        # 한 작업 최대 시간
MANAGER_ALWAYS=true       # true=매 메시지 라우팅(정확). false=활성 대화 유지로 라우팅 생략(빠름·저렴)
```

| 키 | 설명 |
|----|------|
| `TELEGRAM_BOT_TOKEN` | BotFather 토큰 (필수) |
| `ALLOWED_USER_IDS` | 허용 user ID 목록. **여기 없는 사람은 무시됨**(보안 핵심) |
| `MANAGER_MODEL` | 라우팅 판단 모델. haiku 권장(라우팅은 가벼움) |
| `WORKER_MODEL` | 실제 코딩 작업 모델. 진지한 개발은 비우거나 `sonnet`/`opus` |
| `CLAUDE_PATH` | claude 실행 파일 경로. 비우면 자동 탐지 |
| `TIMEOUT_MINUTES` | 작업 타임아웃(기본 10분) |
| `MANAGER_ALWAYS` | 라우팅 최적화 스위치(§7 지연 참고) |

---

## 5. 프로젝트 등록

봇은 **등록된 프로젝트 폴더 안에서만** claude를 실행합니다(보안). 두 가지 방법:

### 방법 A — 봇에서 명령 (권장)
```
/project add teleclaude C:\Project\88.MyProject\Teleclaude
/project add voicesvr   C:\Project\88.MyProject\voice-chat-server
/project list
```

### 방법 B — 파일 직접 작성 `%USERPROFILE%\.teleclaude\store.json`
```json
{
  "active": { "project": "", "conversationId": "" },
  "projects": {
    "teleclaude": { "path": "C:\\Project\\88.MyProject\\Teleclaude", "conversations": {} },
    "voicesvr":   { "path": "C:\\Project\\88.MyProject\\voice-chat-server", "conversations": {} }
  }
}
```
> 경로는 실제 존재하는 폴더여야 합니다. 백슬래시는 JSON에서 `\\`로 이스케이프.

---

## 6. 빌드 & 실행

### 권장: launcher.ps1로 실행 (핫스왑 업데이트 지원)

```powershell
cd C:\Project\88.MyProject\Teleclaude
go build -o teleclaude.exe .
.\launcher.ps1                             # teleclaude.exe 실행 + 자동 재시작 루프
```

`launcher.ps1`은 teleclaude.exe를 감싸는 루프입니다. `!update` 명령으로 텔레그램에서 새 버전을 빌드하면 자동으로 교체·재시작합니다(아래 §6-1).

정상 기동 로그:
```
[launcher] teleclaude 시작 (C:\Project\88.MyProject\Teleclaude)
[main] claude: C:\Users\<you>\.local\bin\claude.exe
[main] claude version: 2.1.159 (Claude Code)
[main] allowlist: [6723802240], manager=haiku, worker="haiku"
[bot] @DwNoteDevBot online, long-polling started
```

### 직접 실행 (업데이트 불필요한 경우)
```powershell
.\teleclaude.exe run                       # 기본 config.txt 사용
.\teleclaude.exe run C:\path\to\config.txt # 다른 설정 파일
```

### 백그라운드(콘솔창 없이) 실행
```powershell
Start-Process -WindowStyle Hidden -FilePath .\launcher.ps1
# 중지:
Get-Process teleclaude | Stop-Process -Force
```
> 부팅 시 자동 시작/크래시 자동 재시작이 필요하면, 자매 프로젝트 `clawdbot-service`의 Windows Service 래퍼 패턴을 적용할 수 있습니다(후속 단계).

---

## 6-1. 텔레그램에서 버전 업데이트 (`!update`)

Windows에서는 실행 중인 `.exe`를 직접 덮어쓸 수 없습니다. `launcher.ps1` + `!update` 명령으로 이 문제를 해결합니다.

```
[텔레그램] !update
    ↓
teleclaude: go build -o teleclaude_new.exe . 실행 (소스 디렉토리)
    ↓ 빌드 성공
"✅ 빌드 성공! launcher.ps1이 교체 후 재시작합니다." 전송
    ↓
teleclaude exit(42) → 봇 일시 오프라인 (~5~10초)
    ↓
launcher.ps1: exit 42 감지 → teleclaude_new.exe → teleclaude.exe 교체 → 재시작
    ↓
새 버전 봇 온라인 (Telegram long-polling 재연결)
```

- 빌드 **실패** 시: 오류 메시지만 전송, 기존 프로세스 계속 실행 (안전)
- `launcher.ps1` 없이 직접 실행 중이면 `!update`는 종료만 하고 재시작이 안 됩니다

---

## 7. 사용법

### 자연어 (기본)
그냥 말하면 Manager가 알아서 프로젝트·대화를 고릅니다:
```
나: teleclaude 프로젝트가 뭐 하는 건지 한 줄로 설명해줘
봇: 📂 teleclaude · 💬 teleclaude 설명 (새 대화)
    <claude가 그 폴더를 읽고 답변>

나: 방금 그거 어느 언어로 짰어?
봇: 📂 teleclaude · 💬 teleclaude 설명 (이어가기)   ← 같은 대화 resume
    Go 입니다.

나: HELLO.txt 만들고 "hi" 써줘
봇: <실제로 파일 생성>
```
애매하면 봇이 되묻습니다(🤔 1) A  2) B).

### 명령어
| 명령 | 설명 |
|------|------|
| `!project add <이름> <경로>` | 프로젝트 등록 |
| `!project remove <이름>` | 제거 |
| `!project list` | 목록(⭐=활성) |
| `!chat new [제목]` | 활성 프로젝트에 새 대화 |
| `!chat list` | 대화 목록 |
| `!chat use <id>` | 대화 수동 전환 |
| `!status` | 현재 활성 대화 및 실행 중 작업 |
| `!cancel` | 진행 중 작업 취소 |
| `!update` | 새 버전 빌드 & 자동 재시작 (launcher.ps1 필요) |
| `!help` | 도움말 |

---

## 8. 격리 동작 (중요)

각 `claude` spawn에는 다음이 자동 적용됩니다:
- `--strict-mcp-config` → **글로벌 MCP 서버 전부 무시**(serena·context7·figma·bkend 등 안 뜸). 콜드스타트·노이즈 대폭 감소.
- `--setting-sources project,local` → **user(전역) 설정 제외**. 다른 프로젝트 디렉토리 누수·전역 output-style 영향 차단.
- Worker만 `--dangerously-skip-permissions` → 폰에서 승인 없이 로컬 작업 가능(그래서 **allowlist가 유일한 방어선**).
- OAuth/keychain 인증은 그대로 유지됩니다(`--bare`는 인증을 깨므로 사용하지 않음).

---

## 9. 보안

- `ALLOWED_USER_IDS` 외 사용자는 **전부 무시**됩니다(응답·로그만).
- Worker는 `--dangerously-skip-permissions`로 돕니다 → 등록된 프로젝트 폴더에서 **임의 명령/파일 작업 가능**. 토큰·config.txt를 안전하게 보관하고, allowlist를 최소로 유지하세요.
- `config.txt`, `store.json`은 `%USERPROFILE%\.teleclaude\`에 있으며 git에 올라가지 않습니다(.gitignore).
- 토큰이 노출되면 BotFather `/revoke`.

---

## 10. 트러블슈팅 (실제 겪은 사례)

| 증상 | 원인 / 해결 |
|------|-------------|
| 봇이 응답 없음 | ① allowlist에 내 ID 없음 → config 확인 ② claude 미로그인 → `claude --version`·로그인 ③ 기동 로그 확인 |
| `claude CLI를 찾을 수 없습니다` | PATH에 claude 없음 → `CLAUDE_PATH=` 명시 |
| **serena 등 MCP가 매번 뜸** | 격리 플래그 누락 → 최신 빌드 사용(§8, `--strict-mcp-config` 적용됨) |
| 응답이 장황/다른 프로젝트까지 나열 | 전역 설정(output-style·추가 디렉토리) 누수 → 최신 빌드(`--setting-sources project,local`) |
| 응답이 느림(십수 초) | `claude -p` 콜드스타트가 호출당 고정 비용. 메시지당 Manager+Worker 2회 → `MANAGER_ALWAYS=false`로 활성 대화 시 라우팅 생략 |
| "⏳ 이전 작업 처리 중" | 한 번에 한 작업만 처리(직렬화). `/cancel`로 취소 |
| 코딩 품질이 약함 | `WORKER_MODEL=`을 비우거나 `sonnet`/`opus`로(봇 재시작) |
| 설정/코드 바꾼 게 반영 안 됨 | launcher.ps1 사용 중이면 텔레그램 `!update`로 자동 빌드·재시작. 직접 실행 중이면 `Stop-Process teleclaude` → `go build` → 다시 `run` |

---

## 11. 검증 상태 (2026-06-04, 실측)

| 항목 | 상태 |
|------|------|
| Manager 라우팅 (haiku, structured_output) | ✅ 실측 (myapp/1/resume 정확) |
| Worker 실행 + `--resume` 연속성 | ✅ 실측 ("PONG" 기억) |
| 로컬 파일 생성 (`--dangerously-skip-permissions`) | ✅ 실측 (HELLO.txt 생성) |
| 격리 플래그 (MCP off, 인증 유지) | ✅ 실측 |
| Telegram 폴링 왕복 (폰↔봇) | ✅ 실측 (라우팅 헤더+응답 수신) |
| 단위/목 테스트 | ✅ 37개 통과 |
| 통합 테스트(`go test -tags integration`) | ✅ Route/Run/Resume 통과 |

알려진 한계: 콜드스타트 지연(호출당 십수 초), Windows Service 상시화·Telegram 토픽 UX는 후속.
