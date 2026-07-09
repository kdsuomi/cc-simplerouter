package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func decodeRequestPayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("payload not valid JSON: %v\n%s", err, payload)
	}
	return out
}

func TestOpenRouterTranslationBuildsChatCompletionsRequest(t *testing.T) {
	temp := 0.5
	req := &anthropicRequest{
		Model:     "moonshotai/kimi-k2[1m]",
		MaxTokens: 4096,
		Stream:    true,
		System:    json.RawMessage(`[{"type":"text","text":"be terse","cache_control":{"type":"ephemeral"}}]`),
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"thinking","thinking":"pondering ","signature":"` + openRouterThinkingSignature + `"},
				{"type":"thinking","thinking":"still","signature":"` + openRouterThinkingSignature + `"},
				{"type":"text","text":"calling a tool"},
				{"type":"tool_use","id":"call_9","name":"get_weather","input":{"city":"Paris"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_9","content":"sunny","cache_control":{"type":"ephemeral"}}]`)},
		},
		Tools: []anthropicTool{
			{Name: "get_weather", Description: "Weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
			{Name: "web_search", Type: "web_search_20250305"},
		},
		ToolChoice:    &anthropicToolChoice{Type: "any"},
		Temperature:   &temp,
		StopSequences: []string{"STOP", "HALT"},
		Thinking:      &anthropicThinking{Type: "enabled", BudgetTokens: 9000},
	}
	payload, err := anthropicToOpenRouterChat(req, openRouterProxyOptions{ProviderTag: "novita/fp8", SupportsReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeRequestPayload(t, payload)

	if got["model"] != "moonshotai/kimi-k2" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["max_tokens"] != float64(4096) || got["temperature"] != 0.5 || got["stream"] != true {
		t.Fatalf("basics wrong: %v", got)
	}
	if stop, _ := got["stop"].([]any); len(stop) != 2 {
		t.Fatalf("stop = %v, want both sequences", got["stop"])
	}
	if reasoning := got["reasoning"].(map[string]any); reasoning["max_tokens"] != float64(9000) {
		t.Fatalf("reasoning = %v", got["reasoning"])
	}
	provider := got["provider"].(map[string]any)
	if only := provider["only"].([]any); len(only) != 1 || only[0] != "novita/fp8" || provider["allow_fallbacks"] != false {
		t.Fatalf("provider = %v", provider)
	}
	if usage := got["usage"].(map[string]any); usage["include"] != true {
		t.Fatalf("usage = %v", got["usage"])
	}
	// Explicit cache breakpoints exist, so no automatic top-level one.
	if _, ok := got["cache_control"]; ok {
		t.Fatalf("unexpected top-level cache_control alongside explicit breakpoints")
	}

	messages := got["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d: %v", len(messages), messages)
	}
	system := messages[0].(map[string]any)
	sysParts := system["content"].([]any)
	if system["role"] != "system" || len(sysParts) != 1 {
		t.Fatalf("system = %v", system)
	}
	if part := sysParts[0].(map[string]any); part["text"] != "be terse" || part["cache_control"] == nil {
		t.Fatalf("system part lost cache_control: %v", part)
	}
	if user := messages[1].(map[string]any); user["content"] != "hi" {
		t.Fatalf("user = %v", user)
	}
	assistant := messages[2].(map[string]any)
	if assistant["content"] != "calling a tool" {
		t.Fatalf("assistant content = %v", assistant["content"])
	}
	// Split thinking blocks reassemble without inventing separators.
	if assistant["reasoning"] != "pondering still" {
		t.Fatalf("assistant reasoning = %q", assistant["reasoning"])
	}
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_9" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("tool call = %v", call)
	}
	toolMsg := messages[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_9" {
		t.Fatalf("tool message = %v", toolMsg)
	}
	toolParts := toolMsg["content"].([]any)
	if part := toolParts[0].(map[string]any); part["text"] != "sunny" || part["cache_control"] == nil {
		t.Fatalf("tool result part lost cache_control: %v", part)
	}

	tools := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v (server stub must be dropped)", tools)
	}
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", got["tool_choice"])
	}
}

