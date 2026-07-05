package simplerouter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAITranslationPreservesReasoningAndToolIDs(t *testing.T) {
	rawReasoning := json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"encrypted"}`)
	resp := &openAIResponse{
		Model: "gpt-5.5",
		Output: []json.RawMessage{
			rawReasoning,
			json.RawMessage(`{"type":"function_call","call_id":"call_123","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}`),
		},
		Usage: openAIUsage{InputTokens: 10, OutputTokens: 20},
	}

	anth := openAIToAnthropic(resp, "gpt-5.5")
	if anth.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", anth.StopReason)
	}
	if len(anth.Content) != 2 || anth.Content[0]["type"] != "redacted_thinking" {
		t.Fatalf("content = %+v", anth.Content)
	}
	toolID, _ := anth.Content[1]["id"].(string)
	if got := decodeOpenAIToolID(toolID); got != "call_123" {
		t.Fatalf("decoded tool id = %q", got)
	}

	assistantContent, err := json.Marshal(anth.Content)
	if err != nil {
		t.Fatal(err)
	}
	req := &anthropicRequest{
		Model:     "gpt-5.5",
		MaxTokens: 1024,
		Messages: []anthropicMessage{
			{Role: "assistant", Content: assistantContent},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"72F"}]`)},
		},
	}
	payload, err := anthropicToOpenAIResponses(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	input := out["input"].([]any)
	reasoning := input[0].(map[string]any)
	if reasoning["encrypted_content"] != "encrypted" {
		t.Fatalf("reasoning item not preserved: %+v", reasoning)
	}
	if summary, ok := reasoning["summary"].([]any); !ok || len(summary) != 0 {
		t.Fatalf("reasoning summary = %+v, want empty required array", reasoning["summary"])
	}
	if input[1].(map[string]any)["call_id"] != "call_123" {
		t.Fatalf("tool result call_id not preserved: %+v", input[1])
	}
}

func TestOpenAITranslationUsesXHighEffortAndAssistantPhase(t *testing.T) {
	toolID := encodeOpenAIToolUseID(openAIOutputItem{
		ID:     "fc_123",
		Status: "completed",
		CallID: "call_123",
	})
	req := &anthropicRequest{
		Model:     "gpt-5.5",
		MaxTokens: 1024,
		Thinking:  &anthropicThinking{Type: "enabled", BudgetTokens: 20_000},
		Messages: []anthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"I will check that."},
				{"type":"tool_use","id":"` + toolID + `","name":"get_weather","input":{"city":"Paris"}}
			]`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"Done."}]`)},
		},
	}
	payload, err := anthropicToOpenAIResponses(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	reasoning := out["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning effort = %v, want xhigh", reasoning["effort"])
	}
	input := out["input"].([]any)
	commentary := input[0].(map[string]any)
	if commentary["phase"] != "commentary" {
		t.Fatalf("tool preamble phase = %v", commentary["phase"])
	}
	tool := input[1].(map[string]any)
	if tool["id"] != "fc_123" || tool["status"] != "completed" || tool["call_id"] != "call_123" {
		t.Fatalf("function_call state = %+v", tool)
	}
	final := input[2].(map[string]any)
	if final["phase"] != "final_answer" {
		t.Fatalf("final assistant phase = %v", final["phase"])
	}
}

func TestOpenAIStreamPreservesRawReasoningItem(t *testing.T) {
	rawItem := json.RawMessage(`{"type":"reasoning","id":"rs_1","status":"completed","summary":[],"encrypted_content":"secret","future_field":"keep"}`)
	var buf bytes.Buffer
	tr := newOpenAIStreamTranslator(&buf, nil, "gpt-5.5")
	if err := tr.onEvent(json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + string(rawItem) + `}`)); err != nil {
		t.Fatal(err)
	}
	tr.finish()
	if !strings.Contains(buf.String(), encodeOpenAIReasoningItem(rawItem)) {
		t.Fatalf("stream did not preserve raw reasoning item: %s", buf.String())
	}
}

func TestOpenAIStreamBuffersToolDeltaUntilToolItem(t *testing.T) {
	var buf bytes.Buffer
	tr := newOpenAIStreamTranslator(&buf, nil, "gpt-5.5")
	if err := tr.onEvent(json.RawMessage(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tr.onEvent(json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_123","status":"completed","call_id":"call_123","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`)); err != nil {
		t.Fatal(err)
	}
	tr.finish()
	out := buf.String()
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("started an empty tool block: %s", out)
	}
	if !strings.Contains(out, `"name":"get_weather"`) || !strings.Contains(out, `{\"city\":`) {
		t.Fatalf("tool block or buffered delta missing: %s", out)
	}
}

