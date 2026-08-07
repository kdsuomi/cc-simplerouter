package simplerouter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests against api.x.ai. Enable with SIMPLEROUTER_LIVE_XAI=1.
// Credentials: XAI_API_KEY / GROK_API_KEY, saved xai_api_key, or ~/.grok/auth.json.

func TestLiveXAIEncryptedReasoningToolContinuation(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_XAI") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_XAI=1 to run live xAI tests")
	}
	key := liveXAIAPIKey(t)
	proxyURL, stop, err := startXAIResponsesProxy(defaultXAIAPIBase, "grok-4.5", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	firstInput := []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": strings.Repeat("Background detail for a long coding task. ", 800) +
					"\nCall get_magic_word exactly once. After it returns, reply with exactly the returned word.",
			},
		},
	}}
	tools := []any{map[string]any{
		"type":        "function",
		"name":        "get_magic_word",
		"description": "Return the word that the assistant must print.",
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
		"strict": false,
	}}
	first := liveXAIRequest(t, proxyURL, key, map[string]any{
		"model":       "ignored-by-proxy",
		"input":       firstInput,
		"include":     []string{"reasoning.encrypted_content"},
		"reasoning":   map[string]any{"effort": "low", "summary": "auto"},
		"tools":       tools,
		"tool_choice": "required",
		"stream":      false,
		"store":       false,
	})
	if first.status != http.StatusOK {
		t.Fatalf("first response status = %d: %s", first.status, truncateForError(string(first.body), 1200))
	}
	var firstResponse map[string]any
	if err := json.Unmarshal(first.body, &firstResponse); err != nil {
		t.Fatalf("decode first response: %v\n%s", err, truncateForError(string(first.body), 1200))
	}
	output, ok := firstResponse["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("first response has no output: %s", truncateForError(string(first.body), 1200))
	}
	foundEncryptedReasoning := false
	callID := ""
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] == "reasoning" {
			encryptedContent, _ := item["encrypted_content"].(string)
			if strings.TrimSpace(encryptedContent) != "" {
				foundEncryptedReasoning = true
			}
		}
		if item["type"] == "function_call" {
			callID, _ = item["call_id"].(string)
			callID = strings.TrimSpace(callID)
		}
		if id, _ := item["id"].(string); strings.TrimSpace(id) == "" {
			t.Fatalf("xAI output item omitted id: %#v", item)
		}
	}
	if !foundEncryptedReasoning {
		t.Fatalf("first response omitted encrypted reasoning: %s", truncateForError(string(first.body), 1200))
	}
	if callID == "" {
		t.Fatalf("first response omitted function call: %s", truncateForError(string(first.body), 1200))
	}

	toolOutput := map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  "beta",
	}
	codexShape := cloneJSONArray(t, output)
	for _, raw := range codexShape {
		if item, ok := raw.(map[string]any); ok {
			delete(item, "id")
			delete(item, "status")
			if item["type"] == "reasoning" {
				item["content"] = nil
			}
		}
	}
	codexShape = append(codexShape, toolOutput)

	response := liveXAIRequest(t, proxyURL, key, map[string]any{
		"model":     "ignored-by-proxy",
		"input":     codexShape,
		"include":   []string{"reasoning.encrypted_content"},
		"reasoning": map[string]any{"effort": "low", "summary": "auto"},
		"tools":     tools,
		"stream":    false,
		"store":     false,
	})
	if response.status != http.StatusOK {
		t.Fatalf("tool continuation status = %d: %s", response.status, truncateForError(string(response.body), 1200))
	}
	if !bytes.Contains(bytes.ToLower(response.body), []byte("beta")) {
		t.Fatalf("tool continuation omitted result: %s", truncateForError(string(response.body), 1200))
	}
}

type liveXAIResult struct {
	status int
	body   []byte
}

func liveXAIRequest(t *testing.T, proxyURL, key string, body map[string]any) liveXAIResult {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
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
	return liveXAIResult{status: resp.StatusCode, body: responseBody}
}

func cloneJSONArray(t *testing.T, input []any) []any {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var cloned []any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestLiveXAIResponsesWithCustomToolTranslation(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_XAI") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_XAI=1 to run live xAI tests")
	}
	key := liveXAIAPIKey(t)
	proxyURL, stop, err := startXAIResponsesProxy(defaultXAIAPIBase, "grok-4.5", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	body := map[string]any{
		"model": "ignored-by-proxy",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Reply with exactly: pong. Do not call tools."},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "shell_command",
				"description": "Run a shell command",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
			map[string]any{
				"type":        "custom",
				"name":        "apply_patch",
				"description": "Apply a freeform patch",
			},
			// Codex multi-agent form that previously caused xAI 422.
			map[string]any{
				"type":        "namespace",
				"name":        "collaboration",
				"description": "Multi-agent tools",
				"tools": []any{
					map[string]any{
						"type":        "function",
						"name":        "list_agents",
						"description": "List agents",
						"strict":      true,
						"parameters": map[string]any{
							"type":       "object",
							"properties": map[string]any{"limit": map[string]any{"type": "integer"}},
						},
					},
				},
			},
			map[string]any{
				"type":      "tool_search",
				"execution": "client",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"query": map[string]any{"type": "string"}},
					"required":   []string{"query"},
				},
			},
		},
		"tool_choice":         "none",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": "low", "summary": "auto"},
		"stream":              false,
		"store":               false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, truncateForError(string(respBody), 800))
	}
	if !bytes.Contains(respBody, []byte(`"status":"completed"`)) && !bytes.Contains(respBody, []byte(`"status": "completed"`)) {
		// Streaming-style JSON may still complete; require either completed or output text.
		if !bytes.Contains(bytes.ToLower(respBody), []byte("pong")) {
			t.Fatalf("unexpected response: %s", truncateForError(string(respBody), 800))
		}
	}
}

