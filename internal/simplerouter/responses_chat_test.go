package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesToolsToChatFlattensNamespaceAndWrapsCustom(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{
		  "type":"function",
		  "name":"shell_command",
		  "description":"Run a command",
		  "parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}
		}`),
		json.RawMessage(`{
		  "type":"custom",
		  "name":"apply_patch",
		  "description":"Apply a patch",
		  "format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"}
		}`),
		json.RawMessage(`{
		  "type":"namespace",
		  "name":"collaboration",
		  "description":"Sub-agent tools",
		  "tools":[{
		    "type":"function",
		    "name":"spawn_agent",
		    "description":"Spawn an agent",
		    "parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}
		  }]
		}`),
		json.RawMessage(`{"type":"web_search","external_web_access":false}`),
	}
	tools, registry, err := responsesToolsToChat(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("chat tools = %d, want 3 (server web search omitted)", len(tools))
	}
	if !registry.webSearch {
		t.Fatal("web-search capability was not recorded")
	}
	if got := registry.byChatName["collaboration__spawn_agent"]; got.Namespace != "collaboration" || got.Name != "spawn_agent" {
		t.Fatalf("namespace mapping = %#v", got)
	}
	if got := registry.byChatName["apply_patch"]; !got.Custom {
		t.Fatalf("custom mapping = %#v", got)
	}

	custom := tools[1].(map[string]any)["function"].(map[string]any)
	schema := custom["parameters"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["input"]; !ok {
		t.Fatalf("custom tool schema = %#v", schema)
	}
	namespace := tools[2].(map[string]any)["function"].(map[string]any)
	namespaceSchema := namespace["parameters"].(map[string]any)
	messageSchema := namespaceSchema["properties"].(map[string]any)["message"].(map[string]any)
	if _, found := messageSchema["encrypted"]; found {
		t.Fatalf("provider-incompatible schema keyword was not scrubbed: %#v", messageSchema)
	}
}

func TestResponsesInputToChatRestoresReasoningAndToolReplayState(t *testing.T) {
	topExtra := json.RawMessage(`{"google":{"thought_signature":"top-signature"}}`)
	toolExtra := json.RawMessage(`{"google":{"thought_signature":"tool-signature"}}`)
	state := chatReplayState{
		Version: 1,
		MessageFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"private reasoning"`),
			"extra_content":     topExtra,
		},
		ToolFields: map[string]map[string]json.RawMessage{
			"call_patch": {"extra_content": toolExtra},
		},
	}
	encoded := encodeChatReplayState(state)
	req := &responsesRequest{
		Instructions: "system instructions",
		Input: []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{
			  "type":"reasoning",
			  "summary":[{"type":"summary_text","text":"visible summary"}],
			  "content":[{"type":"reasoning_text","text":"visible reasoning"}],
			  "encrypted_content":%q
			}`, encoded)),
			json.RawMessage(`{
			  "type":"message",
			  "role":"assistant",
			  "content":[{"type":"output_text","text":"I will patch it."}]
			}`),
			json.RawMessage(`{
			  "type":"custom_tool_call",
			  "name":"apply_patch",
			  "call_id":"call_patch",
			  "input":"*** Begin Patch\n*** End Patch"
			}`),
			json.RawMessage(`{
			  "type":"custom_tool_call_output",
			  "call_id":"call_patch",
			  "output":"Done"
			}`),
			json.RawMessage(`{
			  "type":"message",
			  "role":"user",
			  "content":[{"type":"input_text","text":"continue"}]
			}`),
		},
	}
	_, registry, err := responsesToolsToChat([]json.RawMessage{
		json.RawMessage(`{"type":"custom","name":"apply_patch","description":"patch"}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := responsesInputToChat(req, registry, "reasoning_content")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want system + assistant + tool + user: %#v", len(messages), messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "private reasoning" {
		t.Fatalf("reasoning replay = %#v", assistant["reasoning_content"])
	}
	extra := assistant["extra_content"].(map[string]any)
	if extra["google"].(map[string]any)["thought_signature"] != "top-signature" {
		t.Fatalf("top replay state = %#v", extra)
	}
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	callExtra := call["extra_content"].(map[string]any)
	if callExtra["google"].(map[string]any)["thought_signature"] != "tool-signature" {
		t.Fatalf("tool replay state = %#v", callExtra)
	}
	function := call["function"].(map[string]any)
	var customArgs map[string]string
	if err := json.Unmarshal([]byte(function["arguments"].(string)), &customArgs); err != nil {
		t.Fatal(err)
	}
	if customArgs["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input = %q", customArgs["input"])
	}
	if tool := messages[2].(map[string]any); tool["role"] != "tool" || tool["content"] != "Done" {
		t.Fatalf("tool result = %#v", tool)
	}
}

