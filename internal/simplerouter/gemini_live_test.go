package simplerouter

// Live smoke tests against the real Gemini API. Skipped unless
// GEMINI_LIVE_KEY is set. Temporary: used to verify the proxy end-to-end
// with a real key during development.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func liveProxy(t *testing.T) (string, string) {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("GEMINI_LIVE_KEY"))
	if key == "" {
		t.Skip("GEMINI_LIVE_KEY not set")
	}
	baseURL, stop, err := startGeminiProxy(defaultGeminiAPIBase, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return baseURL, key
}

func livePost(t *testing.T, baseURL, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func TestLiveSimpleMessage(t *testing.T) {
	baseURL, key := liveProxy(t)
	resp, body := livePost(t, baseURL, key,
		`{"model":"gemini-2.5-flash","max_tokens":256,"messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "pong") {
		t.Errorf("body = %s", body)
	}
	t.Logf("non-streaming: %s", body)
}

func TestLiveStreamingWithThinking(t *testing.T) {
	baseURL, key := liveProxy(t)
	resp, body := livePost(t, baseURL, key,
		`{"model":"gemini-2.5-flash","max_tokens":2048,"stream":true,
		  "thinking":{"type":"enabled","budget_tokens":1024},
		  "messages":[{"role":"user","content":"What is 17*23? Answer with just the number."}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"message_start", "message_stop", "391"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
	t.Logf("streaming events:\n%s", body)
}

func TestLiveToolRoundTripWithSignatureEcho(t *testing.T) {
	baseURL, key := liveProxy(t)

	// Turn 1: force a tool call (thinking on so a signature is issued).
	turn1 := `{"model":"gemini-2.5-flash","max_tokens":2048,
	  "thinking":{"type":"enabled","budget_tokens":1024},
	  "tools":[{"name":"get_weather","description":"Get current weather for a city",
	    "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
	  "messages":[{"role":"user","content":"What is the weather in London? Use the tool."}]}`
	resp, body := livePost(t, baseURL, key, turn1)
	if resp.StatusCode != 200 {
		t.Fatalf("turn1 status %d: %s", resp.StatusCode, body)
	}
	var msg struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("turn1 not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("turn1 stop_reason = %q\n%s", msg.StopReason, body)
	}
	var toolID string
	var assistantContent []map[string]any
	for _, block := range msg.Content {
		assistantContent = append(assistantContent, block)
		if block["type"] == "tool_use" {
			toolID = block["id"].(string)
		}
	}
	if toolID == "" {
		t.Fatalf("no tool_use in turn1: %s", body)
	}
	t.Logf("turn1 tool id: %s, content blocks: %d", toolID, len(msg.Content))

	// Turn 2: replay the assistant turn (thinking + tool_use, exactly as
	// Claude Code would) plus the tool result. The proxy must re-attach the
	// stored signature or Gemini rejects the request.
	assistantJSON, _ := json.Marshal(assistantContent)
	turn2 := `{"model":"gemini-2.5-flash","max_tokens":2048,
	  "thinking":{"type":"enabled","budget_tokens":1024},
	  "tools":[{"name":"get_weather","description":"Get current weather for a city",
	    "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
	  "messages":[
	    {"role":"user","content":"What is the weather in London? Use the tool."},
	    {"role":"assistant","content":` + string(assistantJSON) + `},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"22C and sunny"}]}
	  ]}`
	resp, body = livePost(t, baseURL, key, turn2)
	if resp.StatusCode != 200 {
		t.Fatalf("turn2 (signature echo) status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "22") {
		t.Errorf("turn2 answer missing tool data: %s", body)
	}
	t.Logf("turn2 (real signature): %s", body)

	// Turn 3: same history but with an unknown tool_use id — simulates a
	// resumed session where the store is empty; the dummy signature must be
	// accepted.
	turn3 := strings.ReplaceAll(turn2, toolID, "toolu_unknown_resumed")
	resp, body = livePost(t, baseURL, key, turn3)
	if resp.StatusCode != 200 {
		t.Fatalf("turn3 (dummy signature) status %d: %s", resp.StatusCode, body)
	}
	t.Logf("turn3 (dummy signature): %s", body)
}

func TestLiveCountTokens(t *testing.T) {
	baseURL, key := liveProxy(t)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/messages/count_tokens", strings.NewReader(
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"count these tokens please"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var counted map[string]int
	if err := json.Unmarshal(out, &counted); err != nil || counted["input_tokens"] <= 0 {
		t.Fatalf("count_tokens = %s (err %v)", out, err)
	}
	t.Logf("count_tokens: %s", out)
}
