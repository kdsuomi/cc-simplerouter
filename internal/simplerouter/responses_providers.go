package simplerouter

import "net/http"

func startGeminiResponsesProxy(upstreamBase, model string, httpClient *http.Client, disableReasoning bool) (string, func(), error) {
	return startGeminiInteractionsProxy(upstreamBase, model, httpClient, disableReasoning)
}

func startDeepSeekResponsesProxy(upstreamBase, model string, httpClient *http.Client, disableReasoning bool) (string, func(), error) {
	return startResponsesChatProxy(upstreamBase, model, httpClient, deepSeekResponsesOptions(disableReasoning))
}

func deepSeekResponsesOptions(disableReasoning bool) responsesChatProxyOptions {
	thinkingType := "enabled"
	if disableReasoning {
		thinkingType = "disabled"
	}
	return responsesChatProxyOptions{
		Label:                "DeepSeek",
		ChatPath:             "/chat/completions",
		DisableReasoning:     disableReasoning,
		SendReasoningEffort:  true,
		ReasoningReplayField: "reasoning_content",
		ReasoningEffortMap: map[string]string{
			"minimal": "high",
			"low":     "high",
			"medium":  "high",
			"xhigh":   "max",
			"ultra":   "max",
		},
		IncludeStreamUsage: true,
		ExtraBody: map[string]any{
			"thinking": map[string]any{"type": thinkingType},
		},
	}
}

func startZAIResponsesProxy(upstreamBase, model string, httpClient *http.Client, disableReasoning bool) (string, func(), error) {
	return startResponsesChatProxy(upstreamBase, model, httpClient, zaiResponsesOptions(disableReasoning))
}

func zaiResponsesOptions(disableReasoning bool) responsesChatProxyOptions {
	thinking := map[string]any{"type": "disabled"}
	if !disableReasoning {
		thinking = map[string]any{
			"type":           "enabled",
			"clear_thinking": false,
		}
	}
	return responsesChatProxyOptions{
		Label:                   "Z.AI",
		ChatPath:                "/chat/completions",
		DisableReasoning:        disableReasoning,
		SendReasoningEffort:     true,
		SendNoneReasoningEffort: true,
		ReasoningReplayField:    "reasoning_content",
		ReasoningEffortMap: map[string]string{
			"ultra": "max",
		},
		ToolStream: true,
		ExtraBody: map[string]any{
			"thinking": thinking,
		},
	}
}