func TestChatResponsesStreamTranslatorPreservesToolsReasoningAndUsage(t *testing.T) {
	_, registry, err := responsesToolsToChat([]json.RawMessage{
		json.RawMessage(`{"type":"custom","name":"apply_patch","description":"patch"}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	translator := newChatResponsesStreamTranslator(&output, nil, "vendor/model", registry)
	events := []string{
		`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"reasoning_content":"think ","extra_content":{"google":{"thought_signature":"top"}}}}]}`,
		`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"Working."}}]}`,
		`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"},"extra_content":{"google":{"thought_signature":"tool"}}}]}}]}`,
		`{"id":"chatcmpl_1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl_1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`,
	}
	for _, event := range events {
		if err := translator.onEvent(json.RawMessage(event)); err != nil {
			t.Fatal(err)
		}
	}
	translator.finish()

	decoded := decodeTestSSE(t, output.String())
	var customItem map[string]any
	var messageItem map[string]any
	var completed map[string]any
	var replay chatReplayState
	var sawLiveReasoning bool
	for _, event := range decoded {
		switch event["type"] {
		case "response.reasoning_summary_text.done":
			if event["text"] == "think " {
				sawLiveReasoning = true
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			switch item["type"] {
			case "custom_tool_call":
				customItem = item
			case "message":
				messageItem = item
			case "reasoning":
				if encrypted, _ := item["encrypted_content"].(string); encrypted != "" {
					if state, ok := decodeChatReplayState(encrypted); ok {
						replay.merge(state)
					}
				}
			}
		case "response.completed":
			completed = event["response"].(map[string]any)
		}
	}
	if !sawLiveReasoning {
		t.Fatal("reasoning was not emitted progressively for sequential-cutoff clients")
	}
	if messageItem["phase"] != "commentary" {
		t.Fatalf("message phase = %#v", messageItem["phase"])
	}
	if customItem["name"] != "apply_patch" || customItem["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom tool item = %#v", customItem)
	}
	if completed["end_turn"] != false {
		t.Fatalf("completed end_turn = %#v, want false for a tool turn", completed["end_turn"])
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(4) {
		t.Fatalf("usage = %#v", usage)
	}
	if replay.MessageFields == nil || replay.ToolFields["call_patch"] == nil {
		t.Fatalf("replay state lost message/tool metadata: %#v", replay)
	}
}

func TestResponsesChatProxyRoundTrip(t *testing.T) {
	upstreamRequests := make(chan map[string]any, 1)
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
		upstreamRequests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`data: {"id":"chatcmpl_proxy","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			``,
			`data: {"id":"chatcmpl_proxy","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: {"id":"chatcmpl_proxy","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newResponsesChatProxy(upstream.URL, "provider-model", http.DefaultClient, responsesChatProxyOptions{
		Label:               "Test Provider",
		SendReasoningEffort: true,
		IncludeStreamUsage:  true,
	}))
	defer proxy.Close()

	requestBody := `{
	  "model":"routed-model",
	  "instructions":"You are Codex.",
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
	  "tools":[{"type":"function","name":"shell_command","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}],
	  "tool_choice":"auto",
	  "parallel_tool_calls":true,
	  "reasoning":{"effort":"high","summary":"auto"},
	  "stream":true
	}`
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(requestBody))
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
	rawResponse, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.StatusCode, rawResponse)
	}
	if !bytes.Contains(rawResponse, []byte(`"type":"response.output_text.delta"`)) ||
		!bytes.Contains(rawResponse, []byte(`"type":"response.completed"`)) {
		t.Fatalf("unexpected Responses stream:\n%s", rawResponse)
	}

	upstreamBody := <-upstreamRequests
	if upstreamBody["model"] != "provider-model" || upstreamBody["reasoning_effort"] != "high" {
		t.Fatalf("upstream routing/reasoning = %#v", upstreamBody)
	}
	messages := upstreamBody["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("upstream messages = %#v", messages)
	}
	if _, ok := upstreamBody["stream_options"]; !ok {
		t.Fatalf("upstream stream usage was not requested: %#v", upstreamBody)
	}
}

func decodeTestSSE(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode SSE line %q: %v", line, err)
		}
		out = append(out, event)
	}
	return out
}
