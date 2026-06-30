# M1 — Windows 화면제어 MCP (Go 네이티브) 설계

작성일: 2026-06-30 · 상태: 승인됨(brainstorming) · 다음: writing-plans

## 목표
teleclaude(Windows)가 **자체 Go 네이티브 화면제어 MCP 서버**를 내장하여, claude 워커가 텔레그램 명령으로 Windows GUI(예: NetGuard Lite 같은 네이티브 앱)를 **실행·인식·조작**할 수 있게 한다. 별도 파일/런타임/다운로드 없이 **teleclaude.exe 하나**로 동작.

핵심 전략: **UIA(요소 이름) 우선 → 비전(스크린샷+좌표) 폴백** (토큰 절약 + 네이티브 앱 신뢰성).

## 비목표 (Out of Scope → M2)
- 패킷 캡처 / "기능별 패킷 상관 리포트"
- 완전 자율 메뉴 스윕 오케스트레이션(도구는 제공, 본격 스윕 프롬프트는 M2)
- macOS/Linux 화면제어 (Windows 전용; 다른 OS는 screen_control 무시)

## 1. 구조

```
[Telegram] → teleclaude(Win) 워커 실행:
   claude -p "<task>" --strict-mcp-config --mcp-config <screen> 
          --allowedTools mcp__screen__* --dangerously-skip-permissions
          --append-system-prompt "<UIA 우선/비전 폴백 지침>"
        │
        ▼  (stdio MCP)
   teleclaude.exe __mcp-screen   ← 자기 자신을 숨은 서브커맨드로 기동(=MCP 서버)
        │  Win32 + UIA(go-ole)
        ▼
   [Windows 데스크톱 / NetGuard Lite 등]
```

- **config 구동**: `screen_control.enabled: true`(Phase 0 YAML)일 때만 워커가 위 MCP를 켬. 사용자 대면 서브커맨드 아님 — teleclaude가 자신을 `__mcp-screen` 인자로 spawn하여 stdio MCP 서버 역할. Phase 0의 `OnScreenControl` 훅이 enable/disable에 연결.
- **OS 가드**: Windows 빌드태그에서만 실제 구현. 비-Windows는 enabled여도 무시(로그).
- **MCP 프로토콜**: 순수 Go SDK `github.com/mark3labs/mcp-go` (CGO-free). **OCR/onnx 없음**(화면 판독은 claude 비전).
- **UIA**: `github.com/go-ole/go-ole` + IUIAutomation COM (CGO-free).

## 2. MCP 도구 (서버 키 `screen` → `mcp__screen__*`)

| 도구 | 입력 | 동작 | 우선순위 |
|------|------|------|:--------:|
| `snapshot` | (window?) | 포그라운드(또는 지정) 창의 **UIA 요소 트리**를 텍스트로 반환: name·controlType·자동화ID·클릭가능여부 (+필요시 bounding rect) | **1순위(싸다)** |
| `invoke` | name 또는 automationId | 이름으로 요소 찾아 **InvokePattern 클릭**(버튼/메뉴/트리노드/체크박스) | **1순위** |
| `set_value` | name, text | ValuePattern으로 텍스트 입력(입력 필드) | 1순위 |
| `screenshot` | (scale?) | 화면/활성창 캡처 → PNG(MCP 이미지 콘텐츠). claude 비전 폴백 | 2순위(비쌈) |
| `click` | x,y,button | 좌표 클릭(비전/프리셋 폴백) | 2순위 |
| `type` / `key` | text / combo | 키보드 입력/단축키(SendInput) | 폴백 |
| `scroll` | dx,dy | 스크롤 | 폴백 |
| `launch_app` | name | 시작메뉴(.lnk)/Program Files/바탕화면에서 **이름으로 찾아 실행** | — |
| `list_windows` / `focus_window` | (title?) | 창 목록 / 포커스 | — |
| `preset_save` / `preset_click` / `preset_list` | name(,x,y) | 고정 레이아웃용 **좌표 프리셋**(캘리브레이션) — `presets_file`(기본 ~/.teleclaude/presets.json) | — |

워커 system prompt 지침(요지): "먼저 `snapshot`으로 UIA 요소를 확인하고 `invoke`/`set_value`로 조작하라. UIA에 없거나 실패하면 그때만 `screenshot`으로 화면을 보고 `click(x,y)`. 고정 위치는 `preset_*` 사용. 스크린샷은 토큰이 크니 꼭 필요할 때만."

## 3. 데이터 흐름 (예: NetGuard Lite)
```
"netguardlite 실행해서 설정 열어줘"
 → launch_app("NetGuard Lite")  (시작메뉴 .lnk 검색→실행)
 → snapshot()                    (UIA 트리: 트리뷰/버튼/톱니 등)
 → invoke("설정"/gear automationId)  또는 UIA에 없으면 screenshot→click(x,y)
 → 결과/상태는 필요시 screenshot으로 claude가 색상·상태 판독
```

