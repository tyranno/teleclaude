package main

import "encoding/json"

// Design Ref: §1 (structure), §2 (tool priority / worker system prompt).
//
// This file assembles the claude CLI args that load ONLY teleclaude's own screen
// MCP server (teleclaude re-invoked with the hidden "__mcp-screen" subcommand).
// Pure functions, no Win32 — testable on any platform.

// screenSystemPrompt returns the worker guidance: prefer the cheap UIA element
// tree (snapshot + invoke/set_value by name); fall back to the expensive
// screenshot + click(x,y) only when UIA can't find or do the thing; use preset_*
// for fixed positions.
func screenSystemPrompt() string {
	return "" +
		"You can control this Windows desktop via the `screen` MCP tools. Prefer cheap, coordinate-free methods; use vision only as a last resort.\n" +
		"0. 앱은 launch_app(name)으로 실행하고, 대상 앱을 조작하기 전에 먼저 focus_window로 창을 앞으로 가져와라.\n" +
		"1. (1순위) snapshot(UIA)로 요소를 확인하고 invoke/set_value(이름)로 조작하라 — 가장 싸고 정확하다.\n" +
		"2. (2순위) snapshot이 비어있거나 거의 없으면 win_controls(window)로 Win32 자식 컨트롤의 정확한 좌표를 얻어라. " +
		"버튼/트리/리스트가 라벨과 함께 center(x,y) 좌표로 나온다. 라벨로 누르려면 click_control(window, text[, nth]), " +
		"좌표로 누르려면 click(x,y)를 그 center 좌표로 호출하라. 이미지 추정이 아니라 OS가 준 정확한 좌표라 신뢰도가 높다.\n" +
		"3. (3순위, 최후) snapshot도 win_controls도 안 되는 커스텀 렌더 영역만 screenshot으로 화면을 보고 click(x,y)/type/key/scroll. " +
		"screenshot은 토큰이 크니 꼭 필요할 때만 쓴다.\n" +
		"4. 고정 좌표는 preset_save로 등록하고 preset_click/preset_list로 재사용하라.\n" +
		"5. 속도: 화면 변화 감지는 screenshot(느림) 대신 win_controls를 다시 호출해 보이는 컨트롤 집합의 변화로 판단하라(수 ms). " +
		"한 번의 답변에서 여러 클릭/감지를 묶어 처리해 LLM 왕복을 줄여라.\n" +
		"6. 대상 앱이 관리자 권한이면 일반 권한 클릭은 UIPI로 무시된다. click_control 결과에 UIPI 경고가 보이면 " +
		"screen_control.elevated를 켜고 teleclaude를 관리자로 실행해야 한다.\n" +
		"7. 명령 클릭 후 앱이 '전송하시겠습니까?' 같은 확인창을 띄우면, 사용자에게 묻지 말고 confirm_dialogs(app)로 " +
		"자동 확인하라(연쇄 확인창도 처리). 그래야 메뉴 전수 스윕이 사용자 개입 없이 연속 진행된다. 외부 패킷 캡처는 Bash로 " +
		"dumpcap/tshark를 실행하고 결과파일을 읽어 기능↔패킷을 상관시켜라.\n" +
		"8. list_windows에서 [other-desktop]로 표시된 창은 다른 가상 데스크톱에 있다. focus_window/capture_window/click_control로 " +
		"조작하면 그 데스크톱으로 자동 전환된다(안 그러면 캡처·클릭이 엉뚱한 데스크톱을 대상으로 함). 작업이 끝나면 return_desktop을 " +
		"호출해 사용자가 있던 원래 데스크톱으로 되돌려라.\n" +
		"Always prefer snapshot/invoke, then win_controls/click_control, then screenshot+click as the last resort."
}

// mcpServerSpec is one entry under mcpServers in an inline --mcp-config.
type mcpServerSpec struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// mcpConfig is the inline --mcp-config document shape.
type mcpConfig struct {
	McpServers map[string]mcpServerSpec `json:"mcpServers"`
}

// screenWorkerArgs returns the claude CLI args that load only teleclaude's own
// screen MCP server (selfExePath re-invoked with "__mcp-screen"), restrict the
// allowed tools to mcp__screen__*, and append the UIA-first system prompt.
//
// The --mcp-config value is built as inline JSON via encoding/json so that
// backslashes in Windows paths are escaped correctly (no string concatenation).
func screenWorkerArgs(selfExePath string) []string {
	cfg := mcpConfig{
		McpServers: map[string]mcpServerSpec{
			"screen": {
				Command: selfExePath,
				Args:    []string{"__mcp-screen"},
			},
		},
	}

	inline, err := json.Marshal(cfg)
	if err != nil {
		// cfg is a fixed, marshalable shape; this can't realistically fail.
		inline = []byte(`{"mcpServers":{}}`)
	}

	return []string{
		"--strict-mcp-config",
		"--mcp-config", string(inline),
		"--allowedTools", "mcp__screen__*",
		"--append-system-prompt", screenSystemPrompt(),
	}
}
