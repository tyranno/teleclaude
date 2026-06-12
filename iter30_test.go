package main

import (
	"strings"
	"testing"
)

// ---- parseRunResult ----

func TestParseRunResult_NormalOutput(t *testing.T) {
	json := `{"type":"result","result":"hello world","is_error":false,"session_id":"abc123"}`
	res, err := parseRunResult(json)
	if err != nil {
		t.Fatalf("parseRunResult: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q, want %q", res.Text, "hello world")
	}
	if res.IsError {
		t.Error("IsError should be false")
	}
}

func TestParseRunResult_ErrorOutput(t *testing.T) {
	json := `{"type":"result","result":"prompt is too long","is_error":true,"session_id":""}`
	res, err := parseRunResult(json)
	if err != nil {
		t.Fatalf("parseRunResult error: %v", err)
	}
	if !res.IsError {
		t.Error("IsError should be true")
	}
}

func TestParseRunResult_TrimsWhitespace(t *testing.T) {
	json := `{"result":"  trimmed  ","is_error":false}`
	res, err := parseRunResult(json)
	if err != nil {
		t.Fatalf("parseRunResult: %v", err)
	}
	if res.Text != "trimmed" {
		t.Errorf("Text = %q, want %q", res.Text, "trimmed")
	}
}

func TestParseRunResult_InvalidJSON(t *testing.T) {
	_, err := parseRunResult("not json")
	if err == nil {
		t.Error("invalid JSON should return error")
	}
}

// ---- unmarshalDecision ----

func TestUnmarshalDecision_DirectJSON(t *testing.T) {
	s := `{"action":"resume","project":"myapp","conversationId":"3"}`
	dec, ok := unmarshalDecision(s)
	if !ok {
		t.Fatal("unmarshalDecision should succeed")
	}
	if dec.Action != "resume" || dec.Project != "myapp" || dec.ConversationID != "3" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestUnmarshalDecision_JSONWithProse(t *testing.T) {
	// LLM may output text before the JSON block.
	s := `Here is my decision: {"action":"new","project":"voice","newTitle":"새 기능"}`
	dec, ok := unmarshalDecision(s)
	if !ok {
		t.Fatal("unmarshalDecision should extract JSON from prose")
	}
	if dec.Action != "new" || dec.Project != "voice" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestUnmarshalDecision_NoAction_Fails(t *testing.T) {
	// JSON without "action" field is not a valid RouteDecision.
	_, ok := unmarshalDecision(`{"project":"x"}`)
	if ok {
		t.Error("JSON without action should fail")
	}
}

func TestUnmarshalDecision_Empty_Fails(t *testing.T) {
	_, ok := unmarshalDecision("")
	if ok {
		t.Error("empty string should fail")
	}
}

// ---- parseRouteDecision ----

func TestParseRouteDecision_ViaResult(t *testing.T) {
	// Envelope with the decision in .result (no structured_output).
	env := `{"type":"result","result":"{\"action\":\"status\"}","is_error":false}`
	dec, err := parseRouteDecision(env)
	if err != nil {
		t.Fatalf("parseRouteDecision: %v", err)
	}
	if dec.Action != "status" {
		t.Errorf("action = %q, want status", dec.Action)
	}
}

func TestParseRouteDecision_ViaRawStdout(t *testing.T) {
	// Raw stdout fallback: no envelope wrapper at all.
	raw := `{"action":"clarify","clarify":"어느 프로젝트인지 알려주세요"}`
	dec, err := parseRouteDecision(raw)
	if err != nil {
		t.Fatalf("parseRouteDecision: %v", err)
	}
	if dec.Action != "clarify" {
		t.Errorf("action = %q, want clarify", dec.Action)
	}
}

func TestParseRouteDecision_NoValidJSON_Error(t *testing.T) {
	_, err := parseRouteDecision("no json here")
	if err == nil {
		t.Error("no JSON should return error")
	}
}

// ---- buildRoutePrompt ----

func TestBuildRoutePrompt_ContainsProject(t *testing.T) {
	req := RouteRequest{
		Message: "서버 확인해줘",
		Projects: []ProjectSummary{
			{Name: "myapp", Conversations: []ConversationSummary{{ID: "1", Title: "API 개발"}}},
		},
	}
	got := buildRoutePrompt(req)
	if !strings.Contains(got, "myapp") {
		t.Error("prompt should contain project name")
	}
	if !strings.Contains(got, "API 개발") {
		t.Error("prompt should contain conversation title")
	}
	if !strings.Contains(got, "서버 확인해줘") {
		t.Error("prompt should contain user message")
	}
}

func TestBuildRoutePrompt_NoProjects(t *testing.T) {
	req := RouteRequest{Message: "hi", Projects: nil}
	got := buildRoutePrompt(req)
	if !strings.Contains(got, "none yet") {
		t.Error("no-projects prompt should mention 'none yet'")
	}
}

func TestBuildRoutePrompt_ActiveRef(t *testing.T) {
	req := RouteRequest{
		Message: "계속해줘",
		Active:  ActiveRef{Project: "proj", ConversationID: "2"},
	}
	got := buildRoutePrompt(req)
	if !strings.Contains(got, "proj") || !strings.Contains(got, "\"2\"") {
		t.Errorf("prompt should show active project/conv, got: %s", got)
	}
}

func TestBuildRoutePrompt_ConversationWithSummary(t *testing.T) {
	req := RouteRequest{
		Message: "계속해줘",
		Projects: []ProjectSummary{{
			Name: "p",
			Conversations: []ConversationSummary{{ID: "1", Title: "작업", Summary: "로그인 버그 수정 중"}},
		}},
	}
	got := buildRoutePrompt(req)
	if !strings.Contains(got, "로그인 버그 수정 중") {
		t.Error("prompt should include conversation summary")
	}
}
