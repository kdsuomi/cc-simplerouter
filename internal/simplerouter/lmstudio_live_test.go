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

// Live test against the default LM Studio Local Server. Enable with
// SIMPLEROUTER_LIVE_LMSTUDIO=1; optionally select a model with
// SIMPLEROUTER_LIVE_LMSTUDIO_MODEL.
func TestLiveLMStudioCodexCompatibility(t *testing.T) {
	if os.Getenv("SIMPLEROUTER_LIVE_LMSTUDIO") != "1" {
		t.Skip("set SIMPLEROUTER_LIVE_LMSTUDIO=1 to run the live LM Studio test")
	}
	models, err := lmStudioModels(context.Background(), http.DefaultClient, defaultLMStudioAPIBase)
	if err != nil {
		t.Fatal(err)
	}
	modelID := strings.TrimSpace(os.Getenv("SIMPLEROUTER_LIVE_LMSTUDIO_MODEL"))
	if modelID == "" {
		modelID = models[0].ID
	}
	if resolved, ok := resolveModel(modelID, models); !ok || !resolved.Exact {
		t.Fatalf("LM Studio model %q is not installed", modelID)
	} else {
		modelID = resolved.Model.ID
	}

	client := &http.Client{Timeout: 3 * time.Minute}
	proxyURL, stop, err := startLMStudioResponsesProxy(defaultLMStudioAPIBase, modelID, client)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	payload, err := json.Marshal(map[string]any{
		"model":        "ignored-by-proxy",
		"instructions": "You are a concise coding assistant.",
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "Follow the user's exact output request."}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with exactly OK."}}},
		},
		"prompt_cache_key": "live-compatibility-test",
		"include":          []string{"reasoning.encrypted_content"},
		"reasoning":        map[string]any{"effort": "none"},
		"tools": []any{
			map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: PATCH"}},
			map[string]any{"type": "tool_search", "execution": "client"},
		},
		"stream": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+lmStudioSessionKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LM Studio proxy returned HTTP %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("response.completed")) {
		t.Fatalf("LM Studio stream did not complete: %s", body)
	}
}
