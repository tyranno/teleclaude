package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codexRunner implements ClaudeClient backed by the local codex CLI.
// Codex JSONL sessions are identified by thread_id from the thread.started event.
type codexRunner struct {
	codexPath string
	cfgh      *ConfigHolder
}

func (r *codexRunner) cfg() *Config { return r.cfgh.Get() }

// resolveNativeCodex finds the platform-specific codex.exe given the npm .cmd wrapper path.
// npm installs codex.cmd at <root> and the native binary inside node_modules.
// Running the native exe directly avoids cmd.exe + node.exe stdin piping issues.
func resolveNativeCodex(cmdPath string) string {
	dir := filepath.Dir(cmdPath) // e.g. C:\Program Files\nodejs
	// Try both nested (npm global) and flat node_modules layouts.
	roots := []string{
		filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai"),
		filepath.Join(dir, "node_modules", "@openai"),
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "codex-") {
				continue
			}
			vendorDir := filepath.Join(root, e.Name(), "vendor")
			triples, _ := os.ReadDir(vendorDir)
			for _, triple := range triples {
				exe := filepath.Join(vendorDir, triple.Name(), "bin", "codex.exe")
				if _, err := os.Stat(exe); err == nil {
					return exe
				}
			}
		}
	}
	return ""
}

// NewCodexRunner builds a ClaudeClient backed by the local codex CLI.
// If given an npm .cmd wrapper, it resolves to the native codex.exe to avoid
// cmd.exe + node.exe chain issues with stdin and spaces-in-path.
func NewCodexRunner(codexPath string, cfgh *ConfigHolder) *codexRunner {
	ext := strings.ToLower(filepath.Ext(codexPath))
	if ext == ".cmd" || ext == ".bat" {
		if native := resolveNativeCodex(codexPath); native != "" {
			log.Printf("[codex] resolved native binary: %s", native)
			codexPath = native
		}
	}
	return &codexRunner{codexPath: codexPath, cfgh: cfgh}
}

// codexWorkerModel returns the model for actual work (Run). "" = codex built-in default.
func codexWorkerModel(cfg *Config) string {
	return cfg.CodexModel
}

// codexManagerModel returns the model for routing (Route). Falls back to worker model.
func codexManagerModel(cfg *Config) string {
	if cfg.CodexManagerModel != "" {
		return cfg.CodexManagerModel
	}
	return cfg.CodexModel
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

// logCodexEvent logs a single JSONL event from codex stdout in real-time.
func logCodexEvent(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	evType, _ := ev["type"].(string)
	switch evType {
	case "thread.started":
		log.Printf("[codex] ▶ session: %v", ev["thread_id"])
	case "turn.started":
		log.Printf("[codex] ▶ turn started")
	case "item.started":
		if item, ok := ev["item"].(map[string]any); ok {
			cmd, _ := item["command"].(string)
			if len(cmd) > 100 {
				cmd = cmd[:100] + "..."
			}
			log.Printf("[codex] ⚙ %s: %s", item["type"], cmd)
		}
	case "item.completed":
		if item, ok := ev["item"].(map[string]any); ok {
			log.Printf("[codex] ✓ %s (exit=%v)", item["type"], item["exit_code"])
		}
	case "turn.completed":
		if usage, ok := ev["usage"].(map[string]any); ok {
			log.Printf("[codex] ✅ turn done — in:%v cached:%v out:%v reasoning:%v",
				usage["input_tokens"], usage["cached_input_tokens"],
				usage["output_tokens"], usage["reasoning_output_tokens"])
		}
	}
}

// exec runs codex with real-time JSONL event logging and process-tree cancellation.
// stdinData, if non-empty, is piped to the process stdin (used for single-turn prompts).
// Passing prompt via stdin + EOF tells codex to process one turn then exit (avoids REPL loop).
func (r *codexRunner) exec(ctx context.Context, dir string, args []string, stdinData string) (stdout, stderr string, err error) {
	// On Windows, .cmd/.bat files need cmd.exe /C with special quoting.
	// Go's automatic arg escaping conflicts with cmd.exe /C quoting rules:
	//   cmd.exe /C "path with spaces" → strips first & last quote → unquoted path
	// Fix: use the double-outer-quote pattern and bypass Go quoting via SysProcAttr.CmdLine:
	//   cmd.exe /C ""path with spaces" arg1 "arg2 with spaces" ..."
	// cmd.exe strips outer quotes leaving the properly-quoted inner command.
	ext := strings.ToLower(filepath.Ext(r.codexPath))
	var cmd *exec.Cmd
	if ext == ".cmd" || ext == ".bat" {
		var sb strings.Builder
		sb.WriteString(`"`) // outer quote open
		sb.WriteString(`"`) // inner path quote open
		sb.WriteString(r.codexPath)
		sb.WriteString(`"`) // inner path quote close
		for _, a := range args {
			sb.WriteString(" ")
			if strings.ContainsAny(a, " \t") {
				sb.WriteString(`"`)
				sb.WriteString(a)
				sb.WriteString(`"`)
			} else {
				sb.WriteString(a)
			}
		}
		sb.WriteString(`"`) // outer quote close
		cmd = exec.CommandContext(ctx, "cmd.exe")
		applyCmdLine(cmd, "cmd.exe /C "+sb.String())
	} else {
		cmd = exec.CommandContext(ctx, r.codexPath, args...)
	}
	cmd.Dir = dir
	cmd.Cancel = func() error {
		log.Printf("[codex] ⚠ cancelling (PID %d) — killing process tree + all codex.exe", cmd.Process.Pid)
		killErr := killTree(cmd.Process.Pid)
		killByImageName("codex" + exeSuffix)
		return killErr
	}

	// Tee stdout: buffer for return value + pipe for real-time logging
	pr, pw := io.Pipe()
	var outBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(&outBuf, pw)

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	// Pipe prompt via stdin so codex processes one turn then exits on EOF.
	if stdinData != "" {
		cmd.Stdin = strings.NewReader(stdinData)
	}

	if startErr := cmd.Start(); startErr != nil {
		pr.Close()
		pw.Close()
		return "", "", startErr
	}

	// Show first few args (skip secrets/tokens)
	shown := args
	if len(shown) > 4 {
		shown = shown[:4]
	}
	log.Printf("[codex] started PID %d: %s %s", cmd.Process.Pid, r.codexPath, strings.Join(shown, " "))

	// Goroutine: drain pipe and log events as they arrive
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			logCodexEvent(scanner.Text())
		}
	}()

	err = cmd.Wait()
	pw.Close() // signal EOF to logging goroutine
	<-logDone  // wait for all events to be logged

	if stderrStr := strings.TrimSpace(errBuf.String()); stderrStr != "" {
		// Only log first 500 chars of stderr to avoid flooding
		if len(stderrStr) > 500 {
			stderrStr = stderrStr[:500] + "...(truncated)"
		}
		log.Printf("[codex] stderr: %s", stderrStr)
	}
	if err != nil {
		log.Printf("[codex] PID %d exited: %v", cmd.Process.Pid, err)
	}

	return outBuf.String(), errBuf.String(), err
}