## 4. 구현 (Windows 빌드태그)
| 파일 | 책임 |
|------|------|
| `mcpscreen.go` (build windows) | MCP 서버(mcp-go), 도구 등록·디스패치, `__mcp-screen` 진입 |
| `screen_capture_windows.go` | BitBlt 캡처→PNG |
| `screen_input_windows.go` | SendInput 마우스/키보드/스크롤 |
| `screen_uia_windows.go` | go-ole IUIAutomation: snapshot/find/invoke/set_value |
| `screen_apps_windows.go` | launch_app(이름→경로 검색), list/focus windows |
| `screen_presets.go` | 프리셋 JSON CRUD (크로스플랫폼 OK) |
| `mcpscreen_stub.go` (build !windows) | 비-Windows no-op(서버 미기동) |
| `main.go` | `__mcp-screen` 인자 분기 → 서버 기동 |
| `runner.go` / manager | screen_control.enabled면 워커 args에 `--mcp-config <self __mcp-screen>` + allowedTools + append-system-prompt |
| `confighot.go` 훅 | OnScreenControl → (재)활성 로그/상태 |
| go.mod | `mark3labs/mcp-go`, `go-ole/go-ole` 추가 |

## 5. 안전
- `screen_control.enabled`가 명시적으로 true일 때만. 기본 false(NanoPi/서버 무영향).
- 워커는 이미 allowlist + `--dangerously-skip-permissions`(MCP 도구 자동허용). 화면조작은 위험하므로 **활성 시 텔레그램에 경고 1회** + 진행 스크린샷 공유 권장.
- Windows 전용; 비-Windows enabled는 무시(명확 로그).

## 6. 테스트
- 단위(크로스플랫폼): 프리셋 CRUD, launch_app 경로검색 로직(시작메뉴 파싱 모킹), MCP 도구 라우팅/스키마, system-prompt/args 조립.
- Windows 통합(수동/이 dev머신): snapshot가 네이티브 창 요소 반환, invoke 클릭 동작, screenshot 캡처, launch_app 실행, claude 워커가 도구 호출(스모크) — 메모장 등 네이티브 앱으로 검증.
- UIA가 안 잡히는 컨트롤 → screenshot+click 폴백 경로 확인.

## 7. 다음 (M2)
패킷 캡처(dumpcap/tshark 또는 자체) + 기능별 패킷 상관 + 자율 메뉴 스윕 리포트.

## 8. M1 구현 결과 (실측 반영, 2026-06-30)
설계의 UIA-우선 외에, 실제 타깃(NetGuard Lite = 커스텀 렌더 + 관리자 권한)을 다루며 다음이 추가/확정됨:

- **`win_controls` / `click_control`**: UIA가 비어도 표준 Win32 자식창(Button/SysTreeView32/SysListView32/Edit)이 있으면 `EnumChildWindows`+`GetWindowRect`로 **정확 좌표+라벨**을 얻어 라벨로 클릭. 비전 추정 불필요(저토큰·멀티모니터 무관). 우선순위: snapshot(UIA) → win_controls → screenshot.
- **`capture_window`**: 대상 창만 크롭 캡처(보통 비전 다운스케일 한계 미만이라 선명+픽셀정확). 전체 스크린샷은 다운스케일로 좌표 부정확.
- **`confirm_dialogs`**: 앱의 "전송하시겠습니까?" 등 확인창(연쇄 포함)을 예/확인 자동 클릭 → 무인 연속 스윕 가능.
- **`drag`, click `modifiers`(ctrl/shift)**: 러버밴드 다중선택·슬라이더·드래그드롭, 다중선택.
- **`launch_app(name, elevated)`**: 임의 앱 이름으로 실행(시작메뉴/Program Files/PATH 검색); elevated=true면 runas(UAC). 앱별 하드코딩 없음.
- **UIPI/권한 (`screen_control.elevated`)**: 관리자 권한 대상 앱은 일반 권한 클릭이 UIPI로 무음 차단됨(이동은 됨). teleclaude를 관리자로 실행해야 제어 가능 — `screen_control.elevated`면 시작 시 UAC로 자기 승격, 이후 launch한 앱도 권한 상속.
- **속도**: 조작 레이어 ms급(enumControls ~8ms, 클릭→변화감지 ~10ms). 병목은 LLM 비전 턴 → 변화감지는 win_controls(텍스트)로.
- **M2 패킷 캡처**: teleclaude에 내장하지 않음. 워커가 Bash로 외부 dumpcap/tshark를 실행하고 결과를 읽어 분석(teleclaude=오케스트레이터). end-to-end 실증 완료(예: "CH2 ON" → `0f0603620100 28ee` 전송 확인).
