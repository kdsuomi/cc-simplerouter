package simplerouter

// Live smoke tests against the Meta Model API. Skipped unless
// SIMPLEROUTER_LIVE_META is set. The key comes from META_API_KEY (or
// MODEL_API_KEY) or, failing that, the key saved in the simplerouter config.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func metaLiveProxy(t *testing.T) (string, string) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_LIVE_META")) == "" {
		t.Skip("SIMPLEROUTER_LIVE_META not set")
	}
	key := cleanAPIKey(os.Getenv("META_API_KEY"))
	if key == "" {
		key = cleanAPIKey(os.Getenv("MODEL_API_KEY"))
	}
	if key == "" {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		key = cfg.MetaAPIKey
	}
	if key == "" {
		t.Skip("no Meta key in env or config")
	}
	baseURL, stop, err := startMetaProxy(defaultMetaAPIBase, "muse-spark-1.1", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return baseURL, key
}

func metaLivePost(t *testing.T, baseURL, key, path, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
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

func TestMetaLiveNonStreaming(t *testing.T) {
	baseURL, key := metaLiveProxy(t)
	resp, body := metaLivePost(t, baseURL, key, "/v1/messages",
		`{"model":"muse-spark-1.1[1m]","max_tokens":300,"stop_sequences":["never"],"top_k":5,"messages":[{"role":"user","content":"Reply with exactly: pong"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var msg struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	if msg.Type != "message" || msg.StopReason != "end_turn" {
		t.Fatalf("unexpected message: %s", body)
	}
	var text string
	for _, b := range msg.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Errorf("text = %q", text)
	}
}

func TestMetaLiveStreamingWithTools(t *testing.T) {
	baseURL, key := metaLiveProxy(t)
	resp, body := metaLivePost(t, baseURL, key, "/v1/messages",
		`{"model":"muse-spark-1.1","max_tokens":2000,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"tools":[{"name":"get_weather","description":"Get weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"event: message_start", "tool_use", "get_weather", `"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
}

func TestMetaLiveCountTokens(t *testing.T) {
	baseURL, key := metaLiveProxy(t)
	resp, body := metaLivePost(t, baseURL, key, "/v1/messages/count_tokens",
		`{"model":"muse-spark-1.1[1m]","messages":[{"role":"user","content":"hello world"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.InputTokens <= 0 {
		t.Fatalf("count_tokens: %v, body = %s", err, body)
	}
}

// TestMetaLiveMultiTurnThinking replays an assistant turn with the
// redacted_thinking block Meta returns, verifying reasoning carries across
// turns the way Claude Code will replay it.
func TestMetaLiveMultiTurnThinking(t *testing.T) {
	baseURL, key := metaLiveProxy(t)
	resp, body := metaLivePost(t, baseURL, key, "/v1/messages",
		`{"model":"muse-spark-1.1","max_tokens":600,"messages":[{"role":"user","content":"Pick a secret color and tell me only its first letter."}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first turn status = %d, body = %s", resp.StatusCode, body)
	}
	var first struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("parse first turn: %v", err)
	}
	assistant, err := json.Marshal(first.Content)
	if err != nil {
		t.Fatal(err)
	}
	second := `{"model":"muse-spark-1.1","max_tokens":600,"messages":[` +
		`{"role":"user","content":"Pick a secret color and tell me only its first letter."},` +
		`{"role":"assistant","content":` + string(assistant) + `},` +
		`{"role":"user","content":"Now say the full color."}]}`
	resp, body = metaLivePost(t, baseURL, key, "/v1/messages", second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second turn status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"type":"message"`) {
		t.Errorf("second turn body = %s", body)
	}
}
