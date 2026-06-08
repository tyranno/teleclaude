package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
func (r *codexRunner) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	// Write route schema to a temp file (codex requires a file path, not inline JSON).
	sf, err := os.CreateTemp("", "teleclaude_route_schema_*.json")
	if err != nil {
		return RouteDecision{}, fmt.Errorf("codex route schema 임시 파일 생성 실패: %w", err)
	}
	schemaFile := sf.Name()
	sf.Close()
	defer os.Remove(schemaFile)
	if err := os.WriteFile(schemaFile, []byte(routeJSONSchema), 0600); err != nil {
		return RouteDecision{}, fmt.Errorf("codex route schema 쓰기 실패: %w", err)
	}

	of, err := os.CreateTemp("", "teleclaude_route_out_*.txt")
	if err != nil {
		return RouteDecision{}, fmt.Errorf("codex route 출력 임시 파일 생성 실패: %w", err)
	}
	outFile := of.Name()
	of.Close()
	defer os.Remove(outFile)

	prompt := buildRoutePrompt(req)
	args := []string{
		"exec",
		"--ignore-user-config",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"--ephemeral",
		"--output-schema", schemaFile,
		"--json",
		"-o", outFile,
		"-m", codexDefaultModel(r.cfg),
		prompt,
	}

	home, _ := os.UserHomeDir()
	_, stderr, err := r.exec(ctx, home, args)
	if err != nil {
		return RouteDecision{}, fmt.Errorf("codex manager 호출 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}

	content, rerr := os.ReadFile(outFile)
	if rerr != nil {
		return RouteDecision{}, fmt.Errorf("codex route 결과 파일 읽기 실패: %w", rerr)
	}
	return parseCodexRouteDecision(string(content))
}

// Run executes a worker turn via codex exec.
func (r *codexRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("teleclaude_codex_%d_%s.txt", os.Getpid(), req.SessionID))
	defer os.Remove(outFile)

	model := req.Model
	if model == "" {
		model = codexDefaultModel(r.cfg)
	}

	var args []string
	if req.Resume && req.SessionID != "" {
		args = []string{
			"exec", "resume", req.SessionID,
			"--dangerously-bypass-approvals-and-sandbox",
			"--ignore-user-config",
			"--skip-git-repo-check",
			"--json",
			"-o", outFile,
			"-m", model,
			req.Prompt,
		}
	} else {
		args = []string{
			"exec",
			"-C", req.WorkDir,
			"--dangerously-bypass-approvals-and-sandbox",
			"--ignore-user-config",
			"--skip-git-repo-check",
			"--json",
			"-o", outFile,
			"-m", model,
			req.Prompt,
		}
	}

	stdout, stderr, err := r.exec(ctx, req.WorkDir, args)
	if err != nil {
		if ctx.Err() != nil {
			return RunResult{}, ctx.Err()
		}
		// Read output file even on non-zero exit — codex may still produce output.
		if content, rerr := os.ReadFile(outFile); rerr == nil && len(content) > 0 {
			return RunResult{Text: parseCodexOutput(string(content))}, nil
		}
		return RunResult{}, fmt.Errorf("codex worker 실행 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}

	// Extract thread_id for new sessions so the store can persist it.
	// If empty, codex changed its JSONL event format — resume will fall back to UUID-based attempt.
	threadID := extractThreadID(stdout)
	if !req.Resume && threadID == "" {
		log.Printf("[codex] warning: thread_id not found in JSONL output; session resume may not work")
	}

	content, rerr := os.ReadFile(outFile)
	if rerr != nil {
		return RunResult{}, fmt.Errorf("codex 결과 파일 읽기 실패: %w", rerr)
	}

	result := RunResult{Text: parseCodexOutput(string(content))}
	if !req.Resume && threadID != "" {
		result.SessionID = threadID
	}
	return result, nil
}
