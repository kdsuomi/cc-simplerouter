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

func TestResponsesToolsToGeminiInteractionsPreservesCodexToolsAndSearch(t *testing.T) {
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
		  "tools":[{
		    "type":"function",
		    "name":"spawn_agent",
		    "description":"Spawn an agent",
		    "parameters":{"type":"object","properties":{"message":{"type":"string"}}}
		  }]
		}`),
		json.RawMessage(`{"type":"web_search"}`),
	}

	tools, registry, err := responsesToolsToGeminiInteractions(raw, "gemini-3.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("Gemini 3 tools = %d, want 3 functions + Google Search: %#v", len(tools), tools)
	}
	if tools[3].(map[string]any)["type"] != "google_search" || !registry.webSearch {
		t.Fatalf("Google Search mapping = %#v, registry = %#v", tools[3], registry)
	}
	if identity := registry.byChatName["collaboration__spawn_agent"]; identity.Namespace != "collaboration" {
		t.Fatalf("namespace identity = %#v", identity)
	}
	custom := tools[1].(map[string]any)
	properties := custom["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := properties["input"]; !ok {
		t.Fatalf("custom tool parameters = %#v", custom["parameters"])
	}

	olderTools, _, err := responsesToolsToGeminiInteractions(raw, "gemini-2.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if len(olderTools) != 3 {
		t.Fatalf("Gemini 2.5 mixed tools = %d, want search omitted per provider constraint", len(olderTools))
	}
	searchOnly, _, err := responsesToolsToGeminiInteractions(
		[]json.RawMessage{json.RawMessage(`{"type":"web_search"}`)},
		"gemini-2.5-flash",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(searchOnly) != 1 || searchOnly[0].(map[string]any)["type"] != "google_search" {
		t.Fatalf("Gemini 2.5 search-only tools = %#v", searchOnly)
	}
}

func TestGeminiInteractionFunctionAlwaysSendsParametersSchema(t *testing.T) {
	// Codex ships parameterless tools as empty object schemas. After scrubbing,
	// Interactions still requires a parameters object (or boolean).
	cases := []struct {
		name       string
		parameters json.RawMessage
	}{
		{"missing", nil},
		{"null", json.RawMessage(`null`)},
		{"empty object", json.RawMessage(`{}`)},
		{"empty properties", json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{"type object only", json.RawMessage(`{"type":"object"}`)},
		{"invalid json", json.RawMessage(`not-json`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := geminiInteractionFunction("get_context_remaining", "remaining tokens", tc.parameters)
			params, ok := tool["parameters"].(map[string]any)
			if !ok {
				t.Fatalf("parameters missing or not an object: %#v", tool)
			}
			if params["type"] != "object" {
				t.Fatalf("parameters.type = %#v, want object", params["type"])
			}
			props, _ := params["properties"].(map[string]any)
			if props == nil {
				t.Fatalf("parameters.properties missing: %#v", params)
			}
			if _, hasAdditional := params["additionalProperties"]; hasAdditional {
				t.Fatalf("additionalProperties should be scrubbed: %#v", params)
			}
		})
	}

	// Non-empty schemas still scrub and preserve useful fields.
	tool := geminiInteractionFunction("shell_command", "run", json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{"command":{"type":"string","description":"cmd"}},
		"required":["command"]
	}`))
	params := tool["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if props["command"].(map[string]any)["type"] != "string" {
		t.Fatalf("shell parameters not preserved: %#v", params)
	}
	if _, ok := params["additionalProperties"]; ok {
		t.Fatalf("additionalProperties should be scrubbed: %#v", params)
	}
}

func TestGeminiInteractionGenerationConfigMapsReasoning(t *testing.T) {
	req := &responsesRequest{
		Reasoning:       &responsesReasoning{Effort: "minimal", Summary: "auto"},
		MaxOutputTokens: 1234,
		ToolChoice:      json.RawMessage(`"required"`),
	}
	pro := geminiInteractionGenerationConfig("gemini-3.1-pro-preview", req, false)
	if pro["thinking_level"] != "low" || pro["thinking_summaries"] != "auto" {
		t.Fatalf("Pro config = %#v", pro)
	}
	if pro["max_output_tokens"] != 1234 || pro["tool_choice"] != "any" {
		t.Fatalf("generation config = %#v", pro)
	}

	disabled := geminiInteractionGenerationConfig("gemini-3.5-flash", req, true)
	if disabled["thinking_level"] != "minimal" || disabled["thinking_summaries"] != "none" {
		t.Fatalf("disabled config = %#v", disabled)
	}
}

