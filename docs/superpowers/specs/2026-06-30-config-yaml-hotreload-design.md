# Phase 0 — YAML 설정 + 마이그레이션 + Hot-Reload 설계

작성일: 2026-06-30 · 상태: 승인됨(brainstorming) · 다음: writing-plans

## 목표
teleclaude 설정을 `key=value` (`config.txt`) → **YAML(섹션 구조)** 로 전환하고,
- 기존 `config.txt`에서 **자동 마이그레이션**,
- 파일 변경을 **자동 인식(hot-reload)** 하여 재시작 없이 적용,
- 향후 화면제어(M1)를 위한 `screen_control` 섹션을 **자리만** 예약한다.

화면 인식·제어 기능 자체(M1)와 패킷 캡처(M2)는 **본 설계 범위 밖** — 이 Phase는 설정 시스템 기반만 다룬다.

## 비목표 (Out of Scope)
- 실제 화면 캡처/클릭/MCP 서버 동작 (M1)
- 패킷 캡처 (M2)
- bot_token 등 재시작 필요한 항목의 무중단 교체(=재시작 안내로 처리)

---

## 1. YAML 스키마 (`~/.teleclaude/config.yaml`)

```yaml
telegram:
  bot_token: "123456789:AAH..."
  allowed_user_ids: [6723802240]
  allowed_usernames: []          # @ 없이
models:
  manager: haiku
  worker: sonnet
  manager_always: false
claude:
  path: ""                       # 비우면 자동탐지
  oauth_token: ""                # CLAUDE_CODE_OAUTH_TOKEN (헤드리스 인증)
backend:
  default: claude                # claude | codex
  codex_path: ""
  codex_model: o4-mini
  codex_manager_model: ""
runtime:
  timeout_minutes: 10
  max_workers: 3
  rate_limit_per_min: 20
scripts:
  allow: false
  allowed_commands: []
screen_control:                  # M1에서 사용 (Phase0는 파싱/보존만)
  enabled: false
  presets_file: ""               # 비우면 ~/.teleclaude/presets.json
```

### 1.1 YAML → Config 매핑
기존 `Config` 구조체(types.go)는 **그대로 유지**하고, YAML을 중간 구조체(`yamlConfig`)로 파싱한 뒤 `Config`로 평탄화한다. (기존 검증 로직 `validate()` 재사용)

| YAML 경로 | Config 필드 |
|-----------|-------------|
| telegram.bot_token | TelegramBotToken |
| telegram.allowed_user_ids | AllowedUserIDs |
| telegram.allowed_usernames | AllowedUsernames |
| models.manager | ManagerModel |
| models.worker | WorkerModel |
| models.manager_always | ManagerAlways |
| claude.path | ClaudePath |
| claude.oauth_token | ClaudeOauthToken |
| backend.default | DefaultBackend |
| backend.codex_path/model/manager_model | CodexPath/CodexModel/CodexManagerModel |
| runtime.timeout_minutes/max_workers/rate_limit_per_min | TimeoutMinutes/MaxWorkers/RateLimitPerMin |
| scripts.allow/allowed_commands | AllowScripts/AllowedScriptCommands |
| screen_control.enabled/presets_file | ScreenControl(bool)/ScreenPresetsFile(string) ← 신규 필드 |

기본값은 현재와 동일(manager=haiku, timeout=10, max_workers=3, rate_limit=20, manager_always=true→**주의**: 현 코드 기본 true; YAML 미지정 시 동일 기본 적용).

---

## 2. 마이그레이션 (config.txt → config.yaml)

시작 시 로드 순서:
1. `config.yaml` 존재 → YAML 로드 (정상 경로)
2. `config.yaml` 없음 + `config.txt` 존재 → **마이그레이션**:
   - 기존 key=value 파서로 `config.txt` 읽어 `Config` 생성
   - `Config` → YAML 직렬화 → `config.yaml` 저장
   - `config.txt` → `config.txt.bak`로 이름 변경(보존)
   - 로그: "config.txt → config.yaml 마이그레이션 완료"
3. 둘 다 없음 → 설정 마법사(setup) 안내(기존 동작)

설정 마법사(`writeConfigFile`)는 이제 **YAML로 출력**한다.

> 정책: yaml과 txt가 모두 있으면 **yaml 우선**(txt는 무시, 안내 로그).

---

## 3. Hot-Reload (변경 자동 인식)

