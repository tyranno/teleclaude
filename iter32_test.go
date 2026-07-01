package main

import (
	"strings"
	"testing"
)

// ---- formatProgressEvent ----

func TestFormatProgressEvent_ToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`
	got := formatProgressEvent(line)
	want := "🔧 Bash: go test ./..."
	if got != want {
		t.Errorf("formatProgressEvent = %q, want %q", got, want)
	}
}

func TestFormatProgressEvent_ToolUse_UnrecognizedInput_FallsBackToName(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TaskCreate","input":{"subject":"x"}}]}}`
	got := formatProgressEvent(line)
	if got != "🔧 TaskCreate" {
		t.Errorf("formatProgressEvent = %q, want %q", got, "🔧 TaskCreate")
	}
}

func TestFormatProgressEvent_TextBlock_ReturnsEmpty(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`
	if got := formatProgressEvent(line); got != "" {
		t.Errorf("formatProgressEvent(text block) = %q, want empty", got)
	}
}

func TestFormatProgressEvent_NonAssistantLine_ReturnsEmpty(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`,
		`{"type":"result","subtype":"success","result":"done"}`,
		`not json at all`,
		``,
	} {
		if got := formatProgressEvent(line); got != "" {
			t.Errorf("formatProgressEvent(%q) = %q, want empty", line, got)
		}
	}
}

func TestFormatProgressEvent_TruncatesLongInput(t *testing.T) {
	longCmd := strings.Repeat("a", 200)
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + longCmd + `"}}]}}`
	got := formatProgressEvent(line)
	if len(got) > 100 {
		t.Errorf("formatProgressEvent output too long: %d chars", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("formatProgressEvent = %q, want truncation ellipsis", got)
	}
}

// ---- parseStreamResult ----

func TestParseStreamResult_FindsResultLine(t *testing.T) {
	stdout := `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"result","subtype":"success","result":"hi","is_error":false,"session_id":"abc"}
`
	res, err := parseStreamResult(stdout)
	if err != nil {
		t.Fatalf("parseStreamResult: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("Text = %q, want %q", res.Text, "hi")
	}
	if res.IsError {
		t.Error("IsError should be false")
	}
}

func TestParseStreamResult_ErrorResult(t *testing.T) {
	stdout := `{"type":"result","result":"prompt is too long","is_error":true}`
	res, err := parseStreamResult(stdout)
	if err != nil {
		t.Fatalf("parseStreamResult: %v", err)
	}
	if !res.IsError {
		t.Error("IsError should be true")
	}
}

func TestParseStreamResult_NoResultLine_Errors(t *testing.T) {
	stdout := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
`
	if _, err := parseStreamResult(stdout); err == nil {
		t.Error("expected error when no result line present")
	}
}

// ---- workerBaseArgs: stream-json wiring ----

func TestWorkerBaseArgs_OnProgress_UsesStreamJSON(t *testing.T) {
	req := RunRequest{Prompt: "hi", SessionID: "11111111-1111-1111-1111-111111111111", OnProgress: func(string) {}}
	args := workerBaseArgs(&Config{}, req, "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--output-format stream-json") {
		t.Errorf("expected stream-json output format, got: %v", args)
	}
	if !strings.Contains(joined, "--include-partial-messages") {
		t.Errorf("expected --include-partial-messages, got: %v", args)
	}
	if strings.Contains(joined, "--output-format json ") || strings.HasSuffix(joined, "--output-format json") {
		t.Errorf("should not also request json format, got: %v", args)
	}
}

func TestWorkerBaseArgs_NoOnProgress_UsesJSON(t *testing.T) {
	req := RunRequest{Prompt: "hi", SessionID: "11111111-1111-1111-1111-111111111111"}
	args := workerBaseArgs(&Config{}, req, "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--output-format json") {
		t.Errorf("expected json output format, got: %v", args)
	}
	if strings.Contains(joined, "stream-json") {
		t.Errorf("should not request stream-json, got: %v", args)
	}
}
