package simplerouter

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func handleEstimatedCountTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": len(body) / 4})
}

func relayCompatUpstreamError(w http.ResponseWriter, resp *http.Response, label string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	var wrapped struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error != nil && wrapped.Error.Message != "" {
		message = wrapped.Error.Message
	}
	if message == "" {
		message = label + " request failed"
	}
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	status, errType := anthropicErrorForStatus(resp.StatusCode)
	writeAnthropicError(w, status, errType, message)
}

func systemTextFromRaw(raw json.RawMessage) string {
	blocks, err := blocksFromContent(raw)
	if err != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func rawJSONAsString(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	return trimmed
}

func jsonRawOrObject(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return map[string]any{}
	}
	return raw
}

func parseToolArguments(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" || !json.Valid([]byte(args)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

func toolResultText(b anthropicBlock) string {
	if blocks, err := blocksFromContent(b.Content); err == nil {
		var parts []string
		for _, cb := range blocks {
			switch cb.Type {
			case "text":
				if cb.Text != "" {
					parts = append(parts, cb.Text)
				}
			case "image":
				if cb.Source != nil {
					parts = append(parts, "[image:"+cb.Source.MediaType+"]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(b.Content))
}

func reasoningEffort(budgetTokens int) string {
	switch {
	case budgetTokens >= 16_384:
		return "xhigh"
	case budgetTokens >= 8_192:
		return "high"
	case budgetTokens >= 4_096:
		return "medium"
	case budgetTokens > 0:
		return "low"
	default:
		return "medium"
	}
}

func zaiReasoningEffort(budgetTokens int) string {
	switch {
	case budgetTokens >= 16_384:
		return "max"
	case budgetTokens >= 8_192:
		return "high"
	case budgetTokens > 0:
		return "low"
	default:
		return "max"
	}
}
