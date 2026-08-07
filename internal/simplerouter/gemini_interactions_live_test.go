package simplerouter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live Gemini Interactions checks for the empty-parameters fix.
// Opt in with SIMPLEROUTER_LIVE_GEMINI=1. Uses GEMINI_API_KEY / GOOGLE_API_KEY
// when set, otherwise ~/.simplerouter/config.json gemini_api_key.
func TestLiveGeminiInteractionsEmptyParameterTools(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_GEMINI") == "" {
		t.Skip("set SIMPLEROUTER_LIVE_GEMINI=1 to run live Gemini Interactions tests")
	}
	key := liveGeminiAPIKey(t)
	client := &http.Client{Timeout: 90 * time.Second}
	proxy := newGeminiInteractionsProxy(defaultGeminiAPIBase, "gemini-3.6-flash", client, false)

	// Reproduce the Codex failure mode: shell_command + parameterless tools
	// (get_context_remaining / new_context style empty object schemas).
	body := map[string]any{
		"model":  "gemini-3.6-flash",
		"stream": true,
		"input": []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Reply with exactly: GEMINI_OK. Do not call tools."},
				},
			},
		},
		"tools": []map[string]any{
			{
				"type":        "function",
				"name":        "shell_command",
				"description": "Run a shell command",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required":             []string{"command"},
					"additionalProperties": false,
				},
			},
			{
				"type":        "function",
				"name":        "get_context_remaining",
				"description": "Get the remaining tokens in the current context window.",
				"parameters": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
			{
				"type":        "function",
				"name":        "new_context",
				"description": "Start a new context window.",
				"parameters": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
			{"type": "web_search"},
		},
		"reasoning": map[string]any{"effort": "low", "summary": "auto"},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	resp := rr.Result()
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	text := string(out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("proxy HTTP %d: %s", resp.StatusCode, text)
	}
	if strings.Contains(text, "schema at top-level must be a boolean or an object") {
		t.Fatalf("empty-parameter tools still rejected by Gemini:\n%s", text)
	}
	if strings.Contains(text, `"type":"response.failed"`) || strings.Contains(text, "upstream_error") {
		t.Fatalf("proxy reported failure:\n%s", text)
	}
	if !strings.Contains(text, "response.created") && !strings.Contains(text, "response.completed") {
		t.Fatalf("unexpected stream (no response.created/completed):\n%s", text)
	}
	t.Logf("live Gemini stream ok (%d bytes)", len(out))
}

func TestLiveGeminiInteractionsDirectEmptyParametersShapes(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_GEMINI") == "" {
		t.Skip("set SIMPLEROUTER_LIVE_GEMINI=1 to run live Gemini Interactions tests")
	}
	key := liveGeminiAPIKey(t)

	// Unit-level conversion must produce schemas Gemini accepts.
	tools, _, err := responsesToolsToGeminiInteractions([]json.RawMessage{
		json.RawMessage(`{"type":"function","name":"get_context_remaining","description":"remaining","parameters":{"type":"object","properties":{},"additionalProperties":false}}`),
		json.RawMessage(`{"type":"function","name":"new_context","description":"new window","parameters":{"type":"object","properties":{}}}`),
		json.RawMessage(`{"type":"function","name":"missing_params","description":"no params field"}`),
		json.RawMessage(`{"type":"function","name":"shell_command","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`),
	}, "gemini-3.6-flash")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		m := tool.(map[string]any)
		if m["type"] != "function" {
			continue
		}
		if _, ok := m["parameters"]; !ok {
			t.Fatalf("function %q missing parameters after conversion: %#v", m["name"], m)
		}
	}

	payload := map[string]any{
		"model":  "gemini-3.6-flash",
		"input":  "Reply with exactly: GEMINI_OK. Do not call tools.",
		"stream": false,
		"store":  false,
		"tools":  tools,
		"generation_config": map[string]any{
			"thinking_level":     "low",
			"thinking_summaries": "auto",
			"tool_choice":        "auto",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, defaultGeminiAPIBase+"/interactions", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("Gemini request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("Gemini HTTP %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "schema at top-level must be a boolean or an object") {
		t.Fatalf("Gemini still rejects converted tools: %s", body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	status, _ := parsed["status"].(string)
	if status != "completed" && status != "requires_action" {
		t.Fatalf("unexpected status %q body=%s", status, body)
	}
	t.Logf("direct Interactions OK status=%s", status)
}

func liveGeminiAPIKey(t *testing.T) string {
	t.Helper()
	for _, env := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_AI_API_KEY"} {
		if key := strings.TrimSpace(os.Getenv(env)); key != "" {
			return key
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".simplerouter", "config.json"))
	if err != nil {
		t.Fatalf("read simplerouter config (set GEMINI_API_KEY or save a Gemini key): %v", err)
	}
	var cfg struct {
		GeminiAPIKey string `json:"gemini_api_key"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse simplerouter config: %v", err)
	}
	key := strings.TrimSpace(cfg.GeminiAPIKey)
	if key == "" {
		t.Fatal("no Gemini API key in env or ~/.simplerouter/config.json")
	}
	return key
}