// Route asks Codex to classify the user message and return a routing decision.
// Uses a cheap/fast model (codex_manager_model) and a 60s timeout.
// --sandbox read-only prevents command execution during text classification.
func (r *codexRunner) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	routeCtx, routeCancel := context.WithTimeout(ctx, 60*time.Second)
	defer routeCancel()

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
		"--sandbox", "read-only",
		"--json",
		"-o", outFile,
	}
	if m := codexManagerModel(r.cfg()); m != "" {
		args = append(args, "-m", m)
	}
	args = append(args, prompt)

	log.Printf("[codex] route: model=%q projects=%d", codexManagerModel(r.cfg()), len(req.Projects))
	home, _ := os.UserHomeDir()
	_, stderr, err := r.exec(routeCtx, home, args, "")
	if err != nil {
		return RouteDecision{}, fmt.Errorf("codex manager 호출 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}

	content, rerr := os.ReadFile(outFile)
	if rerr != nil {
		return RouteDecision{}, fmt.Errorf("codex route 결과 파일 읽기 실패: %w", rerr)
	}
	dec, perr := parseCodexRouteDecision(string(content))
	if perr != nil {
		log.Printf("[codex] route parse error — raw output: %q", string(content))
		return RouteDecision{}, perr
	}
	log.Printf("[codex] route: action=%s project=%q conv=%q", dec.Action, dec.Project, dec.ConversationID)
	return dec, nil
}

// Run executes a worker turn via codex exec.
// Uses a powerful model (codex_model) for actual work.
func (r *codexRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("teleclaude_codex_%d_%s.txt", os.Getpid(), req.SessionID))
	defer os.Remove(outFile)

	model := req.Model
	if model == "" {
		model = codexWorkerModel(r.cfg())
	}

	// Codex exec without --ephemeral enters a REPL loop and waits for more stdin after
	// each turn, causing "Reading additional input from stdin..." on EOF.
	// With --ephemeral, exec runs a single non-interactive turn and exits cleanly.
	// Conversation context is preserved via buildContextPrompt (history in every prompt).
	args := []string{
		"exec",
		"-C", req.WorkDir,
		"--ignore-user-config",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"--ephemeral",
		"--json",
		"-o", outFile,
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, req.Prompt)

	log.Printf("[codex] run: model=%q session=%s resume=%v dir=%s prompt=%d chars",
		model, req.SessionID, req.Resume, req.WorkDir, len(req.Prompt))

	stdout, stderr, err := r.exec(ctx, req.WorkDir, args, "")
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("[codex] run: context cancelled/timed out")
			return RunResult{}, ctx.Err()
		}
		// Read output file even on non-zero exit — codex may still produce output.
		if content, rerr := os.ReadFile(outFile); rerr == nil && len(content) > 0 {
			log.Printf("[codex] run: partial output on error (%d bytes)", len(content))
			return RunResult{Text: parseCodexOutput(string(content))}, nil
		}
		return RunResult{}, fmt.Errorf("codex worker 실행 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}

	// Extract thread_id for new sessions so the store can persist it.
	threadID := extractThreadID(stdout)
	if !req.Resume && threadID == "" {
		log.Printf("[codex] warning: thread_id not found in JSONL output; session resume may not work")
	}

	content, rerr := os.ReadFile(outFile)
	if rerr != nil {
		return RunResult{}, fmt.Errorf("codex 결과 파일 읽기 실패: %w", rerr)
	}

	text := parseCodexOutput(string(content))
	log.Printf("[codex] run done: output=%d bytes session=%s", len(text), threadID)

	result := RunResult{Text: text}
	if !req.Resume && threadID != "" {
		result.SessionID = threadID
	}
	return result, nil
}
