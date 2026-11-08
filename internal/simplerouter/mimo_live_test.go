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

// Enable with SIMPLEROUTER_LIVE_MIMO=1. Uses MIMO_API_KEY or the saved
// mimo_api_key without changing the user's config. Requests contain only
// synthetic test prompts; tools are simulated, never executed.
func TestLiveMiMoV26(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_MIMO") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_MIMO=1 to run live MiMo tests")
	}
	key := cleanAPIKey(os.Getenv("MIMO_API_KEY"))
	if key == "" {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		key = cfg.MiMoAPIKey
	}
	if key == "" {
		t.Fatal("set MIMO_API_KEY or save mimo_api_key in config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := validateBearerModels(ctx, http.DefaultClient, defaultMiMoAPIBase, key, "MiMo"); err != nil {
		t.Fatal(err)
	}
	for _, model := range curatedProviderModels(providerMiMo) {
		t.Run(model.ID, func(t *testing.T) {
			t.Run("thinking_disabled", func(t *testing.T) {
				proxyURL, stop, err := startMiMoResponsesProxy(defaultMiMoAPIBase, model.ID, http.DefaultClient, true)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				response := liveMiMoRequest(t, proxyURL, key, map[string]any{
					"input": []any{map[string]any{"type": "message", "role": "user", "content": "Reply with exactly: pong"}},
				})
				assertMiMoOutputText(t, response, "pong")
			})
			t.Run("reasoning_tool_continuation", func(t *testing.T) {
				proxyURL, stop, err := startMiMoResponsesProxy(defaultMiMoAPIBase, model.ID, http.DefaultClient, false)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				input := []any{map[string]any{
					"type": "message", "role": "user", "content": "Call test.get_magic_word exactly once. After the tool returns, reply with exactly its returned word. Do not call apply_patch.",
				}}
				tools := []any{
					map[string]any{
						"type": "namespace", "name": "test", "description": "Test tools",
						"tools": []any{map[string]any{
							"type": "function", "name": "get_magic_word", "description": "Return the word to print.",
							"parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
						}},
					},
					map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a freeform patch."},
				}
				first := liveMiMoRequest(t, proxyURL, key, map[string]any{
					"input": input, "tools": tools, "tool_choice": "auto", "parallel_tool_calls": true,
					"reasoning": map[string]any{"effort": "high", "summary": "auto"},
				})
				output, _ := first["output"].([]any)
				callID := ""
				hasReplay := false
				for _, raw := range output {
					item, _ := raw.(map[string]any)
					if item["type"] == "reasoning" {
						replay, _ := item["encrypted_content"].(string)
						hasReplay = strings.HasPrefix(replay, chatReplayStatePrefix)
					}
					if item["type"] == "function_call" {
						if item["name"] != "get_magic_word" || item["namespace"] != "test" {
							t.Fatalf("tool identity not restored: %#v", item)
						}
						callID, _ = item["call_id"].(string)
					}
				}
				if callID == "" || !hasReplay {
					t.Fatalf("missing tool call or reasoning replay: %#v", output)
				}
				input = append(input, output...)
				input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": "beta"})
				second := liveMiMoRequest(t, proxyURL, key, map[string]any{
					"input": input, "tools": tools, "tool_choice": "auto", "parallel_tool_calls": true,
					"reasoning": map[string]any{"effort": "high", "summary": "auto"},
				})
				assertMiMoOutputText(t, second, "beta")
			})
		})
	}
}

func liveMiMoRequest(t *testing.T, proxyURL, key string, body map[string]any) map[string]any {
	t.Helper()
	body["model"] = "ignored-by-proxy"
	body["stream"] = true
	body["max_output_tokens"] = 1024
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	stream := strings.ReplaceAll(string(responseBody), key, "[redacted]")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MiMo proxy HTTP %d: %s", resp.StatusCode, truncateForError(stream, 1500))
	}
	var output []any
	for _, event := range decodeTestSSE(t, stream) {
		if event["type"] == "response.output_item.done" {
			output = append(output, event["item"])
		}
		if event["type"] == "response.completed" {
			response, _ := event["response"].(map[string]any)
			usage, _ := response["usage"].(map[string]any)
			if total, _ := usage["total_tokens"].(float64); total <= 0 {
				t.Fatalf("missing usage: %#v", usage)
			}
			// Like Codex, collect completed items as they arrive. The Chat
			// translator's completion event carries status and usage only.
			response["output"] = output
			return response
		}
	}
	t.Fatalf("missing completed response: %s", truncateForError(stream, 1500))
	return nil
}

func assertMiMoOutputText(t *testing.T, response map[string]any, want string) {
	t.Helper()
	var text strings.Builder
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			if part["type"] == "output_text" {
				value, _ := part["text"].(string)
				text.WriteString(value)
			}
		}
	}
	if strings.TrimSpace(text.String()) != want {
		t.Fatalf("output text = %q, want %q", text.String(), want)
	}
}