func TestZAITranslationPreservesReasoningAndTools(t *testing.T) {
	req := &anthropicRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Thinking:  &anthropicThinking{Type: "enabled", BudgetTokens: 20_000},
		Messages: []anthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"plan","signature":"zai#old"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"72F"}]`)},
		},
	}
	payload, err := anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v", out["reasoning_effort"])
	}
	thinking := out["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["clear_thinking"] != false {
		t.Fatalf("thinking = %+v", thinking)
	}
	messages := out["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if assistant["reasoning_content"] != "plan" {
		t.Fatalf("reasoning_content = %+v", assistant)
	}
	if !strings.Contains(mustJSON(t, assistant["tool_calls"]), "toolu_1") {
		t.Fatalf("tool call id not preserved: %+v", assistant["tool_calls"])
	}
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" {
		t.Fatalf("tool result = %+v", tool)
	}

	anth := zaiToAnthropic(&zaiChatResponse{
		Model: "glm-5.2",
		Choices: []zaiChoice{{
			Message: zaiMessage{
				ReasoningContent: "new plan",
				Content:          "answer",
				ToolCalls: []zaiToolCall{{
					ID: "call_1",
					Function: zaiToolFunction{
						Name:      "get_weather",
						Arguments: zaiToolArguments(`{"city":"Paris"}`),
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}, "glm-5.2")
	if anth.StopReason != "tool_use" || len(anth.Content) != 3 {
		t.Fatalf("anthropic response = %+v", anth)
	}
	if anth.Content[0]["type"] != "thinking" || anth.Content[0]["signature"] != zaiThinkingSignature {
		t.Fatalf("thinking block = %+v", anth.Content[0])
	}
	if anth.Content[2]["id"] != "call_1" {
		t.Fatalf("tool id = %+v", anth.Content[2])
	}
}

func TestZAITranslationForwardsUserImages(t *testing.T) {
	req := &anthropicRequest{
		Model:     "glm-5v-turbo",
		MaxTokens: 1024,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"look at this"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},
				{"type":"text","text":"describe it"}
			]`)},
		},
	}
	payload, err := anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	messages := out["messages"].([]any)
	user := messages[0].(map[string]any)
	content, ok := user["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("content = %#v", user["content"])
	}
	image := content[1].(map[string]any)
	imageURL := image["image_url"].(map[string]any)
	if image["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image content = %+v", image)
	}
}

func TestZAITranslationAcceptsObjectToolArguments(t *testing.T) {
	var resp zaiChatResponse
	if err := json.Unmarshal([]byte(`{
		"model":"glm-5.2",
		"choices":[{
			"message":{
				"tool_calls":[{
					"id":"call_1",
					"type":"function",
					"function":{"name":"get_weather","arguments":{"city":"Paris"}}
				}]
			},
			"finish_reason":"tool_calls"
		}]
	}`), &resp); err != nil {
		t.Fatal(err)
	}
	anth := zaiToAnthropic(&resp, "glm-5.2")
	if anth.StopReason != "tool_use" || len(anth.Content) != 1 {
		t.Fatalf("anthropic response = %+v", anth)
	}
	input, ok := anth.Content[0]["input"].(json.RawMessage)
	if !ok || string(input) != `{"city":"Paris"}` {
		t.Fatalf("tool input = %#v", anth.Content[0]["input"])
	}
}

func TestZAITranslationUsesOnlySupportedToolChoice(t *testing.T) {
	req := &anthropicRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		ToolChoice: &anthropicToolChoice{
			Type: "none",
		},
		Tools: []anthropicTool{
			{Name: "get_weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "search", Description: "Search docs", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
		},
	}
	payload, err := anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["tools"]; ok {
		t.Fatalf("tools should be omitted for unsupported none choice: %+v", out)
	}
	if _, ok := out["tool_choice"]; ok {
		t.Fatalf("tool_choice should be omitted for unsupported none choice: %+v", out)
	}

	req.ToolChoice = &anthropicToolChoice{Type: "auto"}
	payload, err = anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", out["tool_choice"])
	}
	tools := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool name = %v", fn["name"])
	}

	req.ToolChoice = &anthropicToolChoice{Type: "any"}
	payload, err = anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	out = nil
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("any tool_choice = %v, want auto fallback", out["tool_choice"])
	}
	if tools := out["tools"].([]any); len(tools) != 2 {
		t.Fatalf("any tools = %+v, want all tools", tools)
	}

	req.ToolChoice = &anthropicToolChoice{Type: "tool", Name: "get_weather"}
	payload, err = anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	out = nil
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("named tool_choice = %v, want auto fallback", out["tool_choice"])
	}
	tools = out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("named tools = %+v, want only selected tool", tools)
	}
	fn = tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("named tool = %v", fn["name"])
	}
}

