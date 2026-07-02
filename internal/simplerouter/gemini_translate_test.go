package simplerouter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func mustAnthropicRequest(t *testing.T, body string) *anthropicRequest {
	t.Helper()
	var req anthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return &req
}

func TestAnthropicToGeminiSystemAndContent(t *testing.T) {
	req := mustAnthropicRequest(t, `{
		"model": "gemini-2.5-flash",
		"max_tokens": 100,
		"system": [
			{"type": "text", "text": "You are Claude Code.", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "Be terse."}
		],
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [{"type": "text", "text": "hi"}]},
			{"role": "user", "content": [
				{"type": "text", "text": "look at this"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}}
			]}
		]
	}`)
	out, err := anthropicToGemini(req, newSignatureStore())
	if err != nil {
		t.Fatal(err)
	}
	if got := out.SystemInstruction.Parts[0].Text; got != "You are Claude Code.\n\nBe terse." {
		t.Errorf("systemInstruction = %q", got)
	}
	if len(out.Contents) != 3 {
		t.Fatalf("contents len = %d", len(out.Contents))
	}
	if out.Contents[0].Role != "user" || out.Contents[0].Parts[0].Text != "hello" {
		t.Errorf("content[0] = %+v", out.Contents[0])
	}
	if out.Contents[1].Role != "model" {
		t.Errorf("assistant should map to model, got %q", out.Contents[1].Role)
	}
	last := out.Contents[2].Parts
	if len(last) != 2 || last[1].InlineData == nil || last[1].InlineData.MimeType != "image/png" {
		t.Errorf("image block not translated: %+v", last)
	}
	if out.GenerationConfig.MaxOutputTokens != 100 {
		t.Errorf("maxOutputTokens = %d", out.GenerationConfig.MaxOutputTokens)
	}
	if out.GenerationConfig.ThinkingConfig != nil {
		t.Error("thinkingConfig should be omitted when thinking absent")
	}
}

func TestAnthropicToGeminiSystemString(t *testing.T) {
	req := mustAnthropicRequest(t, `{"model":"m","max_tokens":1,"system":"be nice","messages":[{"role":"user","content":"x"}]}`)
	out, _ := anthropicToGemini(req, newSignatureStore())
	if out.SystemInstruction == nil || out.SystemInstruction.Parts[0].Text != "be nice" {
		t.Errorf("system string not translated: %+v", out.SystemInstruction)
	}
}

func TestAnthropicToGeminiMergesConsecutiveRoles(t *testing.T) {
	req := mustAnthropicRequest(t, `{"model":"m","max_tokens":1,"messages":[
		{"role": "user", "content": "one"},
		{"role": "user", "content": "two"},
		{"role": "assistant", "content": [{"type":"text","text":""}]},
		{"role": "user", "content": "three"}
	]}`)
	out, _ := anthropicToGemini(req, newSignatureStore())
	// Empty assistant turn drops, so all three user turns merge into one.
	if len(out.Contents) != 1 {
		t.Fatalf("contents = %+v, want single merged user turn", out.Contents)
	}
	if len(out.Contents[0].Parts) != 3 {
		t.Errorf("merged parts = %+v", out.Contents[0].Parts)
	}
}

func TestAnthropicToGeminiThinkingBlocks(t *testing.T) {
	valid := "aGVsbG8=" // valid base64
	req := mustAnthropicRequest(t, fmt.Sprintf(`{"model":"m","max_tokens":1,"messages":[
		{"role": "assistant", "content": [
			{"type": "thinking", "thinking": "signed", "signature": %q},
			{"type": "thinking", "thinking": "unsigned"},
			{"type": "redacted_thinking", "data": "xxx"},
			{"type": "thinking", "thinking": "foreign", "signature": "claude#abc"},
			{"type": "text", "text": "answer"}
		]}
	]}`, valid))
	out, _ := anthropicToGemini(req, newSignatureStore())
	parts := out.Contents[0].Parts
	if len(parts) != 3 {
		t.Fatalf("parts = %+v, want signed thinking + sanitized thinking + text", parts)
	}
	if !parts[0].Thought || parts[0].ThoughtSignature != valid || parts[0].Text != "signed" {
		t.Errorf("signed thinking part = %+v", parts[0])
	}
	if parts[1].ThoughtSignature != geminiDummySignature {
		t.Errorf("foreign signature should be replaced with dummy, got %q", parts[1].ThoughtSignature)
	}
	if parts[2].Text != "answer" {
		t.Errorf("text part = %+v", parts[2])
	}
}

