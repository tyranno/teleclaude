package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Design Ref: §4.2 — claude CLI contract. Infrastructure impl of ClaudeClient.
// Refinement (Do phase, env check): Worker uses --output-format json (single robust envelope)
// + --session-id/--resume with a UUID we own; Manager uses --json-schema for structured routing.

type claudeRunner struct {
	claudePath string
	cfg        *Config
}

// NewClaudeRunner builds a ClaudeClient backed by the local claude CLI.
func NewClaudeRunner(claudePath string, cfg *Config) *claudeRunner {
	return &claudeRunner{claudePath: claudePath, cfg: cfg}
}

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

const routeJSONSchema = `{"type":"object","properties":{"project":{"type":"string"},"conversationId":{"type":"string"},"action":{"type":"string","enum":["resume","new","clarify"]},"newTitle":{"type":"string"},"clarify":{"type":"string"},"confidence":{"type":"number"}},"required":["action"]}`

// isolationArgs keep each spawned claude lightweight and isolated:
//   - --strict-mcp-config: ignore all global MCP servers (no serena/context7/figma/bkend boot)
//   - --setting-sources project,local: skip USER-global settings (additional dirs, plugins, output-style)
// OAuth/keychain auth is unaffected (unlike --bare). Big cold-start + noise reduction.
var isolationArgs = []string{"--strict-mcp-config", "--setting-sources", "project,local"}

// Route asks the Manager model to decide routing. Runs in a neutral cwd with no tools/permissions.
func (r *claudeRunner) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	prompt := buildRoutePrompt(req)
	args := []string{"-p", prompt, "--output-format", "json", "--json-schema", routeJSONSchema}
	args = append(args, isolationArgs...)
	if r.cfg.ManagerModel != "" {
		args = append(args, "--model", r.cfg.ManagerModel)
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

// Run executes a Worker turn in the project directory and returns the final text.
func (r *claudeRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	args := []string{"-p", req.Prompt, "--output-format", "json", "--dangerously-skip-permissions"}
	args = append(args, isolationArgs...)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Resume {
		args = append(args, "--resume", req.SessionID)
	} else {
		args = append(args, "--session-id", req.SessionID)
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
	// Kill the whole process tree on cancel (claude spawns node child processes on Windows).
	cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// killTree force-kills a process and its children on Windows.
func killTree(pid int) error {
	return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// --- Pure parsing helpers (unit-testable without claude) ---

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