func TestZAIProxyNetworkErrorIsError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"model":"glm-5.2","choices":[{"message":{"content":"partial"},"finish_reason":"network_error"}]}`)
	}))
	defer upstream.Close()

	baseURL, stop, err := startZAIProxy(upstream.URL, "glm-5.2", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	errObj := body["error"].(map[string]any)
	if body["type"] != "error" || errObj["type"] != "api_error" || !strings.Contains(errObj["message"].(string), "network_error") {
		t.Fatalf("error body = %+v", body)
	}
}

func TestZAIStreamNetworkErrorEmitsError(t *testing.T) {
	var buf bytes.Buffer
	tr := newZAIStreamTranslator(&buf, nil, "glm-5.2")
	err := tr.onEvent(json.RawMessage(`{"choices":[{"finish_reason":"network_error"}]}`))
	if err != errStreamAborted {
		t.Fatalf("err = %v, want errStreamAborted", err)
	}
	tr.finish()

	events := parseSSE(t, buf.String())
	last := events[len(events)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %q, want error", last.Name)
	}
	errObj := last.Data["error"].(map[string]any)
	if errObj["type"] != "api_error" || !strings.Contains(errObj["message"].(string), "network_error") {
		t.Fatalf("error = %+v", errObj)
	}
	for _, name := range eventNames(events) {
		if name == "message_stop" {
			t.Fatal("message_stop must not follow network_error")
		}
	}
}

func TestZAIReasoningEffortOnlyForSupportedModels(t *testing.T) {
	req := &anthropicRequest{
		Model:     "glm-5",
		MaxTokens: 1024,
		Thinking:  &anthropicThinking{Type: "enabled", BudgetTokens: 20_000},
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"think"}]`)},
		},
	}
	payload, err := anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["reasoning_effort"]; ok {
		t.Fatalf("glm-5 should not send reasoning_effort: %+v", out)
	}

	req.Model = "glm-4-32b-0414-128k"
	payload, err = anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	out = nil
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["thinking"]; ok {
		t.Fatalf("unsupported model should not send thinking: %+v", out)
	}
	if _, ok := out["reasoning_effort"]; ok {
		t.Fatalf("unsupported model should not send reasoning_effort: %+v", out)
	}

	req.Model = "glm-5.2"
	payload, err = anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	out = nil
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v, want max", out["reasoning_effort"])
	}
}

func TestZAIStopSequencesOnlyForwardsFirst(t *testing.T) {
	req := &anthropicRequest{
		Model:         "z-ai/glm-5.2[1m]",
		MaxTokens:     1024,
		StopSequences: []string{"first", "second"},
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
		},
	}
	payload, err := anthropicToZAIChat(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "glm-5.2" {
		t.Fatalf("model = %v", out["model"])
	}
	stop := out["stop"].([]any)
	if len(stop) != 1 || stop[0] != "first" {
		t.Fatalf("stop = %+v", stop)
	}
}

func TestZAIStreamBuffersToolArgumentsUntilName(t *testing.T) {
	var buf bytes.Buffer
	tr := newZAIStreamTranslator(&buf, nil, "glm-5.2")
	if err := tr.onEvent(json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"type":"tool_use"`) {
		t.Fatalf("started tool block before name: %s", buf.String())
	}
	if err := tr.onEvent(json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"\"Paris\"}"}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	tr.finish()
	out := buf.String()
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("started an empty tool block: %s", out)
	}
	if !strings.Contains(out, `"name":"get_weather"`) || !strings.Contains(out, `{\"city\":`) || !strings.Contains(out, `\"Paris\"}`) {
		t.Fatalf("tool block or buffered arguments missing: %s", out)
	}
}

func TestZAIStopReasonMappings(t *testing.T) {
	if got := zaiStopReason("model_context_window_exceeded", false); got != "max_tokens" {
		t.Fatalf("context stop = %q", got)
	}
	if got := zaiStopReason("sensitive", false); got != "refusal" {
		t.Fatalf("sensitive stop = %q", got)
	}
	if got := zaiStopReason("network_error", false); got != "" {
		t.Fatalf("network_error stop = %q, want no clean stop reason", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