func TestResponsesInputToGeminiInteractionsRestoresExactReplaySteps(t *testing.T) {
	replaySteps := []json.RawMessage{
		json.RawMessage(`{"type":"thought","summary":[{"type":"text","text":"plan"}],"signature":"signed-thought"}`),
		json.RawMessage(`{"type":"model_output","content":[{"type":"text","text":"Working."}]}`),
		json.RawMessage(`{"type":"function_call","id":"call_patch","name":"apply_patch","arguments":{"input":"patch"}}`),
	}
	encoded := encodeGeminiInteractionReplay(geminiInteractionReplayState{Steps: replaySteps})
	req := &responsesRequest{
		Input: []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"type":"reasoning","encrypted_content":%q}`, encoded)),
			json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"duplicate"}]}`),
			json.RawMessage(`{"type":"custom_tool_call","name":"apply_patch","call_id":"call_patch","input":"duplicate"}`),
			json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_patch","output":"Done"}`),
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`),
		},
	}
	_, registry, err := responsesToolsToGeminiInteractions(
		[]json.RawMessage{json.RawMessage(`{"type":"custom","name":"apply_patch"}`)},
		"gemini-3.5-flash",
	)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := responsesInputToGeminiInteractions(req, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 5 {
		t.Fatalf("steps = %d, want replay 3 + result + user: %#v", len(steps), steps)
	}
	for i, want := range replaySteps {
		got, err := json.Marshal(steps[i])
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		_ = json.Unmarshal(got, &gotValue)
		_ = json.Unmarshal(want, &wantValue)
		if fmt.Sprint(gotValue) != fmt.Sprint(wantValue) {
			t.Fatalf("replay step %d = %s, want %s", i, got, want)
		}
	}
	result := steps[3].(map[string]any)
	if result["type"] != "function_result" || result["call_id"] != "call_patch" || result["name"] != "apply_patch" {
		t.Fatalf("function result = %#v", result)
	}
	if user := steps[4].(map[string]any); user["type"] != "user_input" {
		t.Fatalf("user step = %#v", user)
	}
}

func TestGeminiInteractionsStreamTranslatorPreservesStepsAndCodexItems(t *testing.T) {
	_, registry, err := responsesToolsToGeminiInteractions([]json.RawMessage{
		json.RawMessage(`{"type":"custom","name":"apply_patch"}`),
		json.RawMessage(`{"type":"web_search"}`),
	}, "gemini-3.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	translator := newGeminiInteractionsResponsesTranslator(&output, nil, "gemini-3.5-flash", registry)
	events := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_test","status":"in_progress","model":"gemini-3.5-flash"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"thought","summary":[{"type":"text","text":"plan "}]}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"more"}}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"signed"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"step.start","index":1,"step":{"type":"google_search_call","id":"gs_1","arguments":{"queries":["current docs"]},"signature":"search-sig"}}`,
		`{"event_type":"step.stop","index":1}`,
		`{"event_type":"step.start","index":2,"step":{"type":"google_search_result","call_id":"gs_1","result":[{"url":"https://example.com"}],"signature":"result-sig"}}`,
		`{"event_type":"step.stop","index":2}`,
		`{"event_type":"step.start","index":3,"step":{"type":"model_output","content":[{"type":"text","text":"Hello "}]}}`,
		`{"event_type":"step.delta","index":3,"delta":{"type":"text","text":"world"}}`,
		`{"event_type":"step.delta","index":3,"delta":{"type":"text_annotation_delta","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":11}]}}`,
		`{"event_type":"step.stop","index":3}`,
		`{"event_type":"step.start","index":4,"step":{"type":"function_call","id":"call_patch","name":"apply_patch"}}`,
		`{"event_type":"step.delta","index":4,"delta":{"type":"arguments_delta","arguments":"{\"input\":\"patch\"}"}}`,
		`{"event_type":"step.stop","index":4}`,
		`{"event_type":"interaction.requires_action","interaction":{"id":"v1_test","status":"requires_action","model":"gemini-3.5-flash","usage":{"total_input_tokens":10,"total_output_tokens":4,"total_thought_tokens":2,"total_cached_tokens":3,"total_tokens":16}}}`,
	}
	for _, event := range events {
		if err := translator.onEvent(json.RawMessage(event)); err != nil {
			t.Fatal(err)
		}
	}
	translator.finish()

	decoded := decodeTestSSE(t, output.String())
	var sawThought, sawSearch bool
	var message, custom, completed map[string]any
	var replay geminiInteractionReplayState
	for _, event := range decoded {
		switch event["type"] {
		case "response.reasoning_summary_text.done":
			if event["text"] == "plan " || event["text"] == "more" {
				sawThought = true
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			switch item["type"] {
			case "web_search_call":
				sawSearch = item["id"] == "gs_1"
			case "message":
				message = item
			case "custom_tool_call":
				custom = item
			case "reasoning":
				if encrypted, _ := item["encrypted_content"].(string); encrypted != "" {
					replay, _ = decodeGeminiInteractionReplay(encrypted)
				}
			}
		case "response.completed":
			completed = event["response"].(map[string]any)
		}
	}
	if !sawThought || !sawSearch {
		t.Fatalf("thought/search events missing:\n%s", output.String())
	}
	content := message["content"].([]any)[0].(map[string]any)
	if content["text"] != "Hello world" {
		t.Fatalf("message = %#v", message)
	}
	if custom["name"] != "apply_patch" || custom["input"] != "patch" {
		t.Fatalf("custom call = %#v", custom)
	}
	if len(replay.Steps) != 5 {
		t.Fatalf("replay steps = %d, want 5: %#v", len(replay.Steps), replay)
	}
	var thought map[string]any
	if err := json.Unmarshal(replay.Steps[0], &thought); err != nil {
		t.Fatal(err)
	}
	if thought["signature"] != "signed" {
		t.Fatalf("thought replay = %#v", thought)
	}
	var modelOutput map[string]any
	if err := json.Unmarshal(replay.Steps[3], &modelOutput); err != nil {
		t.Fatal(err)
	}
	replayedText := modelOutput["content"].([]any)[0].(map[string]any)
	annotations := replayedText["annotations"].([]any)
	if len(annotations) != 1 || annotations[0].(map[string]any)["url"] != "https://example.com" {
		t.Fatalf("citation annotations were not preserved in replay: %#v", replayedText)
	}
	if completed["end_turn"] != false {
		t.Fatalf("completed end_turn = %#v", completed["end_turn"])
	}
	usage := completed["usage"].(map[string]any)
	if usage["output_tokens"] != float64(6) ||
		usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestGeminiInteractionsStreamTranslatorMapsTerminalStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        string
		wantEvent     string
		wantErrorCode string
	}{
		{name: "incomplete", status: "incomplete", wantEvent: "response.incomplete"},
		{name: "failed", status: "failed", wantEvent: "response.failed", wantErrorCode: "upstream_error"},
		{name: "cancelled", status: "cancelled", wantEvent: "response.failed", wantErrorCode: "cancelled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			translator := newGeminiInteractionsResponsesTranslator(&output, nil, "gemini-3.6-flash", &responseToolRegistry{})
			events := []string{
				`{"event_type":"interaction.created","interaction":{"id":"v1_terminal","status":"in_progress"}}`,
				`{"event_type":"step.start","index":0,"step":{"type":"model_output","content":[{"type":"text","text":"partial"}]}}`,
				`{"event_type":"step.stop","index":0}`,
				fmt.Sprintf(`{"event_type":"interaction.completed","interaction":{"id":"v1_terminal","status":%q}}`, test.status),
			}
			for _, event := range events {
				if err := translator.onEvent(json.RawMessage(event)); err != nil {
					t.Fatal(err)
				}
			}
			translator.finish()

			var terminal map[string]any
			for _, event := range decodeTestSSE(t, output.String()) {
				if event["type"] == test.wantEvent {
					terminal = event["response"].(map[string]any)
				}
			}
			if terminal == nil {
				t.Fatalf("missing %s:\n%s", test.wantEvent, output.String())
			}
			if test.wantEvent == "response.incomplete" {
				details := terminal["incomplete_details"].(map[string]any)
				if details["reason"] != "max_output_tokens" {
					t.Fatalf("incomplete details = %#v", details)
				}
			} else {
				gotError := terminal["error"].(map[string]any)
				if gotError["code"] != test.wantErrorCode {
					t.Fatalf("error = %#v", gotError)
				}
			}
		})
	}
}

