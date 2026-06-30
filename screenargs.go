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
		"You can control this Windows desktop via the `screen` MCP tools. Follow this learned workflow:\n" +
		"0. 앱을 띄울 때는 launch_app(name)으로 실행하고, 대상 앱을 조작하기 전에 먼저 focus_window로 창을 앞으로 가져와라.\n" +
		"1. 먼저 snapshot(UIA)로 요소를 확인하고 invoke/set_value(이름)로 조작하라. snapshot에 내부 컨트롤이 거의 없으면" +
		"(커스텀 렌더 앱) screenshot으로 화면을 보고 click(x,y) 또는 저장된 preset_click을 써라.\n" +
		"2. screenshot은 토큰이 크니 UIA로 안 되는 경우에만. screenshot으로 화면을 본 뒤 click(x,y) / type / key / scroll을 써라.\n" +
		"3. 고정 레이아웃은 preset_save로 좌표를 등록해두고 preset_click(또는 preset_list)으로 재사용하라.\n" +
		"Always prefer snapshot+invoke over screenshot+click."
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