func TestLiveXAIDropsToolChoiceWithoutSupportedTools(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_XAI") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_XAI=1 to run live xAI tests")
	}
	key := liveXAIAPIKey(t)
	proxyURL, stop, err := startXAIResponsesProxy(defaultXAIAPIBase, "grok-4.5", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	result := liveXAIRequest(t, proxyURL, key, map[string]any{
		"model":       "ignored-by-proxy",
		"input":       "Reply with exactly: ok",
		"tools":       []any{map[string]any{"type": "tool_search", "execution": "client"}},
		"tool_choice": "auto",
		"reasoning":   map[string]any{"effort": "low"},
		"stream":      false,
		"store":       false,
	})
	if result.status != http.StatusOK {
		t.Fatalf("status = %d: %s", result.status, truncateForError(string(result.body), 1200))
	}
}

func TestLiveXAIWebSearchStripsExternalWebAccess(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_XAI") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_XAI=1 to run live xAI tests")
	}
	key := liveXAIAPIKey(t)
	proxyURL, stop, err := startXAIResponsesProxy(defaultXAIAPIBase, "grok-4.5", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	body := map[string]any{
		"model": "ignored",
		"input": "Reply with exactly: ok. Do not search the web.",
		"tools": []any{
			map[string]any{
				"type":                 "web_search",
				"external_web_access":  false,
				"search_content_types": []string{"text"},
			},
		},
		"tool_choice": "none",
		"reasoning":   map[string]any{"effort": "low"},
		"stream":      false,
		"store":       false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (external_web_access not stripped?): %s", resp.StatusCode, truncateForError(string(respBody), 800))
	}
}

func TestLiveXAIDisableThinkingMapsNoneToLow(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_XAI") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_XAI=1 to run live xAI tests")
	}
	key := liveXAIAPIKey(t)
	proxyURL, stop, err := startXAIResponsesProxy(defaultXAIAPIBase, "grok-4.5", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	body := map[string]any{
		"model": "ignored",
		"input": "Reply with exactly: ok",
		"reasoning": map[string]any{
			"effort":  "none",
			"summary": "auto",
		},
		"stream": false,
		"store":  false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (none→low mapping failed?): %s", resp.StatusCode, truncateForError(string(respBody), 800))
	}
}

func TestLiveXAIModelSpecificReasoningCompatibility(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_XAI") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_XAI=1 to run live xAI tests")
	}
	key := liveXAIAPIKey(t)
	tests := []struct {
		name   string
		model  string
		effort string
	}{
		{name: "grok45_minimal_clamps_to_low", model: "grok-4.5", effort: "minimal"},
		{name: "grok45_xhigh_clamps_to_high", model: "grok-4.5", effort: "xhigh"},
		{name: "grok43_preserves_none", model: "grok-4.3", effort: "none"},
		{name: "grok_build_high", model: "grok-build-0.1", effort: "high"},
		{name: "grok420_reasoning_high", model: "grok-4.20-0309-reasoning", effort: "high"},
		{name: "grok420_non_reasoning_omits_controls", model: "grok-4.20-0309-non-reasoning", effort: "high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxyURL, stop, err := startXAIResponsesProxy(defaultXAIAPIBase, test.model, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()

			result := liveXAIRequest(t, proxyURL, key, map[string]any{
				"model":     "ignored-by-proxy",
				"input":     "Reply with exactly: ok",
				"reasoning": map[string]any{"effort": test.effort, "summary": "auto"},
				"stream":    false,
				"store":     false,
			})
			if result.status != http.StatusOK {
				t.Fatalf("%s status = %d: %s", test.model, result.status, truncateForError(string(result.body), 1200))
			}
			if (test.model == "grok-build-0.1" || test.model == "grok-4.20-0309-reasoning") &&
				!bytes.Contains(result.body, []byte(`"summary"`)) {
				t.Fatalf("%s response omitted reasoning summary: %s", test.model, truncateForError(string(result.body), 1200))
			}
		})
	}
}

func liveXAIAPIKey(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"XAI_API_KEY", "GROK_API_KEY"} {
		if key := cleanAPIKey(os.Getenv(name)); key != "" {
			return key
		}
	}
	if cfg, err := loadConfig(); err == nil {
		if key := cleanAPIKey(cfg.XAIAPIKey); key != "" {
			return key
		}
	}
	token, err := loadGrokCLISessionToken(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatalf("load Grok CLI session: %v", err)
	}
	if token == "" {
		t.Fatal("no xAI credentials: set XAI_API_KEY or run grok login")
	}
	// Sanity-check the token talks to api.x.ai.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := validateBearerModels(ctx, http.DefaultClient, defaultXAIAPIBase, token, "xAI"); err != nil {
		t.Fatalf("session token rejected: %v", err)
	}
	t.Log("using Grok CLI session token from ~/.grok/auth.json")
	return token
}
