package main

import (
	"slices"
	"strings"
	"testing"
)

// TestWorkerBaseArgs_ScreenControlInjection verifies that the worker arg builder
// injects the screen MCP args only when cfg.ScreenControl is enabled (and a self
// exe path is available). This exercises the config-gated injection without
// actually exec'ing claude.
func TestWorkerBaseArgs_ScreenControlInjection(t *testing.T) {
	const self = "C:\\t\\teleclaude.exe"
	req := RunRequest{Prompt: "hi", SessionID: "11111111-1111-1111-1111-111111111111"}

	// ScreenControl ON → screen MCP args present.
	on := workerBaseArgs(&Config{ScreenControl: true}, req, self)
	if !slices.Contains(on, "--mcp-config") {
		t.Errorf("ScreenControl=true: missing --mcp-config in %v", on)
	}
	if !slices.Contains(on, "mcp__screen__*") {
		t.Errorf("ScreenControl=true: missing allowedTools mcp__screen__* in %v", on)
	}
	if !slices.Contains(on, "--append-system-prompt") {
		t.Errorf("ScreenControl=true: missing --append-system-prompt in %v", on)
	}
	// The inline mcp-config must reference the self exe via __mcp-screen.
	joined := strings.Join(on, " ")
	if !strings.Contains(joined, "__mcp-screen") {
		t.Errorf("ScreenControl=true: inline config missing __mcp-screen in %v", on)
	}

	// ScreenControl OFF → no screen MCP args.
	off := workerBaseArgs(&Config{ScreenControl: false}, req, self)
	if slices.Contains(off, "--mcp-config") {
		t.Errorf("ScreenControl=false: unexpected --mcp-config in %v", off)
	}
	if slices.Contains(off, "mcp__screen__*") {
		t.Errorf("ScreenControl=false: unexpected mcp__screen__* in %v", off)
	}
	if strings.Contains(strings.Join(off, " "), "mcp__screen__") {
		t.Errorf("ScreenControl=false: unexpected mcp__screen__ token in %v", off)
	}

	// Even with ScreenControl on, an empty self exe path skips injection
	// (cannot point the worker at ourselves).
	noSelf := workerBaseArgs(&Config{ScreenControl: true}, req, "")
	if slices.Contains(noSelf, "--mcp-config") {
		t.Errorf("empty selfExe: unexpected --mcp-config in %v", noSelf)
	}

	// Base args are always present regardless of screen control.
	for _, base := range []string{"-p", "--output-format", "json", "--dangerously-skip-permissions"} {
		if !slices.Contains(off, base) {
			t.Errorf("base arg %q missing in %v", base, off)
		}
	}
}
