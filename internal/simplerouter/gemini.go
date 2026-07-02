package simplerouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// errGeminiKeyRejected signals that Google AI Studio explicitly rejected the
// API key. Note: Google returns HTTP 400 INVALID_ARGUMENT for a malformed or
// invalid key (not just 401), and 403 for a disabled/permission-denied key —
// all three mean "this key is bad". Everything else is transient.
var errGeminiKeyRejected = errors.New("Google AI Studio rejected the API key")

type geminiClient struct {
	httpClient *http.Client
	apiBase    string
}

func newGeminiClient(httpClient *http.Client, apiBase string) *geminiClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(apiBase) == "" {
		apiBase = defaultGeminiAPIBase
	}
	return &geminiClient{httpClient: httpClient, apiBase: strings.TrimRight(apiBase, "/")}
}

func (c *geminiClient) validateKey(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/models?pageSize=1", nil)
	if err != nil {
		return err
	}
	// Header, not query param, so the key never lands in URLs/logs.
	req.Header.Set("x-goog-api-key", key)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("validate Google AI Studio key: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return errGeminiKeyRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Google AI Studio key validation failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// models lists the Gemini catalog, following nextPageToken pagination, and
// filters it to models Claude Code can drive.
func (c *geminiClient) models(ctx context.Context, key string) ([]Model, error) {
	var out []Model
	pageToken := ""
	for {
		url := c.apiBase + "/models?pageSize=1000"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", key)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch Gemini models: %w", err)
		}
		var raw geminiModelsResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("fetch Gemini models: HTTP %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode Gemini models: %w", decodeErr)
		}
		for _, m := range raw.Models {
			id := strings.TrimPrefix(strings.TrimSpace(m.Name), "models/")
			if id == "" || !keepGeminiModel(id, m.SupportedGenerationMethods) {
				continue
			}
			// SupportedParameters is synthesized so the shared picker tags
			// work: every kept model supports function calling, and the
			// thinking-capable families get the reasoning tag.
			params := []string{"tools"}
			if geminiSupportsThinking(id) {
				params = append(params, "reasoning")
			}
			out = append(out, Model{
				ID:                  id,
				Name:                strings.TrimSpace(m.DisplayName),
				ContextLength:       m.InputTokenLimit,
				SupportedParameters: params,
			})
		}
		pageToken = raw.NextPageToken
		if pageToken == "" {
			return out, nil
		}
	}
}

// geminiExcludedSubstrings drops models that report generateContent support
// but cannot drive Claude Code: media generation (image/tts/audio/video/
// music), embeddings, and special-purpose endpoints (robotics, computer use,
// deep research, live/realtime, Gemma models without function calling).
var geminiExcludedSubstrings = []string{
	"embedding", "aqa", "tts", "image", "veo", "audio", "gemma", "learnlm",
	"vision", "lyria", "robotics", "computer-use", "deep-research",
	"nano-banana", "omni", "live", "translate", "antigravity",
}

func keepGeminiModel(id string, methods []string) bool {
	supported := false
	for _, m := range methods {
		if m == "generateContent" {
			supported = true
			break
		}
	}
	if !supported {
		return false
	}
	lower := strings.ToLower(id)
	for _, excluded := range geminiExcludedSubstrings {
		if strings.Contains(lower, excluded) {
			return false
		}
	}
	return true
}

// geminiSupportsThinking reports whether a model family emits thinking (2.5
// and 3.x generations, plus the rolling -latest aliases). Heuristic — the
// listing API does not expose this.
func geminiSupportsThinking(id string) bool {
	lower := strings.ToLower(id)
	return strings.Contains(lower, "2.5") || strings.Contains(lower, "gemini-3") || strings.HasSuffix(lower, "-latest")
}
