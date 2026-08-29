package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeepSeekResponsesOptions(t *testing.T) {
	got := deepSeekResponsesOptions(false)
	if got.ChatPath != "/chat/completions" || got.ReasoningReplayField != "reasoning_content" || !got.IncludeStreamUsage {
		t.Fatalf("DeepSeek options = %+v", got)
	}
	if got.ReasoningEffortMap["medium"] != "high" || got.ReasoningEffortMap["xhigh"] != "max" {
		t.Fatalf("DeepSeek effort map = %#v", got.ReasoningEffortMap)
	}
	thinking := got.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("DeepSeek thinking = %#v", thinking)
	}

	disabled := deepSeekResponsesOptions(true)
	if disabled.ExtraBody["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("DeepSeek disabled thinking = %#v", disabled.ExtraBody)
	}
}

func TestZAIResponsesOptions(t *testing.T) {
	got := zaiResponsesOptions(false)
	if !got.ToolStream || got.IncludeStreamUsage || !got.SendNoneReasoningEffort || got.ReasoningEffortMap["ultra"] != "max" {
		t.Fatalf("Z.AI options = %+v", got)
	}
	thinking := got.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["clear_thinking"] != false {
		t.Fatalf("Z.AI thinking = %#v", thinking)
	}

	disabled := zaiResponsesOptions(true)
	if disabled.ExtraBody["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("Z.AI disabled thinking = %#v", disabled.ExtraBody)
	}
}

func TestOpenRouterResponsesOptions(t *testing.T) {
	got := openRouterResponsesOptions("z-ai/glm-5.2", "")
	if got.Label != "OpenRouter" || got.ProviderTag != "" || got.TranslateCustomTools {
		t.Fatalf("OpenRouter options = %+v", got)
	}
	pinned := openRouterResponsesOptions("z-ai/glm-5.2", "z-ai")
	if pinned.ProviderTag != "z-ai" || pinned.TranslateCustomTools {
		t.Fatalf("OpenRouter pinned options = %+v", pinned)
	}
	// Routes served by Meta's own endpoint reject custom tools and recursive
	// schemas, so they must reuse the Meta tool translation.
	for _, route := range []struct{ model, tag string }{
		{"meta/muse-spark-1.2-contributor", "Meta"},
		{"meta/muse-spark-1.2-contributor", ""},
		{"some/other-model", "meta"},
	} {
		got := openRouterResponsesOptions(route.model, route.tag)
		if !got.TranslateCustomTools || got.ProviderTag != route.tag {
			t.Fatalf("OpenRouter Meta route %+v options = %+v", route, got)
		}
	}
	if opts := openRouterResponsesOptions("meta-llama/llama-3.3-70b-instruct", ""); opts.TranslateCustomTools {
		t.Fatalf("open-weight Llama route should stay passthrough: %+v", opts)
	}
}

func TestMetaResponsesOptions(t *testing.T) {
	got := metaResponsesOptions()
	if got.Label != "Meta" || !got.TranslateCustomTools {
		t.Fatalf("Meta options = %+v", got)
	}
	if got.ReasoningEffortMap["max"] != "xhigh" || got.ReasoningEffortMap["ultra"] != "xhigh" {
		t.Fatalf("Meta effort map = %#v", got.ReasoningEffortMap)
	}
}

func TestXAIResponsesOptions(t *testing.T) {
	got := xaiResponsesOptions("grok-4.5")
	if got.Label != "xAI" || !got.TranslateCustomTools || !got.FlattenNamespaces {
		t.Fatalf("xAI options = %+v", got)
	}
	if !got.OmitNullEncryptedReasoningContent || got.OmitReasoningControls {
		t.Fatalf("xAI Grok 4.5 reasoning options = %+v", got)
	}
	for input, want := range map[string]string{
		"none": "low", "minimal": "low", "xhigh": "high", "max": "high", "ultra": "high",
	} {
		if got.ReasoningEffortMap[input] != want {
			t.Fatalf("xAI Grok 4.5 effort map[%q] = %q, want %q", input, got.ReasoningEffortMap[input], want)
		}
	}
	allowed := map[string]bool{}
	for _, tpe := range got.AllowedToolTypes {
		allowed[tpe] = true
	}
	if !allowed["function"] || allowed["namespace"] || allowed["tool_search"] || allowed["custom"] {
		t.Fatalf("xAI allowed tool types = %#v", got.AllowedToolTypes)
	}

	grok43 := xaiResponsesOptions("grok-4.3")
	if _, mapped := grok43.ReasoningEffortMap["none"]; mapped {
		t.Fatalf("Grok 4.3 none effort should be preserved: %#v", grok43.ReasoningEffortMap)
	}
	if grok43.ReasoningEffortMap["xhigh"] != "high" {
		t.Fatalf("Grok 4.3 xhigh effort map = %#v", grok43.ReasoningEffortMap)
	}

	multiAgent := xaiResponsesOptions("grok-4.20-multi-agent-0309")
	if multiAgent.ReasoningEffortMap["xhigh"] != "xhigh" || multiAgent.ReasoningEffortMap["ultra"] != "xhigh" {
		t.Fatalf("Grok Multi-Agent effort map = %#v", multiAgent.ReasoningEffortMap)
	}

	for _, model := range []string{"grok-build-0.1", "grok-4.20-0309-reasoning"} {
		fixed := xaiResponsesOptions(model)
		if !fixed.OmitReasoningEffort || fixed.OmitReasoningControls {
			t.Fatalf("%s fixed reasoning options = %+v", model, fixed)
		}
	}
	nonReasoning := xaiResponsesOptions("grok-4.20-0309-non-reasoning")
	if nonReasoning.OmitReasoningEffort || !nonReasoning.OmitReasoningControls {
		t.Fatalf("Grok non-reasoning options = %+v", nonReasoning)
	}
}

