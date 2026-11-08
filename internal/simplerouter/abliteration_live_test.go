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

// Enable with SIMPLEROUTER_LIVE_ABLITERATION=1. Uses environment credentials
// or the saved key without writing config. Prompts and tool results are synthetic;
// no returned tool calls are executed.
func TestLiveAbliteration(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_ABLITERATION") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_ABLITERATION=1 to run live Abliteration tests")
	}
	key := cleanAPIKey(os.Getenv("ABLITERATION_API_KEY"))
	if key == "" {
		key = cleanAPIKey(os.Getenv("ABLIT_KEY"))
	}
	if key == "" {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		key = cfg.AbliterationAPIKey
	}
	if key == "" {
		t.Fatal("set ABLITERATION_API_KEY / ABLIT_KEY or save abliteration_api_key in config")
	}
	models, err := abliterationModels(context.Background(), http.DefaultClient, defaultAbliterationAPIBase, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		t.Run(model.ID, func(t *testing.T) {
			proxyURL, stop, err := startResponsesPassthroughProxy(defaultAbliterationAPIBase, model.ID, http.DefaultClient, abliterationResponsesOptions(model.ID))
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			t.Run("default_reasoning_custom_tool", func(t *testing.T) {
				response := liveAbliterationRequest(t, proxyURL, key, map[string]any{
					"input":             "Call apply_patch exactly once to add hello.txt containing hello. Use this exact patch: *** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch",
					"tools":             []any{map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch."}},
					"tool_choice":       "required",
					"reasoning":         map[string]any{"effort": model.DefaultReasoningEffort, "summary": "auto"},
					"text":              map[string]any{"verbosity": "medium"},
					"service_tier":      "default",
					"max_output_tokens": 8192,
				})
				output, _ := response["output"].([]any)
				for _, raw := range output {
					item, _ := raw.(map[string]any)
					if item["type"] == "custom_tool_call" && item["name"] == "apply_patch" {
						if input, _ := item["input"].(string); strings.Contains(input, "*** Begin Patch") {
							return
						}
					}
				}
				t.Fatalf("custom tool call was not restored: %#v", output)
			})
			t.Run("thinking_disabled", func(t *testing.T) {
				response := liveAbliterationRequest(t, proxyURL, key, map[string]any{
					"input":     "Reply with exactly: pong",
					"reasoning": map[string]any{"effort": "none", "summary": "none"},
				})
				if got := abliterationOutputText(response); got != "pong" {
					t.Fatalf("output text = %q, want pong", got)
				}
			})
			t.Run("tool_continuation", func(t *testing.T) {
				input := []any{map[string]any{"type": "message", "role": "user", "content": "Call test.get_magic_word exactly once. After it returns, reply with exactly the returned word. Do not call apply_patch."}}
				tools := []any{
					map[string]any{"type": "namespace", "name": "test", "tools": []any{
						map[string]any{"type": "function", "name": "get_magic_word", "description": "Return the word to print.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
					}},
					map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a freeform patch."},
				}
				first := liveAbliterationRequest(t, proxyURL, key, map[string]any{
					"input": input, "tools": tools, "tool_choice": "auto", "parallel_tool_calls": true,
					"reasoning":        map[string]any{"effort": "low", "summary": "auto"},
					"include":          []string{"reasoning.encrypted_content"},
					"prompt_cache_key": "simplerouter-abliteration-test",
				})
				output, _ := first["output"].([]any)
				callID := ""
				for _, raw := range output {
					item, _ := raw.(map[string]any)
					if item["type"] == "function_call" {
						if item["name"] != "get_magic_word" || item["namespace"] != "test" {
							t.Fatalf("tool identity not restored: %#v", item)
						}
						callID, _ = item["call_id"].(string)
					}
				}
				if callID == "" {
					t.Fatalf("missing function call: %#v", output)
				}
				input = append(input, output...)
				input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": "beta"})
				second := liveAbliterationRequest(t, proxyURL, key, map[string]any{
					"input": input, "tools": tools, "tool_choice": "auto",
					"reasoning": map[string]any{"effort": "low", "summary": "auto"},
				})
				if got := abliterationOutputText(second); got != "beta" {
					t.Fatalf("output text = %q, want beta", got)
				}
			})
		})
	}
}

func liveAbliterationRequest(t *testing.T, proxyURL, key string, body map[string]any) map[string]any {
	t.Helper()
	body["model"], body["stream"], body["store"] = "ignored-by-proxy", true, false
	if _, set := body["max_output_tokens"]; !set {
		body["max_output_tokens"] = 2048
	}
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
		t.Fatalf("Abliteration HTTP %d: %s", resp.StatusCode, truncateForError(stream, 1500))
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
			if len(output) > 0 {
				response["output"] = output
			}
			return response
		}
	}
	t.Fatalf("missing completed response: %s", stream[max(0, len(stream)-1500):])
	return nil
}

func abliterationOutputText(response map[string]any) string {
	var result strings.Builder
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
				result.WriteString(value)
			}
		}
	}
	return strings.TrimSpace(result.String())
}
