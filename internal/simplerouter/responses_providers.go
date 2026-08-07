package simplerouter

import (
	"net/http"
	"strings"
)

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

func startMetaResponsesProxy(upstreamBase, model string, httpClient *http.Client) (string, func(), error) {
	return startResponsesPassthroughProxy(upstreamBase, model, httpClient, metaResponsesOptions())
}

func startXAIResponsesProxy(upstreamBase, model string, httpClient *http.Client) (string, func(), error) {
	return startResponsesPassthroughProxy(upstreamBase, model, httpClient, xaiResponsesOptions(model))
}

func metaResponsesOptions() responsesPassthroughOptions {
	return responsesPassthroughOptions{
		Label:                "Meta",
		ReasoningEffortMap:   map[string]string{"max": "xhigh", "ultra": "xhigh"},
		TranslateCustomTools: true,
	}
}

// xaiResponsesOptions configures the OpenAI-compatible Responses passthrough
// for https://api.x.ai/v1. xAI accepts function tools and built-ins (web_search,
// x_search, …) but rejects Codex freeform `custom` tools, so those are rewritten
// as functions. Effort mapping is model-specific: grok-4.5 cannot disable
// reasoning, grok-4.3 supports none, and only multi-agent supports xhigh.
//
// Docs: https://docs.x.ai/developers/model-capabilities/text/reasoning
//
//	https://docs.x.ai/developers/rest-api-reference/inference/chat
func xaiResponsesOptions(model string) responsesPassthroughOptions {
	model = strings.ToLower(strings.TrimSpace(model))
	effortMap := map[string]string{
		"none":    "low",
		"minimal": "low",
		"xhigh":   "high",
		"max":     "high",
		"ultra":   "high",
	}
	if model == "grok-4.3" || model == "grok-4.3-latest" || model == "grok-latest" {
		delete(effortMap, "none")
	}
	if strings.Contains(model, "multi-agent") {
		effortMap["xhigh"] = "xhigh"
		effortMap["max"] = "xhigh"
		effortMap["ultra"] = "xhigh"
	}
	nonReasoning := strings.Contains(model, "non-reasoning")
	fixedReasoning := !nonReasoning && (model == "grok-build-0.1" ||
		strings.HasPrefix(model, "grok-code-fast") ||
		(strings.Contains(model, "grok-4.20") && !strings.Contains(model, "multi-agent")))

	return responsesPassthroughOptions{
		Label:                             "xAI",
		ReasoningEffortMap:                effortMap,
		OmitReasoningEffort:               fixedReasoning,
		OmitReasoningControls:             nonReasoning,
		TranslateCustomTools:              true,
		OmitNullEncryptedReasoningContent: true,
		// Codex multi-agent tools arrive as Responses "namespace" groups; xAI
		// only accepts flat function / built-in tool variants.
		FlattenNamespaces: true,
		// Documented Responses tool types for api.x.ai (see REST inference chat).
		AllowedToolTypes: []string{
			"function",
			"web_search",
			"x_search",
			"image_generation",
			"collections_search",
			"file_search",
			"code_execution",
			"code_interpreter",
			"mcp",
			"shell",
		},
	}
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
