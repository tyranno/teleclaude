# teleclaude

폰 Telegram 봇 1개로 **내 Windows PC의 여러 프로젝트·여러 대화**를 **자연어로** 골라가며,
PC에 설치된 `claude` CLI로 작업을 수행하고 결과를 받아보는 **Go 네이티브 단일 .exe** 에이전트.

- **Manager(Claude 경량 모델)** 가 "어느 프로젝트의 어느 대화인지"를 자연어로 판단(애매하면 되묻기)
- **Worker(Claude `--resume`)** 가 해당 프로젝트 디렉토리에서 실제 작업 (대화별 맥락 분리)
- Node/Docker/tmux 불필요 (단, `claude` CLI 설치·로그인 필요)

> ⚠️ Worker는 `--dangerously-skip-permissions`로 실행되어 **로컬에서 임의 명령/파일 작업이 가능**합니다.
> 반드시 **본인 Telegram user ID만 allowlist**에 두고, 봇 토큰·설정 파일을 안전하게 보관하세요.

---

## 1. 빠른 시작 (설정 마법사)

**유일한 사전 전제**: `claude` 설치 + 로그인 (`claude --version` 동작).

```powershell
go build -o teleclaude.exe .
.\launcher.ps1                # teleclaude.exe 실행 + 핫스왑 업데이트 지원
# 또는 단순 실행:
.\teleclaude.exe run          # 설정이 없으면 설정 마법사가 자동으로 시작됩니다
```

처음 실행하면 마법사가 안내합니다 — **설정 파일을 손으로 만들 필요 없음**:
1. **봇 만들기 + 토큰** — 봇이 없으면 마법사가 [@BotFather](https://t.me/BotFather)로 만드는 5단계(/newbot → 이름 → username → 토큰)를 화면에 안내합니다. 토큰을 붙여넣으면 즉시 검증.
2. **내 계정 연결** — 안내대로 봇에게 메시지 한 번 보내면 **user ID 자동 감지**
3. **(선택) 첫 프로젝트 폴더** 등록 (나중에 `/project add` 가능)

→ 끝나면 바로 봇이 켜집니다. 잊고 다시 켜도 실행이 곧 설정입니다.

> 설정만 다시 잡기: `.\teleclaude.exe setup`

<details>
<summary>고급: 설정 파일 수동 작성</summary>

`%USERPROFILE%\.teleclaude\config.txt`:
```ini
TELEGRAM_BOT_TOKEN=123456789:AAH...
ALLOWED_USER_IDS=123456789
MANAGER_MODEL=haiku          # 라우팅용 경량 모델 (기본 haiku)
WORKER_MODEL=                 # 작업용 모델 (비우면 claude 기본)
CLAUDE_PATH=                  # 비우면 자동 탐지
TIMEOUT_MINUTES=10           # 작업 타임아웃
MANAGER_ALWAYS=true          # true=매 메시지 라우팅, false=활성 대화 유지로 토큰 절약
```
</details>

## 4. 사용법

봇에게 **그냥 말하면** 됩니다:

```
나: myapp 로그인 버그 이어서 보자
봇: 📂 myapp · 💬 로그인 버그 (이어가기)
    <작업 결과...>

나: voice 서버에 헬스체크 엔드포인트 새로 만들자
봇: 📂 voicesvr · 💬 헬스체크 엔드포인트 (새 대화)
    <작업 결과...>

나: 그거 다시 보자
봇: 🤔 어느 대화일까요? 1) 로그인 버그  2) 헬스체크 엔드포인트
```

### 명령어 (보조)

| 명령 | 설명 |
|------|------|
| `!project add <이름> <경로>` | 프로젝트 등록 (경로는 공백 포함 가능) |
| `!project remove <이름>` | 프로젝트 제거 |
| `!project list` | 프로젝트·대화 목록 (⭐=활성) |
| `!chat new [제목]` | 활성 프로젝트에 새 대화 |
| `!chat list` | 활성 프로젝트의 대화 목록 |
| `!chat use <id>` | 대화 수동 전환 |
| `!status` | 현재 활성 대화 및 실행 중 작업 |
| `!cancel` | 진행 중 작업 취소 |
| `!update` | 새 버전 빌드 & 자동 재시작 (launcher.ps1 필요) |
| `!help` | 도움말 |

> 먼저 `/project add`로 프로젝트를 1개 이상 등록해야 자연어 라우팅이 동작합니다.

> 📖 **자세한 설치·설정·트러블슈팅은 [docs/SETUP.md](docs/SETUP.md)** 를 보세요.

## 5. 동작 방식

```
[Telegram] → bot(auth, 단일 작업) → Manager(claude --json-schema 라우팅, structured_output)
   → 대화 저장소(store.json: 프로젝트→대화→세션UUID)
   → Worker(claude -p --output-format json --session-id/--resume, cwd=프로젝트)
   → 결과를 4096자 분할 회신
```

각 claude spawn은 **격리 실행**됩니다: `--strict-mcp-config`(글로벌 MCP 서버 무시 — serena 등 안 뜸) +
`--setting-sources project,local`(전역 설정/추가 디렉토리 누수 차단). OAuth 인증은 유지(`--bare` 미사용).

상태 파일: `%USERPROFILE%\.teleclaude\store.json`

## 6. 검증 상태 (2026-06-04, 실측)

라우팅 / Worker 실행+resume / **로컬 파일 생성** / 격리(MCP off, 인증 유지) / **Telegram 폴링 왕복(폰↔봇)** 모두 실측 통과.
단위·목 37개 + 통합 테스트(`go test -tags integration`) 통과. 자세한 표는 [docs/SETUP.md §11](docs/SETUP.md).

## 7. 한계 (MVP)

- 한 번에 한 작업만 처리(직렬화). 처리 중 새 메시지는 안내 후 무시 → `/cancel` 가능.
- `claude -p` 콜드스타트 지연(호출당 십수 초). `MANAGER_ALWAYS=false`로 완화 가능.
- Windows Service 상시화·Telegram 토픽 UX·로컬 머신 전반 제어는 후속 단계.
- 실시간 토큰 스트리밍 아님(작업 단위 회신). 진행 중에는 typing 표시.
