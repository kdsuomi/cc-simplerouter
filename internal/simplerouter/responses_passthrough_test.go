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
