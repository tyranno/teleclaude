package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Design Ref: §4.2 — claude CLI contract. Infrastructure impl of ClaudeClient.
// Refinement (Do phase, env check): Worker uses --output-format json (single robust envelope)
// + --session-id/--resume with a UUID we own; Manager uses --json-schema for structured routing.

type claudeRunner struct {
	claudePath string
	cfgh       *ConfigHolder
}

// NewClaudeRunner builds a ClaudeClient backed by the local claude CLI.
func NewClaudeRunner(claudePath string, cfgh *ConfigHolder) *claudeRunner {
	return &claudeRunner{claudePath: claudePath, cfgh: cfgh}
}

func (r *claudeRunner) cfg() *Config { return r.cfgh.Get() }

// claudeEnvelope is the `claude -p --output-format json` result object (fields we use).
// With --json-schema, the validated object lands in StructuredOutput (NOT Result).
type claudeEnvelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	Result           string          `json:"result"`
	IsError          bool            `json:"is_error"`
	SessionID        string          `json:"session_id"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

const routeJSONSchema = `{"type":"object","properties":{"project":{"type":"string"},"conversationId":{"type":"string"},"action":{"type":"string","enum":["resume","new","clarify","status","schedule"]},"newTitle":{"type":"string"},"clarify":{"type":"string"},"confidence":{"type":"number"},"scheduleType":{"type":"string","enum":["remind","cron"]},"scheduleInterval":{"type":"string"},"scheduleTask":{"type":"string"},"scheduleIsTask":{"type":"boolean"}},"required":["action"]}`

// isolationArgs keep each spawned claude lightweight and isolated:
//   - --strict-mcp-config: ignore all global MCP servers (no serena/context7/figma/bkend boot)
//   - --setting-sources project,local: skip USER-global settings (additional dirs, plugins, output-style)
//
// OAuth/keychain auth is unaffected (unlike --bare). Big cold-start + noise reduction.
var isolationArgs = []string{"--strict-mcp-config", "--setting-sources", "project,local"}

// Route asks the Manager model to decide routing. Runs in a neutral cwd with no tools/permissions.
func (r *claudeRunner) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	prompt := buildRoutePrompt(req)
	args := []string{"-p", prompt, "--output-format", "json", "--json-schema", routeJSONSchema}
	args = append(args, isolationArgs...)
	if r.cfg().ManagerModel != "" {
		args = append(args, "--model", r.cfg().ManagerModel)
	}

	home, _ := os.UserHomeDir()
	stdout, stderr, err := r.exec(ctx, home, args)
	if err != nil {
		return RouteDecision{}, fmt.Errorf("manager 호출 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}
	dec, perr := parseRouteDecision(stdout)
	if perr != nil {
		return RouteDecision{}, perr
	}
	return dec, nil
}

// workerBaseArgs builds the claude CLI args for a Worker turn. It is a pure
// function (no exec, no os state beyond the supplied selfExe) so the screen-
// control injection is unit-testable. When cfg.ScreenControl is true, the
// teleclaude-owned screen MCP server args (screenWorkerArgs) are appended so the
// worker can drive the Windows desktop; when false they are omitted.
//
// selfExe is the teleclaude executable path (os.Executable()) that the screen
// MCP server is launched from via the hidden "__mcp-screen" subcommand. If it is
// empty the screen args are skipped (we cannot point the worker at ourselves).
func workerBaseArgs(cfg *Config, req RunRequest, selfExe string) []string {
	args := []string{"-p", req.Prompt}
	if req.OnProgress != nil {
		// Realtime NDJSON stream so tool-use activity can be relayed as it happens
		// (see execStream/formatProgressEvent), instead of one envelope at the end.
		args = append(args, "--output-format", "stream-json", "--include-partial-messages", "--verbose")
	} else {
		args = append(args, "--output-format", "json")
	}
	args = append(args, "--dangerously-skip-permissions")
	args = append(args, isolationArgs...)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Resume {
		args = append(args, "--resume", req.SessionID)
	} else {
		args = append(args, "--session-id", req.SessionID)
	}
	if cfg != nil && cfg.ScreenControl && selfExe != "" {
		args = append(args, screenWorkerArgs(selfExe)...)
	}
	return args
}

// Run executes a Worker turn in the project directory and returns the final text.
// If req.OnProgress is set, the turn streams NDJSON (--output-format stream-json)
// and OnProgress is called with a short human-readable line for each tool-use
// event as it happens, instead of waiting for the single end-of-turn envelope.
func (r *claudeRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	selfExe, _ := os.Executable()
	args := workerBaseArgs(r.cfg(), req, selfExe)

	if req.OnProgress != nil {
		stdout, stderr, err := r.execStream(ctx, req.WorkDir, args, func(line string) {
			if msg := formatProgressEvent(line); msg != "" {
				req.OnProgress(msg)
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return RunResult{}, ctx.Err()
			}
			if res, perr := parseStreamResult(stdout); perr == nil && res.Text != "" {
				return res, nil
			}
			return RunResult{}, fmt.Errorf("worker 실행 실패: %w (%s)", err, strings.TrimSpace(stderr))
		}
		return parseStreamResult(stdout)
	}

	stdout, stderr, err := r.exec(ctx, req.WorkDir, args)
	if err != nil {
		if ctx.Err() != nil {
			return RunResult{}, ctx.Err() // cancelled or timed out
		}
		// Even on non-zero exit, claude may emit a JSON envelope with the error text.
		if res, perr := parseRunResult(stdout); perr == nil && res.Text != "" {
			return res, nil
		}
		return RunResult{}, fmt.Errorf("worker 실행 실패: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return parseRunResult(stdout)
}

// exec runs the claude CLI with process-tree cancellation (Windows-aware).
func (r *claudeRunner) exec(ctx context.Context, dir string, args []string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, r.claudePath, args...)
	cmd.Dir = dir
	// Inject the OAuth token so headless services (systemd, etc.) authenticate without
	// any external env setup — `config.txt` is the single source of truth. Overrides a
	// stale/expired ~/.claude/.credentials.json. Empty = use claude's own login.
	if c := r.cfg(); c != nil && c.ClaudeOauthToken != "" {
		cmd.Env = append(os.Environ(), "CLAUDE_CODE_OAUTH_TOKEN="+c.ClaudeOauthToken)
	}
	// Kill the whole process tree on cancel (claude spawns node child processes on Windows).
	cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// execStream runs the claude CLI like exec, but invokes onLine for each line of
// stdout as it arrives (used for stream-json progress relay). Returns the full
// stdout once the process exits, so the caller can still fall back to parsing it
// (e.g. on non-zero exit).
func (r *claudeRunner) execStream(ctx context.Context, dir string, args []string, onLine func(line string)) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, r.claudePath, args...)
	cmd.Dir = dir
	if c := r.cfg(); c != nil && c.ClaudeOauthToken != "" {
		cmd.Env = append(os.Environ(), "CLAUDE_CODE_OAUTH_TOKEN="+c.ClaudeOauthToken)
	}
	cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }

	stdoutPipe, perr := cmd.StdoutPipe()
	if perr != nil {
		return "", "", perr
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if serr := cmd.Start(); serr != nil {
		return "", "", serr
	}

	var outBuf bytes.Buffer
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // tool output lines can be large
	for scanner.Scan() {
		line := scanner.Text()
		outBuf.WriteString(line)
		outBuf.WriteByte('\n')
		if onLine != nil {
			onLine(line)
		}
	}

	scanErr := scanner.Err()
	err = cmd.Wait()
	if err == nil && scanErr != nil {
		// A scan error (e.g. a line exceeding the buffer) would otherwise return
		// truncated stdout as success; surface it so the caller doesn't parse a
		// partial stream as if complete.
		err = fmt.Errorf("stream read error: %w", scanErr)
	}
	return outBuf.String(), errBuf.String(), err
}

// --- Pure parsing helpers (unit-testable without claude) ---

// parseStreamResult scans stream-json NDJSON output (one JSON object per line)
// for the terminal {"type":"result",...} line and decodes it the same way
// parseRunResult decodes the single-envelope ("json" format) output.
func parseStreamResult(stdout string) (RunResult, error) {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil || probe.Type != "result" {
			continue
		}
		var env claudeEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return RunResult{}, fmt.Errorf("claude stream-json 결과 파싱 실패: %w", err)
		}
		return RunResult{Text: strings.TrimSpace(env.Result), IsError: env.IsError}, nil
	}
	return RunResult{}, fmt.Errorf("claude stream-json: result 라인을 찾지 못함")
}

// streamContentBlock is the subset of a stream-json "assistant" message's
// content block fields we care about for progress relay.
type streamContentBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// formatProgressEvent extracts a short human-readable progress line from one
// stream-json NDJSON line, or "" if the line has nothing progress-worthy
// (partial text deltas, system/init, rate-limit events, user/tool-result lines,
// the final result envelope, etc. are all skipped — only completed tool_use
// blocks in assistant messages produce a line).
func formatProgressEvent(line string) string {
	var m struct {
		Type    string `json:"type"`
		Message *struct {
			Content []streamContentBlock `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &m) != nil || m.Type != "assistant" || m.Message == nil {
		return ""
	}
	for _, block := range m.Message.Content {
		if block.Type != "tool_use" {
			continue
		}
		if summary := toolUseSummary(block.Name, block.Input); summary != "" {
			return "🔧 " + block.Name + ": " + summary
		}
		return "🔧 " + block.Name
	}
	return ""
}

