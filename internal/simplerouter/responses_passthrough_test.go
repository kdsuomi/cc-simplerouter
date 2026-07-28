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