func TestGeminiInteractionsStreamTranslatorRejectsMissingTerminalEvent(t *testing.T) {
	var output bytes.Buffer
	translator := newGeminiInteractionsResponsesTranslator(&output, nil, "gemini-3.6-flash", &responseToolRegistry{})
	if err := translator.onEvent(json.RawMessage(`{"event_type":"interaction.created","interaction":{"id":"v1_truncated","status":"in_progress"}}`)); err != nil {
		t.Fatal(err)
	}
	translator.finish()

	for _, event := range decodeTestSSE(t, output.String()) {
		if event["type"] == "response.failed" {
			response := event["response"].(map[string]any)
			gotError := response["error"].(map[string]any)
			if gotError["code"] != "stream_error" {
				t.Fatalf("error = %#v", gotError)
			}
			return
		}
	}
	t.Fatalf("missing response.failed:\n%s", output.String())
}

func TestGeminiInteractionsProxyRoundTrip(t *testing.T) {
	requests := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions" || r.URL.Query().Get("alt") != "sse" {
			http.Error(w, "unexpected URL "+r.URL.String(), http.StatusNotFound)
			return
		}
		if r.Header.Get("x-goog-api-key") != "gemini-key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: interaction.created`,
			`data: {"event_type":"interaction.created","interaction":{"id":"v1_proxy","status":"in_progress","model":"gemini-3.5-flash"}}`,
			``,
			`event: step.start`,
			`data: {"event_type":"step.start","index":0,"step":{"type":"model_output","content":[{"type":"text","text":"Gemini"}]}}`,
			``,
			`event: step.stop`,
			`data: {"event_type":"step.stop","index":0}`,
			``,
			`event: interaction.completed`,
			`data: {"event_type":"interaction.completed","interaction":{"id":"v1_proxy","status":"completed","usage":{"total_input_tokens":2,"total_output_tokens":1,"total_tokens":3}}}`,
			``,
			`event: done`,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newGeminiInteractionsProxy(upstream.URL, "gemini-3.5-flash", upstream.Client(), false))
	defer proxy.Close()
	requestBody := `{
	  "model":"ignored",
	  "instructions":"You are Codex.",
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
	  "tools":[
	    {"type":"function","name":"shell_command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}},
	    {"type":"web_search"}
	  ],
	  "tool_choice":"auto",
	  "reasoning":{"effort":"medium","summary":"auto"},
	  "stream":true
	}`
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer gemini-key")
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
	if resp.StatusCode != http.StatusOK ||
		!bytes.Contains(rawResponse, []byte(`"type":"response.output_text.delta"`)) ||
		!bytes.Contains(rawResponse, []byte(`"type":"response.completed"`)) {
		t.Fatalf("status = %d, response:\n%s", resp.StatusCode, rawResponse)
	}

	body := <-requests
	if body["model"] != "gemini-3.5-flash" || body["store"] != false || body["system_instruction"] != "You are Codex." {
		t.Fatalf("Gemini request = %#v", body)
	}
	tools := body["tools"].([]any)
	if len(tools) != 2 || tools[1].(map[string]any)["type"] != "google_search" {
		t.Fatalf("Gemini tools = %#v", tools)
	}
	config := body["generation_config"].(map[string]any)
	if config["thinking_level"] != "medium" || config["thinking_summaries"] != "auto" {
		t.Fatalf("generation config = %#v", config)
	}
}
