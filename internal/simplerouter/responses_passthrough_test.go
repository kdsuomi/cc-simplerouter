package simplerouter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesPassthroughPinsOpenRouterEndpointAndRelaysStream(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer openrouter-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "request-123")
		w.Header().Set("X-Generation-ID", "generation-456")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newResponsesPassthroughProxy(
		upstream.URL,
		"author/model",
		http.DefaultClient,
		responsesPassthroughOptions{Label: "OpenRouter", ProviderTag: "deepinfra/fp4"},
	))
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(`{
	  "model":"wrong",
	  "input":[],
	  "provider":{"sort":"latency"},
	  "stream":true
	}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer openrouter-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"type":"response.created"`) {
		t.Fatalf("relayed response = %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-ID") != "request-123" {
		t.Fatalf("request id header = %q", resp.Header.Get("X-Request-ID"))
	}
	if resp.Header.Get("X-Generation-ID") != "generation-456" {
		t.Fatalf("generation id header = %q", resp.Header.Get("X-Generation-ID"))
	}

	request := <-captured
	if request["model"] != "author/model" {
		t.Fatalf("model = %#v", request["model"])
	}
	provider := request["provider"].(map[string]any)
	only := provider["only"].([]any)
	if len(only) != 1 || only[0] != "deepinfra/fp4" || provider["allow_fallbacks"] != false {
		t.Fatalf("provider routing = %#v", provider)
	}
	if provider["sort"] != "latency" {
		t.Fatalf("existing provider options were not preserved: %#v", provider)
	}
}

func TestResponsesPassthroughRetriesJSONSchemaAsJSONObjectAndRemembersCapability(t *testing.T) {
	requests := make(chan map[string]any, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- body
		format := body["text"].(map[string]any)["format"].(map[string]any)
		if format["type"] == "json_schema" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Provider returned error","metadata":{"raw":"Model does not support 'json_schema' response format. Supported formats: json_object."}}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\"}}\n\n")
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newResponsesPassthroughProxy(
		upstream.URL,
		"deepseek/deepseek-v4-flash",
		upstream.Client(),
		responsesPassthroughOptions{Label: "OpenRouter"},
	))
	defer proxy.Close()

	requestBody := `{
	  "model":"wrong",
	  "instructions":"Return the decision.",
	  "input":[],
	  "stream":true,
	  "text":{"format":{"type":"json_schema","name":"decision","strict":true,"schema":{"type":"object","required":["outcome"]}}}
	}`
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), `"id":"resp_2"`) {
		t.Fatalf("response = %d %s", resp.StatusCode, responseBody)
	}

	first := <-requests
	second := <-requests
	firstFormat := first["text"].(map[string]any)["format"].(map[string]any)
	secondFormat := second["text"].(map[string]any)["format"].(map[string]any)
	if firstFormat["type"] != "json_schema" || secondFormat["type"] != "json_object" || len(secondFormat) != 1 {
		t.Fatalf("formats = first %#v, second %#v", firstFormat, secondFormat)
	}
	if instructions := second["instructions"].(string); !strings.Contains(instructions, `"required":["outcome"]`) {
		t.Fatalf("fallback instructions lost schema: %q", instructions)
	}

	secondReq, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	secondResp, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	defer secondResp.Body.Close()
	if _, err := io.Copy(io.Discard, secondResp.Body); err != nil {
		t.Fatal(err)
	}
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("second response status = %d", secondResp.StatusCode)
	}
	third := <-requests
	thirdFormat := third["text"].(map[string]any)["format"].(map[string]any)
	if thirdFormat["type"] != "json_object" {
		t.Fatalf("remembered format = %#v", thirdFormat)
	}
	select {
	case extra := <-requests:
		t.Fatalf("remembered fallback unexpectedly probed json_schema again: %#v", extra)
	default:
	}
}