func TestAnthropicToGeminiToolUseSignatureMatrix(t *testing.T) {
	sigs := newSignatureStore()
	sigs.record("toolu_real", "get_weather", "c2ln")
	sigs.record("toolu_batch2", "get_time", "") // non-first parallel batch member

	req := mustAnthropicRequest(t, `{"model":"m","max_tokens":1,"messages":[
		{"role": "assistant", "content": [
			{"type": "tool_use", "id": "toolu_real", "name": "get_weather", "input": {"city": "London"}},
			{"type": "tool_use", "id": "toolu_batch2", "name": "get_time", "input": {}},
			{"type": "tool_use", "id": "toolu_unknown", "name": "get_news"}
		]}
	]}`)
	out, _ := anthropicToGemini(req, sigs)
	parts := out.Contents[0].Parts
	if parts[0].ThoughtSignature != "c2ln" {
		t.Errorf("stored signature should echo exactly, got %q", parts[0].ThoughtSignature)
	}
	if parts[1].ThoughtSignature != "" {
		t.Errorf("empty stored signature must stay absent, got %q", parts[1].ThoughtSignature)
	}
	if parts[2].ThoughtSignature != geminiDummySignature {
		t.Errorf("store miss should use dummy, got %q", parts[2].ThoughtSignature)
	}
	// Assert the empty signature is truly absent from the wire format.
	wire, _ := json.Marshal(out.Contents[0])
	var decoded map[string]any
	json.Unmarshal(wire, &decoded)
	batchPart := decoded["parts"].([]any)[1].(map[string]any)
	if _, present := batchPart["thoughtSignature"]; present {
		t.Errorf("thoughtSignature key must be omitted for batch member: %s", wire)
	}
	if parts[2].FunctionCall == nil || string(parts[2].FunctionCall.Args) != `{}` {
		t.Errorf("missing input should default to {}: %+v", parts[2].FunctionCall)
	}
}

