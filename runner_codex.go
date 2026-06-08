package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// codexRunner implements ClaudeClient backed by the local codex CLI.
// Codex JSONL sessions are identified by thread_id from the thread.started event.
type codexRunner struct {
	codexPath string
	cfg       *Config
}

// NewCodexRunner builds a ClaudeClient backed by the local codex CLI.
func NewCodexRunner(codexPath string, cfg *Config) *codexRunner {
	return &codexRunner{codexPath: codexPath, cfg: cfg}
}

// codexDefaultModel returns the configured model or the default "o4-mini".
func codexDefaultModel(cfg *Config) string {
	if cfg.CodexModel != "" {
		return cfg.CodexModel
	}
	return "o4-mini"
}

// extractThreadID scans JSONL lines for the thread_id from a thread.started event.
// Returns "" if not found.
func extractThreadID(jsonl string) string {
	for _, line := range strings.Split(jsonl, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev["type"] == "thread.started" {
			if tid, ok := ev["thread_id"].(string); ok && tid != "" {
				return tid
			}
		}
	}
	return ""
}

// parseCodexOutput trims whitespace from the -o file content.
func parseCodexOutput(content string) string {
	return strings.TrimSpace(content)
}

// parseCodexRouteDecision parses a RouteDecision from the codex output string.
func parseCodexRouteDecision(s string) (RouteDecision, error) {
	if dec, ok := unmarshalDecision(s); ok {
		return dec, nil
	}
	return RouteDecision{}, fmt.Errorf("codex 라우팅 JSON 파싱 실패: %q", s)
}

// exec runs codex with process-tree cancellation (Windows-aware).
func (r *codexRunner) exec(ctx context.Context, dir string, args []string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, r.codexPath, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// Route asks Codex to classify the user message and return a routing decision.
// Stub — full implementation in Task 4.
func (r *codexRunner) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	return RouteDecision{}, fmt.Errorf("codex Route: not yet implemented")
}

// Run executes a worker turn via codex exec.
// Stub — full implementation in Task 4.
func (r *codexRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	return RunResult{}, fmt.Errorf("codex Run: not yet implemented")
}
