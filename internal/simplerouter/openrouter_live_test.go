package simplerouter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Live tests against openrouter.ai. Enable with SIMPLEROUTER_LIVE_OPENROUTER=1.
// Credentials: OPENROUTER_API_KEY or the saved openrouter_api_key.

// TestLiveOpenRouterAIStudioWebSearchSubstitution replays the failure that
// motivated the substitution: a Codex-shaped request (web_search + namespace
// tool) pinned to Google AI Studio, whose native grounding lane 429s on
// OpenRouter's shared pool. Through the proxy the request must succeed and
// stream Codex-native web_search_call items.
func TestLiveOpenRouterAIStudioWebSearchSubstitution(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_OPENROUTER") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_OPENROUTER=1 to run live OpenRouter tests")
	}
	key := liveOpenRouterAPIKey(t)
	proxyURL, stop, err := startResponsesPassthroughProxy(
		defaultOpenRouterAPIBase,
		"google/gemini-3.7-flash",
		http.DefaultClient,
		openRouterResponsesOptions("google/gemini-3.7-flash", "google-ai-studio"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	requestBody := map[string]any{
		"model": "ignored",
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "Use web search to find the latest stable Go release version. Reply with just the version number.",
			}},
		}},
		"tools": []any{
			map[string]any{"type": "web_search", "external_web_access": false},
			map[string]any{
				"type": "namespace",
				"name": "mcp__node_repl",
				"tools": []any{map[string]any{
					"type":       "function",
					"name":       "js",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}}},
				}},
			},
		},
		"tool_choice":       "auto",
		"max_output_tokens": 2000,
		"stream":            true,
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/responses", bytes.NewReader(payload))
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d %s", resp.StatusCode, body)
	}
	stream := string(body)
	if !strings.Contains(stream, `"type":"web_search_call"`) {
		t.Fatalf("stream missing web_search_call item:\n%s", stream)
	}
	if strings.Contains(stream, "openrouter:web_search") {
		t.Fatalf("upstream item type leaked back to Codex:\n%s", stream)
	}
	if !strings.Contains(stream, "response.completed") {
		t.Fatalf("stream did not complete:\n%s", stream)
	}
}

func liveOpenRouterAPIKey(t *testing.T) string {
	t.Helper()
	if key := cleanAPIKey(os.Getenv("OPENROUTER_API_KEY")); key != "" {
		return key
	}
	config, err := loadConfig()
	if err == nil {
		if key := cleanAPIKey(config.OpenRouterAPIKey); key != "" {
			return key
		}
	}
	t.Skip("no OpenRouter API key available")
	return ""
}
