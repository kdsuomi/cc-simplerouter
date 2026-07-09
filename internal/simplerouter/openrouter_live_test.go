package simplerouter

// Live smoke tests against OpenRouter. Skipped unless
// SIMPLEROUTER_LIVE_OPENROUTER is set. They use the OpenRouter key saved in
// the simplerouter config file.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func openRouterLiveProxy(t *testing.T, model string, opts openRouterProxyOptions) (string, string) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_LIVE_OPENROUTER")) == "" {
		t.Skip("SIMPLEROUTER_LIVE_OPENROUTER not set")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenRouterAPIKey == "" {
		t.Skip("no OpenRouter key in config")
	}
	baseURL, stop, err := startOpenRouterProxy("", model, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return baseURL, cfg.OpenRouterAPIKey
}

func openRouterLivePost(t *testing.T, baseURL, key, body string) (*http.Response, string) {
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

// liveCollectBlocks reassembles Anthropic content blocks from an SSE stream.
func liveCollectBlocks(t *testing.T, events []sseEvent) (blocks []map[string]any, stopReason string) {
	t.Helper()
	byIndex := map[int]map[string]any{}
	var order []int
	for _, ev := range events {
		switch ev.Name {
		case "content_block_start":
			idx := int(ev.Data["index"].(float64))
			block := map[string]any{}
			for k, v := range ev.Data["content_block"].(map[string]any) {
				block[k] = v
			}
			byIndex[idx] = block
			order = append(order, idx)
		case "content_block_delta":
			idx := int(ev.Data["index"].(float64))
			block := byIndex[idx]
			if block == nil {
				continue
			}
			d := ev.Data["delta"].(map[string]any)
			switch d["type"] {
			case "thinking_delta":
				block["thinking"] = block["thinking"].(string) + d["thinking"].(string)
			case "text_delta":
				block["text"] = block["text"].(string) + d["text"].(string)
			case "signature_delta":
				block["signature"] = d["signature"]
			case "input_json_delta":
				raw, _ := block["partial_json"].(string)
				block["partial_json"] = raw + d["partial_json"].(string)
			}
		case "message_delta":
			if sr, ok := ev.Data["delta"].(map[string]any)["stop_reason"].(string); ok {
				stopReason = sr
			}
		case "error":
			t.Fatalf("stream error: %v", ev.Data)
		}
	}
	for _, idx := range order {
		block := byIndex[idx]
		if raw, ok := block["partial_json"].(string); ok {
			block["input"] = json.RawMessage(raw)
			delete(block, "partial_json")
		}
		blocks = append(blocks, block)
	}
	return blocks, stopReason
}

func TestLiveOpenRouterStreamingThinking(t *testing.T) {
	model := "minimax/minimax-m3"
	baseURL, key := openRouterLiveProxy(t, model, openRouterProxyOptions{SupportsReasoning: true})
	resp, body := openRouterLivePost(t, baseURL, key, `{
		"model":"`+model+`","max_tokens":3000,"stream":true,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"In one word, what is 2+2? Think briefly first."}]
	}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	events := parseSSE(t, body)
	blocks, _ := liveCollectBlocks(t, events)
	var thinking, text string
	thinkingBlocks := 0
	for _, b := range blocks {
		switch b["type"] {
		case "thinking":
			thinkingBlocks++
			thinking += b["thinking"].(string)
		case "text":
			text += b["text"].(string)
		}
	}
	t.Logf("model=%s thinking_blocks=%d thinking=%q text=%q", model, thinkingBlocks, thinking, text)
	if thinking == "" {
		t.Error("no thinking text streamed")
	}
	if !strings.Contains(strings.ToLower(text), "four") && !strings.Contains(text, "4") {
		t.Errorf("text = %q", text)
	}
	last := events[len(events)-1]
	if last.Name != "message_stop" {
		t.Errorf("last event = %s", last.Name)
	}
}

// TestLiveOpenRouterAnthropicToolRoundTrip exercises the riskiest path: an
// Anthropic model's signed reasoning must survive the redacted_thinking
// round trip when a tool result is sent back.
func TestLiveOpenRouterAnthropicToolRoundTrip(t *testing.T) {
	model := "anthropic/claude-haiku-4.5"
	baseURL, key := openRouterLiveProxy(t, model, openRouterProxyOptions{SupportsReasoning: true})
	toolsAndSystem := `
		"system":"Answer weather questions using the get_weather tool.",
		"tools":[{"name":"get_weather","description":"Get current weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`
	resp, body := openRouterLivePost(t, baseURL, key, `{
		"model":"`+model+`","max_tokens":3000,"stream":true,
		"thinking":{"type":"enabled","budget_tokens":1024},`+toolsAndSystem+`,
		"messages":[{"role":"user","content":"What is the weather in Paris right now? You must use the tool."}]
	}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	blocks, stopReason := liveCollectBlocks(t, parseSSE(t, body))
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, blocks: %v", stopReason, blocks)
	}
	var toolUseID string
	sawRedacted := false
	assistantContent := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b["type"] {
		case "tool_use":
			toolUseID = b["id"].(string)
		case "redacted_thinking":
			sawRedacted = true
		}
		assistantContent = append(assistantContent, b)
	}
	if toolUseID == "" {
		t.Fatalf("no tool_use block: %v", blocks)
	}
	if !sawRedacted {
		t.Fatal("no redacted_thinking block carrying reasoning_details")
	}

	turn2 := map[string]any{
		"model": model, "max_tokens": 3000, "stream": true,
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
		"messages": []any{
			map[string]any{"role": "user", "content": "What is the weather in Paris right now? You must use the tool."},
			map[string]any{"role": "assistant", "content": assistantContent},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": toolUseID, "content": "22C and sunny",
			}}},
		},
		"tools": json.RawMessage(`[{"name":"get_weather","description":"Get current weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`),
	}
	payload, err := json.Marshal(turn2)
	if err != nil {
		t.Fatal(err)
	}
	resp2, body2 := openRouterLivePost(t, baseURL, key, string(payload))
	if resp2.StatusCode != 200 {
		t.Fatalf("turn 2 status %d: %s", resp2.StatusCode, body2)
	}
	blocks2, _ := liveCollectBlocks(t, parseSSE(t, body2))
	var text string
	for _, b := range blocks2 {
		if b["type"] == "text" {
			text += b["text"].(string)
		}
	}
	t.Logf("turn 2 text: %q", text)
	if !strings.Contains(text, "22") && !strings.Contains(strings.ToLower(text), "sunny") {
		t.Errorf("turn 2 did not use the tool result: %q", text)
	}
}