func TestOpenRouterTranslationRoundTripsReasoningDetails(t *testing.T) {
	details := []openRouterReasoningDetail{
		{Type: "reasoning.text", Text: "hidden chain", Signature: "sig1", Format: "anthropic-claude-v1"},
		{Type: "reasoning.encrypted", Data: "opaque", Format: "openai-responses-v1"},
	}
	encoded := encodeOpenRouterReasoningDetails(details)
	req := &anthropicRequest{
		Model:     "anthropic/claude-sonnet-4.5",
		MaxTokens: 512,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(mustJSON(t, []map[string]any{
				{"type": "thinking", "thinking": "hidden chain", "signature": openRouterThinkingSignature},
				{"type": "redacted_thinking", "data": encoded},
				{"type": "tool_use", "id": "call_1", "name": "look", "input": map[string]any{}},
			}))},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]`)},
		},
	}
	payload, err := anthropicToOpenRouterChat(req, openRouterProxyOptions{SupportsReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeRequestPayload(t, payload)
	assistant := got["messages"].([]any)[1].(map[string]any)
	if _, hasReasoning := assistant["reasoning"]; hasReasoning {
		t.Fatalf("assistant must not duplicate reasoning next to reasoning_details: %v", assistant)
	}
	rd, err := json.Marshal(assistant["reasoning_details"])
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped []openRouterReasoningDetail
	if err := json.Unmarshal(rd, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTripped, details) {
		t.Fatalf("reasoning_details = %s", rd)
	}
}

func TestOpenRouterReasoningParam(t *testing.T) {
	on := openRouterProxyOptions{SupportsReasoning: true}
	if got := openRouterReasoningParam(&anthropicThinking{Type: "adaptive"}, on); got["enabled"] != true {
		t.Fatalf("adaptive = %v", got)
	}
	if got := openRouterReasoningParam(&anthropicThinking{Type: "enabled", BudgetTokens: 2048}, on); got["max_tokens"] != 2048 {
		t.Fatalf("enabled+budget = %v", got)
	}
	if got := openRouterReasoningParam(&anthropicThinking{Type: "disabled"}, on); got != nil {
		t.Fatalf("disabled = %v", got)
	}
	if got := openRouterReasoningParam(nil, on); got != nil {
		t.Fatalf("nil thinking = %v", got)
	}
	if got := openRouterReasoningParam(&anthropicThinking{Type: "enabled"}, openRouterProxyOptions{}); got != nil {
		t.Fatalf("unsupported model must not get reasoning: %v", got)
	}
	if got := openRouterReasoningParam(&anthropicThinking{Type: "enabled"}, openRouterProxyOptions{SupportsReasoning: true, DisableThinking: true}); got != nil {
		t.Fatalf("disable-thinking must suppress reasoning: %v", got)
	}
}

func TestOpenRouterTranslationAddsAutomaticCacheControl(t *testing.T) {
	req := &anthropicRequest{
		Model:     "z-ai/glm-5.2",
		MaxTokens: 100,
		System:    json.RawMessage(`"plain system"`),
		Messages:  []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	payload, err := anthropicToOpenRouterChat(req, openRouterProxyOptions{SupportsReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeRequestPayload(t, payload)
	if cc, ok := got["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Fatalf("automatic cache_control missing: %v", got["cache_control"])
	}
	if got["messages"].([]any)[0].(map[string]any)["content"] != "plain system" {
		t.Fatalf("system = %v", got["messages"].([]any)[0])
	}
}

func runOpenRouterStream(t *testing.T, chunks ...string) []sseEvent {
	t.Helper()
	var out bytes.Buffer
	tr := newOpenRouterStreamTranslator(&out, nil, "test/model")
	for _, chunk := range chunks {
		if err := tr.onEvent(json.RawMessage(chunk)); err != nil {
			if err == errStreamAborted {
				break
			}
			t.Fatal(err)
		}
	}
	tr.finish()
	return parseSSE(t, out.String())
}

func TestOpenRouterStreamTranslatesReasoningTextAndTools(t *testing.T) {
	events := runOpenRouterStream(t,
		`{"choices":[{"delta":{"role":"assistant","reasoning":"I will think "}}]}`,
		`{"choices":[{"delta":{"reasoning":"more","reasoning_details":[{"type":"reasoning.text","text":"I will think "}]}}]}`,
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"more","signature":"sig-final"}]}}]}`,
		`{"choices":[{"delta":{"content":"Answer"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":10}}}`,
	)

	wantNames := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // "I will think "
		"content_block_delta", // "more" flushed when text starts
		"content_block_delta", // signature
		"content_block_stop",
		"content_block_start", // redacted_thinking (reasoning_details), before content
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta",
		"content_block_stop",
		"content_block_start", // tool_use
		"content_block_delta", // args part 1
		"content_block_delta", // args part 2
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if got := eventNames(events); !slices.Equal(got, wantNames) {
		t.Fatalf("event names = %v, want %v", got, wantNames)
	}

	thinkingStart := events[1].Data["content_block"].(map[string]any)
	if thinkingStart["type"] != "thinking" || thinkingStart["signature"] != openRouterThinkingSignature {
		t.Fatalf("thinking block start = %v", thinkingStart)
	}
	if d := events[2].Data["delta"].(map[string]any); d["thinking"] != "I will think " {
		t.Fatalf("first thinking delta = %v", d)
	}
	if d := events[3].Data["delta"].(map[string]any); d["thinking"] != "more" {
		t.Fatalf("flushed thinking delta = %v", d)
	}
	toolStart := events[11].Data["content_block"].(map[string]any)
	if toolStart["type"] != "tool_use" || toolStart["id"] != "call_1" || toolStart["name"] != "get_weather" {
		t.Fatalf("tool block = %v", toolStart)
	}
	args := events[12].Data["delta"].(map[string]any)["partial_json"].(string) +
		events[13].Data["delta"].(map[string]any)["partial_json"].(string)
	if args != `{"city":"Paris"}` {
		t.Fatalf("tool args = %q", args)
	}

	redacted := events[6].Data["content_block"].(map[string]any)
	items := decodeOpenRouterReasoningDetails(redacted["data"].(string))
	if len(items) != 1 {
		t.Fatalf("reasoning_details items = %d, want 1 merged entry: %s", len(items), redacted["data"])
	}
	var detail openRouterReasoningDetail
	if err := json.Unmarshal(items[0], &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Text != "I will think more" || detail.Signature != "sig-final" {
		t.Fatalf("merged detail = %+v", detail)
	}

	md := events[15].Data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", md["delta"])
	}
	usage := md["usage"].(map[string]any)
	if usage["input_tokens"] != float64(30) || usage["output_tokens"] != float64(20) ||
		usage["cache_read_input_tokens"] != float64(60) || usage["cache_creation_input_tokens"] != float64(10) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestOpenRouterStreamShowsDetailOnlyReasoning(t *testing.T) {
	// Some providers stream reasoning only through reasoning_details.
	events := runOpenRouterStream(t,
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"quiet thoughts "}]}}]}`,
		`{"choices":[{"delta":{"content":"done"}}]}`,
	)
	var thinking string
	for _, ev := range events {
		if ev.Name != "content_block_delta" {
			continue
		}
		if d := ev.Data["delta"].(map[string]any); d["type"] == "thinking_delta" {
			thinking += d["thinking"].(string)
		}
	}
	if thinking != "quiet thoughts " {
		t.Fatalf("visible thinking = %q", thinking)
	}
}

func TestOpenRouterStreamRotatesThinkingBlocks(t *testing.T) {
	var out bytes.Buffer
	tr := newOpenRouterStreamTranslator(&out, nil, "test/model")
	now := time.Unix(0, 0)
	tr.thinking.now = func() time.Time { return now }

	if err := tr.onEvent(json.RawMessage(`{"choices":[{"delta":{"reasoning":"alpha beta gamma delta epsilon "}}]}`)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(thinkingRotateEvery + time.Millisecond)
	if err := tr.onEvent(json.RawMessage(`{"choices":[{"delta":{"reasoning":"zeta eta "}}]}`)); err != nil {
		t.Fatal(err)
	}
	tr.finish()

	events := parseSSE(t, out.String())
	starts := 0
	var text string
	for _, ev := range events {
		if ev.Name == "content_block_start" && ev.Data["content_block"].(map[string]any)["type"] == "thinking" {
			starts++
		}
		if ev.Name == "content_block_delta" {
			if d := ev.Data["delta"].(map[string]any); d["type"] == "thinking_delta" {
				text += d["thinking"].(string)
			}
		}
	}
	if starts != 2 {
		t.Fatalf("thinking block starts = %d, want 2\n%s", starts, out.String())
	}
	if text != "alpha beta gamma delta epsilon zeta eta " {
		t.Fatalf("reassembled thinking = %q", text)
	}
}

func TestOpenRouterStreamEmitsErrorChunk(t *testing.T) {
	var out bytes.Buffer
	tr := newOpenRouterStreamTranslator(&out, nil, "test/model")
	if err := tr.onEvent(json.RawMessage(`{"choices":[{"delta":{"content":"partial"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := tr.onEvent(json.RawMessage(`{"error":{"code":502,"message":"Provider returned error"},"choices":[{"delta":{},"finish_reason":"error"}]}`)); err != errStreamAborted {
		t.Fatalf("err = %v, want errStreamAborted", err)
	}
	tr.finish()
	events := parseSSE(t, out.String())
	last := events[len(events)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %s, want error", last.Name)
	}
	if msg := last.Data["error"].(map[string]any)["message"]; msg != "Provider returned error" {
		t.Fatalf("error message = %v", msg)
	}
}

func TestOpenRouterNonStreamingResponseTranslation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-test" {
			t.Errorf("auth = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad upstream body: %v", err)
		}
		if req["model"] != "deepseek/deepseek-r1" {
			t.Errorf("model = %v", req["model"])
		}
		fmt.Fprint(w, `{
			"id":"gen-1","model":"deepseek/deepseek-r1",
			"choices":[{"message":{
				"role":"assistant",
				"content":"final answer",
				"reasoning":"chain of thought",
				"reasoning_details":[{"type":"reasoning.text","text":"chain of thought"}],
				"tool_calls":[{"id":"call_7","type":"function","function":{"name":"probe","arguments":"{\"x\":1}"}}]
			},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":50,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":30}}
		}`)
	}))
	defer upstream.Close()

	p := newOpenRouterProxy(upstream.URL, "deepseek/deepseek-r1", upstream.Client(), openRouterProxyOptions{SupportsReasoning: true})
	body := `{"model":"deepseek/deepseek-r1","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-or-test")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out anthropicMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "tool_use" || out.Role != "assistant" {
		t.Fatalf("response = %+v", out)
	}
	types := make([]string, 0, len(out.Content))
	for _, block := range out.Content {
		types = append(types, block["type"].(string))
	}
	if !slices.Equal(types, []string{"thinking", "redacted_thinking", "text", "tool_use"}) {
		t.Fatalf("content types = %v", types)
	}
	if out.Content[0]["thinking"] != "chain of thought" || out.Content[0]["signature"] != openRouterThinkingSignature {
		t.Fatalf("thinking block = %v", out.Content[0])
	}
	if items := decodeOpenRouterReasoningDetails(out.Content[1]["data"].(string)); len(items) != 1 {
		t.Fatalf("redacted details = %v", out.Content[1])
	}
	if out.Content[2]["text"] != "final answer" {
		t.Fatalf("text block = %v", out.Content[2])
	}
	if out.Content[3]["id"] != "call_7" || out.Content[3]["name"] != "probe" {
		t.Fatalf("tool block = %v", out.Content[3])
	}
	if out.Usage.InputTokens != 20 || out.Usage.OutputTokens != 9 || out.Usage.CacheReadInputTokens != 30 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestOpenRouterProxyStreamsEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"delta":{"role":"assistant","reasoning":"thinking hard "}}]}`,
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newOpenRouterProxy(upstream.URL, "test/model", upstream.Client(), openRouterProxyOptions{SupportsReasoning: true})
	body := `{"model":"test/model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	events := parseSSE(t, rec.Body.String())
	names := eventNames(events)
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("stream shape = %v", names)
	}
	var thinking, text string
	for _, ev := range events {
		if ev.Name != "content_block_delta" {
			continue
		}
		d := ev.Data["delta"].(map[string]any)
		switch d["type"] {
		case "thinking_delta":
			thinking += d["thinking"].(string)
		case "text_delta":
			text += d["text"].(string)
		}
	}
	if thinking != "thinking hard " || text != "done" {
		t.Fatalf("thinking = %q, text = %q", thinking, text)
	}
}

func TestOpenRouterUpstreamErrorRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"code":402,"message":"Insufficient credits"}}`)
	}))
	defer upstream.Close()

	p := newOpenRouterProxy(upstream.URL, "test/model", upstream.Client(), openRouterProxyOptions{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test/model","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want error", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Insufficient credits") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestMergeReasoningDetailsSeparatesDistinctEntries(t *testing.T) {
	idx0, idx1 := 0, 1
	merged := mergeReasoningDetails(nil, []openRouterReasoningDetail{
		{Type: "reasoning.text", Text: "a", Index: &idx0},
		{Type: "reasoning.text", Text: "b", Index: &idx0},
	})
	merged = mergeReasoningDetails(merged, []openRouterReasoningDetail{
		{Type: "reasoning.text", Text: "c", Index: &idx1},
		{Type: "reasoning.encrypted", Data: "xx", Index: &idx1},
	})
	if len(merged) != 3 {
		t.Fatalf("merged = %+v, want 3 entries", merged)
	}
	if merged[0].Text != "ab" || merged[1].Text != "c" || merged[2].Data != "xx" {
		t.Fatalf("merged = %+v", merged)
	}
}