func TestResponsesPassthroughAppliesMetaCompatibilityRewrites(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_meta","tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object"}},{"type":"function","name":"shell_command","parameters":{"type":"object"}}]}}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_patch","call_id":"call_patch","name":"apply_patch","arguments":"","status":"in_progress"}}`,
			``,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_patch","output_index":0,"delta":"{\"input\":\"*** Begin"}`,
			``,
			`event: response.function_call_arguments.done`,
			`data: {"type":"response.function_call_arguments.done","item_id":"fc_patch","output_index":0,"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_patch","call_id":"call_patch","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}","status":"completed"}}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_shell","call_id":"call_shell","name":"shell_command","arguments":"","status":"in_progress"}}`,
			``,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_shell","output_index":1,"delta":"{\"command\":\"pwd\"}"}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_shell","call_id":"call_shell","name":"shell_command","arguments":"{\"command\":\"pwd\"}","status":"completed"}}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_meta","output":[{"type":"function_call","id":"fc_patch","call_id":"call_patch","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}","status":"completed"},{"type":"function_call","id":"fc_shell","call_id":"call_shell","name":"shell_command","arguments":"{\"command\":\"pwd\"}","status":"completed"}],"tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object"}},{"type":"function","name":"shell_command","parameters":{"type":"object"}}]}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newResponsesPassthroughProxy(
		upstream.URL,
		"muse-spark-1.1",
		upstream.Client(),
		metaResponsesOptions(),
	))
	defer proxy.Close()

	requestBody := `{
	  "model":"wrong",
	  "input":[
	    {
	      "type":"custom_tool_call",
	      "id":"ctc_prior",
	      "call_id":"call_prior",
	      "name":"apply_patch",
	      "input":"*** Begin Patch\n*** End Patch",
	      "internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}
	    },
	    {
	      "type":"custom_tool_call_output",
	      "call_id":"call_prior",
	      "name":"apply_patch",
	      "output":"Done",
	      "internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}
	    }
	  ],
	  "stream":true,
	  "reasoning":{"effort":"max","summary":"auto"},
	  "tools":[
	    {
	      "type":"custom",
	      "name":"apply_patch",
	      "description":"Apply a patch",
	      "strict":false,
	      "format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"}
	    },
	    {
	      "type":"function",
	      "name":"shell_command",
	      "strict":true,
	      "parameters":{"type":"object","properties":{"command":{"type":"string"},"limit":{"type":"integer"}},"required":["command"]},
	      "format":{"preserve":true}
	    },
	    {
	      "type":"namespace",
	      "name":"collaboration",
	      "tools":[{
	        "type":"function",
	        "name":"list_agents",
	        "strict":true,
	        "parameters":{"type":"object","properties":{"limit":{"type":"integer"}}}
	      }]
	    },
	    {
	      "type":"tool_search",
	      "execution":"client",
	      "parameters":{
	        "type":"object",
	        "properties":{"query":{"type":"string"},"limit":{"type":"number"}},
	        "required":["query"],
	        "additionalProperties":false
	      }
	    },
	    {
	      "type":"web_search",
	      "external_web_access":false,
	      "search_content_types":["text","image"]
	    }
	  ]
	}`
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	} else if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy response = %d %s", resp.StatusCode, responseBody)
	}

	request := <-captured
	if request["model"] != "muse-spark-1.1" {
		t.Fatalf("model = %#v", request["model"])
	}
	reasoning := request["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	tools := request["tools"].([]any)
	custom := tools[0].(map[string]any)
	if _, found := custom["format"]; found || custom["type"] != "function" || custom["name"] != "apply_patch" || custom["strict"] != false {
		t.Fatalf("translated custom tool = %#v", custom)
	}
	parameters := custom["parameters"].(map[string]any)
	inputProperty := parameters["properties"].(map[string]any)["input"].(map[string]any)
	if parameters["additionalProperties"] != false || !strings.Contains(inputProperty["description"].(string), "start: PATCH") {
		t.Fatalf("translated custom parameters = %#v", parameters)
	}
	function := tools[1].(map[string]any)
	if _, found := function["format"]; !found {
		t.Fatalf("function tool was unexpectedly rewritten: %#v", function)
	}
	if function["strict"] != false {
		t.Fatalf("strict function was not relaxed for Meta: %#v", function)
	}
	namespace := tools[2].(map[string]any)
	child := namespace["tools"].([]any)[0].(map[string]any)
	if child["strict"] != false {
		t.Fatalf("strict namespace function was not relaxed for Meta: %#v", child)
	}
	toolSearch := tools[3].(map[string]any)
	toolSearchParameters := toolSearch["parameters"].(map[string]any)
	toolSearchProperties := toolSearchParameters["properties"].(map[string]any)
	if _, found := toolSearchProperties["limit"]; found || toolSearchProperties["query"] == nil {
		t.Fatalf("optional tool_search parameter was not omitted for Meta: %#v", toolSearchParameters)
	}
	webSearch := tools[4].(map[string]any)
	if _, found := webSearch["search_content_types"]; found || webSearch["external_web_access"] != false {
		t.Fatalf("Meta web_search compatibility = %#v", webSearch)
	}
	input := request["input"].([]any)
	priorCall := input[0].(map[string]any)
	var arguments map[string]string
	if err := json.Unmarshal([]byte(priorCall["arguments"].(string)), &arguments); err != nil {
		t.Fatal(err)
	}
	if priorCall["type"] != "function_call" || arguments["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("translated prior call = %#v", priorCall)
	}
	if _, found := priorCall["internal_chat_message_metadata_passthrough"]; found {
		t.Fatalf("prior call retained internal metadata: %#v", priorCall)
	}
	priorOutput := input[1].(map[string]any)
	if priorOutput["type"] != "function_call_output" {
		t.Fatalf("translated prior output = %#v", priorOutput)
	}
	if _, found := priorOutput["name"]; found {
		t.Fatalf("prior output retained custom tool name: %#v", priorOutput)
	}

	stream := string(responseBody)
	for _, want := range []string{
		`event: response.custom_tool_call_input.delta`,
		`"type":"custom_tool_call"`,
		`"input":"*** Begin Patch\n*** End Patch"`,
		`"name":"shell_command"`,
		`event: response.function_call_arguments.delta`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("translated stream missing %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, `"item_id":"fc_patch","output_index":0,"delta":"{\"input\"`) {
		t.Fatalf("custom function argument envelope leaked downstream:\n%s", stream)
	}
}