func TestDeepSeekResponsesRequestMatchesCurrentAPI(t *testing.T) {
	body := captureProviderChatRequest(t, "deepseek-v4-flash", deepSeekResponsesOptions(false), "low")

	if body["model"] != "deepseek-v4-flash" || body["reasoning_effort"] != "high" {
		t.Fatalf("DeepSeek routing/reasoning = %#v", body)
	}
	if body["max_completion_tokens"] != float64(2048) {
		t.Fatalf("DeepSeek max completion tokens = %#v", body["max_completion_tokens"])
	}
	if body["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatalf("DeepSeek thinking = %#v", body["thinking"])
	}
	if body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("DeepSeek stream options = %#v", body["stream_options"])
	}
	if _, found := body["tool_stream"]; found {
		t.Fatalf("DeepSeek request unexpectedly enabled Z.AI tool streaming: %#v", body)
	}
	assertProviderReasoningReplay(t, body)
}

func TestZAIResponsesRequestMatchesCurrentAPI(t *testing.T) {
	body := captureProviderChatRequest(t, "glm-5.2", zaiResponsesOptions(false), "ultra")

	if body["model"] != "glm-5.2" || body["reasoning_effort"] != "max" {
		t.Fatalf("Z.AI routing/reasoning = %#v", body)
	}
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["clear_thinking"] != false {
		t.Fatalf("Z.AI thinking = %#v", thinking)
	}
	if body["tool_stream"] != true {
		t.Fatalf("Z.AI tool_stream = %#v", body["tool_stream"])
	}
	if _, found := body["stream_options"]; found {
		t.Fatalf("Z.AI request unexpectedly enabled DeepSeek stream usage: %#v", body)
	}
	assertProviderReasoningReplay(t, body)
}

func TestZAIResponsesForwardsNoneReasoningEffort(t *testing.T) {
	body := captureProviderChatRequest(t, "glm-5.2", zaiResponsesOptions(false), "none")
	if body["reasoning_effort"] != "none" {
		t.Fatalf("Z.AI reasoning_effort = %#v, want none", body["reasoning_effort"])
	}
}

func captureProviderChatRequest(t *testing.T, model string, options responsesChatProxyOptions, effort string) map[string]any {
	t.Helper()
	requests := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_provider\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_provider\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newResponsesChatProxy(upstream.URL, model, upstream.Client(), options))
	defer proxy.Close()

	replay := encodeChatReplayState(chatReplayState{
		MessageFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"exact prior reasoning"`),
		},
	})
	requestBody := map[string]any{
		"model":        "codex-selected-model",
		"instructions": "You are Codex.",
		"input": []any{
			map[string]any{
				"type":              "reasoning",
				"encrypted_content": replay,
			},
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "I will inspect it."},
				},
			},
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "continue"},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "shell_command",
				"description": "Run a command",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
		},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": effort, "summary": "auto"},
		"max_output_tokens":   2048,
		"stream":              true,
	}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer provider-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.StatusCode, responseBody)
	}
	return <-requests
}

func assertProviderReasoningReplay(t *testing.T, body map[string]any) {
	t.Helper()
	messages := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("provider messages = %#v", messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "exact prior reasoning" || assistant["content"] != "I will inspect it." {
		t.Fatalf("provider assistant replay = %#v", assistant)
	}
	tools := body["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "shell_command" {
		t.Fatalf("provider tools = %#v", tools)
	}
}