func TestAnthropicToGeminiToolResults(t *testing.T) {
	sigs := newSignatureStore()
	sigs.record("toolu_1", "read_file", "sig1")

	cases := []struct {
		name     string
		content  string
		isError  bool
		wantResp string
	}{
		{"string content", `"content": "file text"`, false, `{"result":"file text"}`},
		{"object content", `"content": "{\"temp\": 22}"`, false, `{"temp": 22}`},
		{"error content", `"content": "boom", "is_error": true`, true, `{"error":"boom"}`},
		{"block content", `"content": [{"type":"text","text":"a"},{"type":"text","text":"b"}]`, false, `{"result":"a\nb"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mustAnthropicRequest(t, fmt.Sprintf(`{"model":"m","max_tokens":1,"messages":[
				{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", %s}]}
			]}`, tc.content))
			out, _ := anthropicToGemini(req, sigs)
			part := out.Contents[0].Parts[0]
			if out.Contents[0].Role != "user" {
				t.Errorf("tool_result must land in user role, got %q", out.Contents[0].Role)
			}
			if part.FunctionResponse == nil || part.FunctionResponse.Name != "read_file" {
				t.Fatalf("functionResponse = %+v", part.FunctionResponse)
			}
			if string(part.FunctionResponse.Response) != tc.wantResp {
				t.Errorf("response = %s, want %s", part.FunctionResponse.Response, tc.wantResp)
			}
		})
	}
}

func TestAnthropicToGeminiToolResultImagesAndUnknownID(t *testing.T) {
	req := mustAnthropicRequest(t, `{"model":"m","max_tokens":1,"messages":[
		{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_gone", "content": [
			{"type":"text","text":"screenshot"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"}}
		]}]}
	]}`)
	out, _ := anthropicToGemini(req, newSignatureStore())
	parts := out.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want functionResponse + inlineData", parts)
	}
	if parts[0].FunctionResponse.Name != "toolu_gone" {
		t.Errorf("unknown id should fall back to id-as-name, got %q", parts[0].FunctionResponse.Name)
	}
	if parts[1].InlineData == nil || parts[1].InlineData.Data != "aW1n" {
		t.Errorf("image should become sibling inlineData: %+v", parts[1])
	}
}

func TestAnthropicToGeminiToolsAndChoice(t *testing.T) {
	req := mustAnthropicRequest(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"tools": [
			{"name": "get_weather", "description": "d", "input_schema": {"type":"object","properties":{"city":{"type":"string"}}}},
			{"name": "web_search", "type": "web_search_20250305"},
			{"name": "no_args", "input_schema": {"type":"object","properties":{}}}
		],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`)
	out, _ := anthropicToGemini(req, newSignatureStore())
	if len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 2 {
		t.Fatalf("tools = %+v, want 2 declarations (server tool skipped)", out.Tools)
	}
	decls := out.Tools[0].FunctionDeclarations
	if decls[0].Name != "get_weather" || decls[0].Parameters == nil {
		t.Errorf("decl[0] = %+v", decls[0])
	}
	if decls[1].Name != "no_args" || decls[1].Parameters != nil {
		t.Errorf("empty schema should omit parameters: %+v", decls[1])
	}
	fcc := out.ToolConfig.FunctionCallingConfig
	if fcc.Mode != "ANY" || len(fcc.AllowedFunctionNames) != 1 || fcc.AllowedFunctionNames[0] != "get_weather" {
		t.Errorf("toolConfig = %+v", fcc)
	}

	for choice, wantMode := range map[string]string{"auto": "AUTO", "any": "ANY", "none": "NONE"} {
		tc := convertToolChoice(&anthropicToolChoice{Type: choice})
		if tc.FunctionCallingConfig.Mode != wantMode {
			t.Errorf("tool_choice %q -> %q, want %q", choice, tc.FunctionCallingConfig.Mode, wantMode)
		}
	}
}

func TestAnthropicToGeminiThinkingConfig(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		thinking   string
		wantBudget int // -1 = nil
		wantLevel  string
	}{
		{"2.5 budget", "gemini-2.5-flash", `{"type":"enabled","budget_tokens":2048}`, 2048, ""},
		{"gemini-3 low", "gemini-3-pro-preview", `{"type":"enabled","budget_tokens":2048}`, -1, "low"},
		{"gemini-3 high", "gemini-3.5-flash", `{"type":"enabled","budget_tokens":16000}`, -1, "high"},
		{"disabled", "gemini-2.5-pro", `{"type":"disabled"}`, -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mustAnthropicRequest(t, fmt.Sprintf(
				`{"model":%q,"max_tokens":1,"thinking":%s,"messages":[{"role":"user","content":"x"}]}`, tc.model, tc.thinking))
			out, _ := anthropicToGemini(req, newSignatureStore())
			cfg := out.GenerationConfig.ThinkingConfig
			if tc.wantBudget == -1 && tc.wantLevel == "" {
				if cfg != nil {
					t.Fatalf("thinkingConfig = %+v, want nil", cfg)
				}
				return
			}
			if cfg == nil || !cfg.IncludeThoughts {
				t.Fatalf("thinkingConfig = %+v, want includeThoughts", cfg)
			}
			if cfg.ThinkingBudget != nil && cfg.ThinkingLevel != "" {
				t.Error("must never set both thinkingBudget and thinkingLevel")
			}
			if tc.wantLevel != "" && cfg.ThinkingLevel != tc.wantLevel {
				t.Errorf("level = %q, want %q", cfg.ThinkingLevel, tc.wantLevel)
			}
			if tc.wantBudget >= 0 && (cfg.ThinkingBudget == nil || *cfg.ThinkingBudget != tc.wantBudget) {
				t.Errorf("budget = %v, want %d", cfg.ThinkingBudget, tc.wantBudget)
			}
		})
	}
}

func TestSanitizeSignature(t *testing.T) {
	if got := sanitizeSignature("aGVsbG8="); got != "aGVsbG8=" {
		t.Errorf("valid base64 should pass through, got %q", got)
	}
	for _, bad := range []string{"claude#RXFn", "not base64!!!", "a"} {
		if got := sanitizeSignature(bad); got != geminiDummySignature {
			t.Errorf("sanitizeSignature(%q) = %q, want dummy", bad, got)
		}
	}
}