// toolUseSummary renders a short (<=80 char) argument preview for common tools.
// Returns "" for tools/shapes it doesn't recognize (caller falls back to the
// bare tool name).
func toolUseSummary(name string, input json.RawMessage) string {
	var probe map[string]any
	if json.Unmarshal(input, &probe) != nil {
		return ""
	}
	key := ""
	switch name {
	case "Bash":
		key = "command"
	case "Read", "Edit", "Write":
		key = "file_path"
	case "Glob", "Grep":
		key = "pattern"
	case "WebFetch":
		key = "url"
	case "WebSearch":
		key = "query"
	default:
		return ""
	}
	v, ok := probe[key].(string)
	if !ok || v == "" {
		return ""
	}
	v = strings.ReplaceAll(strings.ReplaceAll(v, "\n", " "), "\r", "")
	return truncate(v, 80) // rune-safe: never split a multibyte char (e.g. Korean paths)
}

// parseRunResult extracts the worker result text from a claude json envelope.
func parseRunResult(stdout string) (RunResult, error) {
	env, err := decodeEnvelope(stdout)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{Text: strings.TrimSpace(env.Result), IsError: env.IsError}, nil
}

// parseRouteDecision extracts a RouteDecision from the manager's json output.
// Order: (1) structured_output (--json-schema), (2) .result string, (3) raw stdout.
func parseRouteDecision(stdout string) (RouteDecision, error) {
	if env, err := decodeEnvelope(stdout); err == nil {
		// 1) --json-schema places the validated object here.
		if len(env.StructuredOutput) > 0 {
			var dec RouteDecision
			if json.Unmarshal(env.StructuredOutput, &dec) == nil && dec.Action != "" {
				return dec, nil
			}
		}
		// 2) Otherwise the decision may be in .result (possibly fenced/with prose).
		if env.Result != "" {
			if dec, ok := unmarshalDecision(env.Result); ok {
				return dec, nil
			}
		}
	}
	// 3) Last resort: find the first JSON object anywhere in stdout.
	if dec, ok := unmarshalDecision(stdout); ok {
		return dec, nil
	}
	return RouteDecision{}, fmt.Errorf("라우팅 JSON 파싱 실패")
}

