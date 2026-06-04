package main

import (
	"strings"
	"testing"
)

func TestParseRunResult(t *testing.T) {
	out := `{"type":"result","subtype":"success","result":"작업 완료\n파일 생성됨","is_error":false,"session_id":"abc-123"}`
	res, err := parseRunResult(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "작업 완료\n파일 생성됨" {
		t.Errorf("text = %q", res.Text)
	}
	if res.IsError {
		t.Error("should not be error")
	}
}

func TestParseRunResult_Error(t *testing.T) {
	out := `{"type":"result","result":"권한 거부","is_error":true}`
	res, err := parseRunResult(out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Text != "권한 거부" {
		t.Errorf("got %+v", res)
	}
}

func TestParseRouteDecision_FromEnvelopeResult(t *testing.T) {
	// Manager output: envelope whose .result holds the routing JSON as a string.
	out := `{"type":"result","result":"{\"action\":\"resume\",\"project\":\"myapp\",\"conversationId\":\"1\",\"confidence\":0.9}"}`
	dec, err := parseRouteDecision(out)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionResume || dec.Project != "myapp" || dec.ConversationID != "1" {
		t.Errorf("decision = %+v", dec)
	}
}

func TestParseRouteDecision_NewWithProse(t *testing.T) {
	// result contains some prose around the JSON object — firstJSONObject must extract it.
	out := `{"type":"result","result":"Here you go: {\"action\":\"new\",\"project\":\"voicesvr\",\"newTitle\":\"헬스체크\"} done"}`
	dec, err := parseRouteDecision(out)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionNew || dec.Project != "voicesvr" || dec.NewTitle != "헬스체크" {
		t.Errorf("decision = %+v", dec)
	}
}

func TestParseRouteDecision_StructuredOutput(t *testing.T) {
	// Real claude --json-schema behavior: decision is in structured_output, not result.
	out := `{"type":"result","result":"Done. Routing submitted.","structured_output":{"action":"resume","project":"myapp","confidence":0.95}}`
	dec, err := parseRouteDecision(out)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionResume || dec.Project != "myapp" {
		t.Errorf("decision = %+v", dec)
	}
}

func TestParseRouteDecision_StructuredOutputPreferredOverResult(t *testing.T) {
	// structured_output must win even if result also contains a (stale) object.
	out := `{"type":"result","result":"{\"action\":\"new\",\"project\":\"other\"}","structured_output":{"action":"clarify","clarify":"which?"}}`
	dec, err := parseRouteDecision(out)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionClarify {
		t.Errorf("expected structured_output to win, got %+v", dec)
	}
}

func TestParseRouteDecision_RawObject(t *testing.T) {
	// Fallback: routing JSON appears directly (no envelope).
	out := `{"action":"clarify","clarify":"어느 대화?"}`
	dec, err := parseRouteDecision(out)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionClarify || dec.Clarify != "어느 대화?" {
		t.Errorf("decision = %+v", dec)
	}
}

func TestParseRouteDecision_Garbage(t *testing.T) {
	if _, err := parseRouteDecision("not json at all"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestFirstJSONObject(t *testing.T) {
	cases := map[string]string{
		`pre {"a":{"b":1}} post`: `{"a":{"b":1}}`,
		`{"x":1}`:                `{"x":1}`,
		`no object here`:         ``,
	}
	for in, want := range cases {
		if got := firstJSONObject(in); got != want {
			t.Errorf("firstJSONObject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRoutePrompt_IncludesContext(t *testing.T) {
	req := RouteRequest{
		Message: "로그인 문제 보자",
		Projects: []ProjectSummary{
			{Name: "myapp", Conversations: []ConversationSummary{{ID: "1", Title: "로그인 버그", Summary: "세션 만료"}}},
		},
		Active: ActiveRef{Project: "myapp", ConversationID: "1"},
	}
	p := buildRoutePrompt(req)
	for _, want := range []string{"myapp", "로그인 버그", "세션 만료", "로그인 문제 보자", "clarify"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildRoutePrompt_Empty(t *testing.T) {
	p := buildRoutePrompt(RouteRequest{Message: "hi"})
	if !strings.Contains(p, "none yet") {
		t.Errorf("expected 'none yet' for empty registry, got:\n%s", p)
	}
}