func TestGeminiToAnthropicResponse(t *testing.T) {
	sigs := newSignatureStore()
	ids := []string{"toolu_a", "toolu_b"}
	i := 0
	newID := func() string { id := ids[i]; i++; return id }

	resp := &geminiResponse{
		Candidates: []geminiCandidate{{
			FinishReason: "STOP",
			Content: geminiContent{Role: "model", Parts: []geminiPart{
				{Text: "let me think", Thought: true, ThoughtSignature: "dGhpbmtzaWc="},
				{Text: "calling tools"},
				{FunctionCall: &geminiFuncCall{Name: "get_weather", Args: json.RawMessage(`{"city":"London"}`)}, ThoughtSignature: "Y2FsbHNpZw=="},
				{FunctionCall: &geminiFuncCall{Name: "get_time", Args: json.RawMessage(`{}`)}}, // parallel: no signature
			}},
		}},
		UsageMetadata: &geminiUsage{PromptTokenCount: 50, CandidatesTokenCount: 15, ThoughtsTokenCount: 47},
	}
	out := geminiToAnthropic(resp, "gemini-2.5-flash", sigs, newID)

	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q (Gemini reports STOP for tool calls)", out.StopReason)
	}
	if out.Usage.InputTokens != 50 || out.Usage.OutputTokens != 62 {
		t.Errorf("usage = %+v, want 50/62 (candidates+thoughts)", out.Usage)
	}
	if len(out.Content) != 4 {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Content[0]["type"] != "thinking" || out.Content[0]["signature"] != "dGhpbmtzaWc=" {
		t.Errorf("thinking block = %+v", out.Content[0])
	}
	if out.Content[1]["type"] != "text" || out.Content[1]["text"] != "calling tools" {
		t.Errorf("text block = %+v", out.Content[1])
	}
	if out.Content[2]["id"] != "toolu_a" || out.Content[2]["name"] != "get_weather" {
		t.Errorf("tool_use block = %+v", out.Content[2])
	}
	if !strings.HasPrefix(out.ID, "msg_") {
		t.Errorf("message id = %q", out.ID)
	}

	// Store must hold the exact signature for the first call and "" for the second.
	if rec, _ := sigs.lookup("toolu_a"); rec.signature != "Y2FsbHNpZw==" || rec.name != "get_weather" {
		t.Errorf("store[toolu_a] = %+v", rec)
	}
	if rec, ok := sigs.lookup("toolu_b"); !ok || rec.signature != "" {
		t.Errorf("store[toolu_b] = %+v, want present with empty signature", rec)
	}
}

func TestGeminiToAnthropicStopReasons(t *testing.T) {
	resp := &geminiResponse{Candidates: []geminiCandidate{{
		FinishReason: "MAX_TOKENS",
		Content:      geminiContent{Parts: []geminiPart{{Text: "trunca"}}},
	}}}
	out := geminiToAnthropic(resp, "m", newSignatureStore(), newToolUseID)
	if out.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
	// No candidates (e.g. safety block) degrades to an empty end_turn message.
	out = geminiToAnthropic(&geminiResponse{}, "m", newSignatureStore(), newToolUseID)
	if out.StopReason != "end_turn" || len(out.Content) != 0 {
		t.Errorf("empty response = %+v", out)
	}
}

func TestSignatureStoreLatch(t *testing.T) {
	s := newSignatureStore()
	s.record("id1", "fn", "")
	s.latchSignature("id1", "bGF0ZQ==")
	if rec, _ := s.lookup("id1"); rec.signature != "bGF0ZQ==" {
		t.Errorf("latch failed: %+v", rec)
	}
	s.latchSignature("id1", "b3ZlcndyaXRl")
	if rec, _ := s.lookup("id1"); rec.signature != "bGF0ZQ==" {
		t.Errorf("latch must not overwrite: %+v", rec)
	}
	s.latchSignature("missing", "c2ln")
	if _, ok := s.lookup("missing"); ok {
		t.Error("latch must not create entries")
	}
}
