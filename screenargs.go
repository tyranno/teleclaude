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
		"You can control this Windows desktop via the `screen` MCP tools. Follow this priority:\n" +
		"1. UIA first (cheap): call `snapshot` to read the foreground window's UI Automation element " +
		"tree (names, control types, automation IDs), then `invoke(name)` to click and `set_value(name, text)` " +
		"to type into fields. Operate by element name whenever possible — this is reliable and uses few tokens.\n" +
		"2. Vision fallback (expensive): only when UIA can't find or operate the target, call `screenshot` to " +
		"see the screen, then `click(x, y)` / `type` / `key` / `scroll`. Screenshots cost many tokens, so use " +
		"them only when necessary.\n" +
		"3. Fixed positions: use `preset_save` / `preset_click` / `preset_list` for calibrated coordinates of " +
		"fixed layouts.\n" +
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