func decodeEnvelope(stdout string) (claudeEnvelope, error) {
	var env claudeEnvelope
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		return claudeEnvelope{}, fmt.Errorf("claude json 파싱 실패: %w", err)
	}
	return env, nil
}

// unmarshalDecision tries to parse s (or the first {...} block within it) as a RouteDecision.
func unmarshalDecision(s string) (RouteDecision, bool) {
	var dec RouteDecision
	if err := json.Unmarshal([]byte(s), &dec); err == nil && dec.Action != "" {
		return dec, true
	}
	if obj := firstJSONObject(s); obj != "" {
		if err := json.Unmarshal([]byte(obj), &dec); err == nil && dec.Action != "" {
			return dec, true
		}
	}
	return RouteDecision{}, false
}

// firstJSONObject returns the first balanced {...} substring, or "".
func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// buildRoutePrompt renders the manager instruction + registry context + user message.
func buildRoutePrompt(req RouteRequest) string {
	var b strings.Builder
	b.WriteString("You are a routing assistant for a Telegram-to-Claude tool. ")
	b.WriteString("Decide which PROJECT and CONVERSATION a user message belongs to. Rules:\n")
	b.WriteString("- project MUST be one of the registered project names below (exact). If none fits or it's unclear, use action \"clarify\".\n")
	b.WriteString("- If the user is asking about the current task progress or status (e.g. \"진행 중이야?\", \"살아있어?\", \"얼마나 남았어?\", \"뭐하고 있어?\", \"아직 실행 중?\"), use action \"status\". No project or conversationId needed.\n")
	b.WriteString("- If the user wants to set a reminder or schedule a recurring task (e.g. \"30분 후에 알림\", \"1시간마다 서버 확인\", \"매일 배포 체크해줘\", \"2시간 후에 X 해줘\", \"매일 오전 7시 30분에 X\"), use action \"schedule\" with:\n")
	b.WriteString("  - scheduleType: \"remind\" for one-time delay, \"cron\" for recurring\n")
	b.WriteString("  - scheduleInterval: duration like \"30m\",\"2h\",\"daily\",\"weekly\", OR a 5-field cron expression (e.g. \"30 7 * * *\" for 07:30 daily, \"0 9 * * 1-5\" for weekdays 09:00, \"*/30 * * * *\" for every 30 min)\n")
	b.WriteString("  - scheduleTask: the message text or Claude prompt to execute\n")
	b.WriteString("  - scheduleIsTask: true only if user wants Claude to actively DO work (e.g. \"확인해줘\", \"분석해줘\"), false for simple notifications\n")
	b.WriteString("  - For specific clock times (e.g. \"오전 9시\", \"15:30\"), always output a 5-field cron expression in scheduleInterval.\n")
	b.WriteString("- If the message clearly continues an existing conversation, action \"resume\" with its conversationId.\n")
	b.WriteString("- If it's a new topic in a known project, action \"new\" with a short Korean newTitle.\n")
	b.WriteString("- If ambiguous (e.g. \"that thing again\" with multiple candidates), action \"clarify\" with a short Korean question listing options.\n")
	b.WriteString("- Output ONLY the JSON object. No prose.\n\n")

	if len(req.Projects) == 0 {
		b.WriteString("Registered projects: (none yet)\n")
	} else {
		b.WriteString("Registered projects and conversations:\n")
		for _, p := range req.Projects {
			b.WriteString("- project \"" + p.Name + "\":\n")
			if len(p.Conversations) == 0 {
				b.WriteString("    (no conversations yet)\n")
			}
			for _, c := range p.Conversations {
				line := "    [" + c.ID + "] " + c.Title
				if c.Summary != "" {
					line += " — " + c.Summary
				}
				b.WriteString(line + "\n")
			}
		}
	}
	if req.Active.Project != "" {
		b.WriteString("\nCurrently active: project \"" + req.Active.Project + "\", conversation \"" + req.Active.ConversationID + "\".\n")
	}
	b.WriteString("\nUser message:\n" + req.Message + "\n")
	return b.String()
}
