package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestScreenSystemPrompt(t *testing.T) {
	p := screenSystemPrompt()
	for _, kw := range []string{"snapshot", "UIA", "screenshot", "preset"} {
		if !strings.Contains(p, kw) {
			t.Errorf("screenSystemPrompt() missing keyword %q", kw)
		}
	}
}

func TestScreenWorkerArgs(t *testing.T) {
	const self = "C:\\t\\teleclaude.exe"
	args := screenWorkerArgs(self)

	for _, want := range []string{
		"--strict-mcp-config",
		"--mcp-config",
		"--allowedTools",
		"mcp__screen__*",
		"--append-system-prompt",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("screenWorkerArgs missing %q in %v", want, args)
		}
	}

	// Locate the inline JSON arg (the value after --mcp-config).
	idx := slices.Index(args, "--mcp-config")
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("--mcp-config has no value: %v", args)
	}
	inline := args[idx+1]

	if !strings.Contains(inline, "__mcp-screen") {
		t.Errorf("inline JSON missing __mcp-screen: %s", inline)
	}
	if !strings.Contains(inline, "screen") {
		t.Errorf("inline JSON missing server key screen: %s", inline)
	}
	// The exe path appears JSON-escaped (backslashes doubled) inside the inline
	// string, so check via encoding/json to mirror how it was produced.
	escaped, _ := json.Marshal(self)
	// strip surrounding quotes that Marshal adds around the string
	escapedInner := strings.Trim(string(escaped), `"`)
	if !strings.Contains(inline, escapedInner) {
		t.Errorf("inline JSON missing (escaped) exe path %q: %s", escapedInner, inline)
	}

	// Must parse back as valid JSON with the expected shape.
	var parsed struct {
		McpServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(inline), &parsed); err != nil {
		t.Fatalf("inline JSON does not parse: %v\n%s", err, inline)
	}
	srv, ok := parsed.McpServers["screen"]
	if !ok {
		t.Fatalf("parsed JSON has no screen server: %s", inline)
	}
	if srv.Command != self {
		t.Errorf("screen.command = %q, want %q", srv.Command, self)
	}
	if !slices.Contains(srv.Args, "__mcp-screen") {
		t.Errorf("screen.args missing __mcp-screen: %v", srv.Args)
	}
}