### 3.1 감시 방식
- **fsnotify**(이벤트 기반, 비동기 — Linux inotify / Windows ReadDirectoryChangesW, CGO 없음). 폴링 대신 OS 알림으로 즉시 반응.
- **디렉토리 감시**: `config.yaml` 파일이 아니라 상위 디렉토리(`~/.teleclaude/`)를 watch하고 `config.yaml` 관련 이벤트(Write/Create/Rename)만 필터. (에디터의 atomic save = 임시파일→rename 패턴에서도 감시가 끊기지 않음)
- **디바운스**: 저장 1회에 이벤트가 여러 번 와도 ~300ms 디바운스 후 1회만 reload.
- 변경 감지 → 재파싱 → `validate()` → 성공 시 적용, 실패 시 **이전 설정 유지 + 텔레그램 경고**("⚠️ 설정 reload 실패: <이유> — 이전 설정 유지").
- watcher 초기화 실패 시 → 로그 경고 후 hot-reload 없이 정상 구동(설정은 시작 시 1회 로드된 값 사용).

### 3.2 동시성 안전
- 전역 설정 접근을 **`atomic.Pointer[Config]`** 로 감싼다(`configHolder.Load()`).
- Bot/Manager/runner는 시작 시 고정 `*Config` 대신 **holder에서 매 사용 시 Load** (또는 turn 시작 시 스냅샷).
- 워커 실행 중 변경돼도 진행 중 작업은 스냅샷 유지, 다음 작업부터 신설정.

### 3.3 적용 정책
| 항목 | 적용 |
|------|------|
| models.*, manager_always, runtime.timeout/max_workers/rate_limit, scripts.*, claude.oauth_token/path, backend.*, screen_control.* | **즉시 적용**(다음 워커/라우팅부터) |
| telegram.bot_token | **재시작 필요** → 변경 감지 시 "토큰 변경은 재시작 필요" 안내(자동 재시작 안 함) |
| telegram.allowed_user_ids/usernames | 즉시 적용 |

reload 성공 시: 로그 + (옵션) 텔레그램 "⚙️ 설정이 reload되었습니다" 1회 알림.

### 3.4 screen_control 훅(자리만)
Phase0에서는 `screen_control.enabled` 변경을 감지해 **콜백 지점만** 마련(로그). 실제 "화면 MCP 자동 기동/중지"는 **M1에서 이 훅에 연결**한다.

---

## 4. 영향 컴포넌트
| 파일 | 변경 |
|------|------|
| types.go | Config에 `ScreenControl bool`, `ScreenPresetsFile string` 추가 |
| config.go | `LoadConfig`를 YAML 로드 + 마이그레이션으로 재작성, 기존 key=value 파서는 마이그레이션용으로 유지 |
| config_yaml.go (신규) | yamlConfig 구조체, YAML↔Config 변환, YAML 직렬화 |
| confighot.go (신규) | fsnotify watcher(디렉토리 감시+디바운스) + atomic.Pointer[Config] holder + 적용 콜백 |
| setup.go | `writeConfigFile` → YAML 출력 |
| main.go | holder 초기화, watcher 기동, 컴포넌트에 holder 전달 |
| bot.go / manager.go / runner.go | `cfg *Config` → holder 스냅샷 사용(읽기 지점 조정) |
| go.mod | `gopkg.in/yaml.v3`, `github.com/fsnotify/fsnotify` 추가 |

> 호환: 기존 `config.txt` 파서/테스트는 마이그레이션 경로로 살려둔다(삭제하지 않음).

---

## 5. 테스트
- YAML 파싱: 전체 섹션 → Config 매핑·기본값
- 마이그레이션: 샘플 config.txt → config.yaml 생성·필드 일치·.bak 생성
- yaml/txt 공존 시 yaml 우선
- hot-reload: 파일 변경 → holder 갱신, validate 실패 시 이전 설정 유지
- 동시성: holder Load/Store 레이스(go test -race)
- setup writeConfigFile → YAML 유효(LoadConfig 가능)

---

## 6. 다음 단계
- **M1**: `screen_control` 섹션 활성화 시 화면제어 MCP(스크린샷/클릭/입력/프리셋) 자동 기동, 텔레그램으로 netguardlite 조작.
- **M2**: 자율 메뉴 스윕 + 패킷 캡처(기능별 패킷 점검 리포트).
