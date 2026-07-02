package simplerouter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiProxyStreamingEndToEnd(t *testing.T) {
	var gotPath, gotQuery, gotKey string
	var gotBody geminiRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("x-goog-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3}}`+"\n\n")
	}))
	defer upstream.Close()

	baseURL, stop, err := startGeminiProxy(upstream.URL, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Claude Code echoes the [1m]-suffixed model in the body; the proxy must strip it.
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(
		`{"model":"gemini-2.5-flash[1m]","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-gemini-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotPath != "/models/gemini-2.5-flash:streamGenerateContent" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotQuery != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", gotQuery)
	}
	if gotKey != "test-gemini-key" {
		t.Errorf("x-goog-api-key = %q", gotKey)
	}
	if len(gotBody.Contents) != 1 || gotBody.Contents[0].Parts[0].Text != "hi" {
		t.Errorf("translated body = %+v", gotBody.Contents)
	}
	if gotBody.GenerationConfig.MaxOutputTokens != 64 {
		t.Errorf("maxOutputTokens = %d", gotBody.GenerationConfig.MaxOutputTokens)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	out, _ := io.ReadAll(resp.Body)
	events := parseSSE(t, string(out))
	names := strings.Join(eventNames(events), ",")
	want := "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if names != want {
		t.Errorf("events = %s\nwant %s", names, want)
	}
	if events[2].Data["delta"].(map[string]any)["text"] != "hi there" {
		t.Errorf("text delta = %+v", events[2].Data)
	}
}

func TestGeminiProxyNonStreamingRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			t.Errorf("non-streaming should hit generateContent, got %q", r.URL.Path)
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"get_weather","args":{"city":"London"}},"thoughtSignature":"c2ln"}
		]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`)
	}))
	defer upstream.Close()

	baseURL, stop, err := startGeminiProxy(upstream.URL, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"models/gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"weather?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	content := msg["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "get_weather" {
		t.Errorf("content = %+v", content)
	}
	if !strings.HasPrefix(block["id"].(string), "toolu_") {
		t.Errorf("tool id = %v", block["id"])
	}
}

func TestGeminiProxyUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":429,"message":"quota exhausted","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer upstream.Close()

	baseURL, stop, err := startGeminiProxy(upstream.URL, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"gemini-2.5-flash","max_tokens":8,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var errBody map[string]any
	json.NewDecoder(resp.Body).Decode(&errBody)
	errObj := errBody["error"].(map[string]any)
	if errBody["type"] != "error" || errObj["type"] != "rate_limit_error" || errObj["message"] != "quota exhausted" {
		t.Errorf("error body = %+v", errBody)
	}
}

func TestGeminiProxyCountTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":countTokens") {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"totalTokens": 123}`)
	}))
	defer upstream.Close()

	baseURL, stop, err := startGeminiProxy(upstream.URL, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Post(baseURL+"/v1/messages/count_tokens", "application/json", strings.NewReader(
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"count me"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]int
	json.NewDecoder(resp.Body).Decode(&out)
	if out["input_tokens"] != 123 {
		t.Errorf("input_tokens = %d", out["input_tokens"])
	}
}

func TestGeminiProxyCountTokensFallsBackToEstimate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	baseURL, stop, err := startGeminiProxy(upstream.URL, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"estimate me please"}]}`
	resp, err := http.Post(baseURL+"/v1/messages/count_tokens", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count_tokens must never fail, got %d", resp.StatusCode)
	}
	var out map[string]int
	json.NewDecoder(resp.Body).Decode(&out)
	if out["input_tokens"] != len(body)/4 {
		t.Errorf("estimate = %d, want %d", out["input_tokens"], len(body)/4)
	}
}

func TestGeminiProxyModelsAndUnknownRoutes(t *testing.T) {
	baseURL, stop, err := startGeminiProxy("http://127.0.0.1:1", "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Get(baseURL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var models map[string]any
	json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	first := models["data"].([]any)[0].(map[string]any)
	if first["id"] != "gemini-2.5-flash" {
		t.Errorf("models = %+v", models)
	}

	resp, err = http.Get(baseURL + "/v1/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route status = %d", resp.StatusCode)
	}
}

func TestGeminiProxySignatureRoundTripAcrossRequests(t *testing.T) {
	// Turn 1 returns a functionCall with a signature; turn 2 replays the
	// tool_use and must carry the exact signature back.
	var secondBody geminiRequest
	call := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[
				{"functionCall":{"name":"get_weather","args":{"city":"London"}},"thoughtSignature":"cm91bmR0cmlw"}
			]},"finishReason":"STOP"}]}`)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&secondBody)
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"22C"}]},"finishReason":"STOP"}]}`)
	}))
	defer upstream.Close()

	baseURL, stop, err := startGeminiProxy(upstream.URL, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"gemini-2.5-flash","max_tokens":8,"messages":[{"role":"user","content":"weather?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()
	toolID := first["content"].([]any)[0].(map[string]any)["id"].(string)

	turn2 := `{"model":"gemini-2.5-flash","max_tokens":8,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"` + toolID + `","name":"get_weather","input":{"city":"London"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"22C"}]}
	]}`
	resp, err = http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(turn2))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(secondBody.Contents) != 3 {
		t.Fatalf("contents = %+v", secondBody.Contents)
	}
	fc := secondBody.Contents[1].Parts[0]
	if fc.FunctionCall == nil || fc.ThoughtSignature != "cm91bmR0cmlw" {
		t.Errorf("replayed functionCall = %+v, want exact signature echo", fc)
	}
	fr := secondBody.Contents[2].Parts[0]
	if fr.FunctionResponse == nil || fr.FunctionResponse.Name != "get_weather" {
		t.Errorf("functionResponse = %+v, want name recovered from store", fr)
	}
}
