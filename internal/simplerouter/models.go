package simplerouter

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

var recommendedModelIDs = []string{
	"z-ai/glm-5.2",
	"deepseek/deepseek-v4-flash",
	"deepseek/deepseek-v4-pro",
	"moonshotai/kimi-k2.6",
	"minimax/minimax-m3",
}

// recommendedGeminiModelIDs is the curated top of the Google AI Studio picker
// (verified against the live /models listing, July 2026). Gemini ids never
// contain "/" and OpenRouter ids always do, so the two lists can share the
// recommendation machinery without colliding.
var recommendedGeminiModelIDs = []string{
	"gemini-3.6-flash",
	"gemini-3.1-pro-preview",
	"gemini-3.5-flash",
	"gemini-2.5-pro",
	"gemini-2.5-flash",
}

var recommendedFirstClassModelIDs = []string{
	"grok-4.5",
	"grok-4.3",
	"grok-build-0.1",
	"grok-4.20-0309-reasoning",
	"muse-spark-1.2",
	"muse-spark-1.2-contributor",
	"muse-spark-1.1",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"glm-5.2",
	"glm-5",
}

var testedModelIDs = map[string]bool{
	"z-ai/glm-5.2":                 true,
	"qwen/qwen3-coder":             true,
	"google/gemini-2.5-flash-lite": true,
	"openai/gpt-4.1-mini":          true,
	"openai/gpt-4.1-nano":          true,
	"deepseek/deepseek-v4-flash":   true,
	"deepseek/deepseek-v4-pro":     true,
	"moonshotai/kimi-k2.6":         true,
	"minimax/minimax-m3":           true,
	"deepseek-v4-flash":            true,
	"deepseek-v4-pro":              true,
	"glm-5.2":                      true,
	"glm-5":                        true,
	"muse-spark-1.2":               true,
	"muse-spark-1.2-contributor":   true,
	"muse-spark-1.1":               true,
}

func curatedProviderModels(provider string) []Model {
	var models []Model
	switch provider {
	case providerOpenAI:
		models = []Model{
			{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", ContextLength: 1_050_000, SupportedParameters: []string{"tools", "reasoning"}},
			{ID: "gpt-5.6-terra", Name: "GPT-5.6 Terra", ContextLength: 1_050_000, SupportedParameters: []string{"tools", "reasoning"}},
			{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", ContextLength: 1_050_000, SupportedParameters: []string{"tools", "reasoning"}},
			{ID: "gpt-5.5", Name: "GPT-5.5", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning"}},
			{ID: "gpt-5.4", Name: "GPT-5.4", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning"}},
			{ID: "gpt-5.4-mini", Name: "GPT-5.4 mini", ContextLength: 400_000, SupportedParameters: []string{"tools", "reasoning"}},
		}
	case providerDeepSeek:
		models = []Model{
			{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning"}},
			{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning"}},
		}
	case providerZAI:
		models = []Model{
			{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning", "reasoning_effort"}},
			{ID: "glm-5", Name: "GLM-5", ContextLength: 200_000, SupportedParameters: []string{"tools", "reasoning"}},
		}
	case providerMeta:
		// Meta Model API model IDs and session defaults from
		// https://dev.meta.ai/docs/models and https://dev.meta.ai/docs/coding-agents
		// (standard vs contributor tier, 1,048,576 context, high/auto reasoning).
		muse := func(id, name string) Model {
			return Model{
				ID:                        id,
				Name:                      name,
				ContextLength:             1_048_576,
				SupportedParameters:       []string{"tools", "reasoning"},
				SupportedReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"},
				DefaultReasoningEffort:    "high",
				DefaultReasoningSummary:   "auto",
				AutoCompactTokenLimit:     900_000,
			}
		}
		models = []Model{
			muse("muse-spark-1.2", "Muse Spark 1.2"),
			muse("muse-spark-1.2-contributor", "Muse Spark 1.2 Contributor"),
			muse("muse-spark-1.1", "Muse Spark 1.1"),
		}
	case providerXAI:
		// xAI text models from https://docs.x.ai/developers/models (August 2026).
		// Grok 4.5 supports low/medium/high and cannot disable reasoning. Grok 4.3
		// additionally supports none; only the multi-agent model supports xhigh.
		// Grok Build 0.1 and Grok 4.20 Reasoning use fixed, non-configurable effort.
		grokReasoning := func(id, name string, ctx int, efforts []string, defaultEffort string) Model {
			autoCompact := int(float64(ctx) * 0.8)
			return Model{
				ID:                        id,
				Name:                      name,
				ContextLength:             ctx,
				SupportedParameters:       []string{"tools", "reasoning"},
				SupportedReasoningEfforts: efforts,
				DefaultReasoningEffort:    defaultEffort,
				DefaultReasoningSummary:   "auto",
				AutoCompactTokenLimit:     autoCompact,
			}
		}
		models = []Model{
			grokReasoning("grok-4.5", "Grok 4.5", 500_000, []string{"low", "medium", "high"}, "high"),
			grokReasoning("grok-4.3", "Grok 4.3", 1_000_000, []string{"none", "low", "medium", "high"}, "low"),
			grokReasoning("grok-build-0.1", "Grok Build 0.1", 256_000, nil, ""),
			grokReasoning("grok-4.20-0309-reasoning", "Grok 4.20 Reasoning", 1_000_000, nil, ""),
			{
				ID:                    "grok-4.20-0309-non-reasoning",
				Name:                  "Grok 4.20 Non-Reasoning",
				ContextLength:         1_000_000,
				SupportedParameters:   []string{"tools"},
				AutoCompactTokenLimit: 800_000,
			},
			grokReasoning("grok-4.20-multi-agent-0309", "Grok 4.20 Multi-Agent", 1_000_000, []string{"low", "medium", "high", "xhigh"}, "high"),
		}
	}
	return append([]Model(nil), models...)
}

// curatedProviderModelAliases are accepted for explicit --model selections but
// omitted from the picker so aliases do not appear as duplicate model rows.
func curatedProviderModelAliases(provider string) []Model {
	switch provider {
	case providerOpenAI:
		return []Model{
			{ID: "gpt-5.6", Name: "GPT-5.6 (Sol alias)", ContextLength: 1_050_000, SupportedParameters: []string{"tools", "reasoning"}},
		}
	case providerXAI:
		// Documented aliases from https://docs.x.ai/developers/models
		return []Model{
			{ID: "grok-4.5-latest", Name: "Grok 4.5 (latest alias)", ContextLength: 500_000, SupportedParameters: []string{"tools", "reasoning"}, SupportedReasoningEfforts: []string{"low", "medium", "high"}, DefaultReasoningEffort: "high", DefaultReasoningSummary: "auto", AutoCompactTokenLimit: 400_000},
			{ID: "grok-build-latest", Name: "Grok Build (latest alias)", ContextLength: 500_000, SupportedParameters: []string{"tools", "reasoning"}, SupportedReasoningEfforts: []string{"low", "medium", "high"}, DefaultReasoningEffort: "high", DefaultReasoningSummary: "auto", AutoCompactTokenLimit: 400_000},
			{ID: "grok-4.3-latest", Name: "Grok 4.3 (latest alias)", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning"}, SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}, DefaultReasoningEffort: "low", DefaultReasoningSummary: "auto", AutoCompactTokenLimit: 800_000},
			{ID: "grok-latest", Name: "Grok (latest alias)", ContextLength: 1_000_000, SupportedParameters: []string{"tools", "reasoning"}, SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}, DefaultReasoningEffort: "low", DefaultReasoningSummary: "auto", AutoCompactTokenLimit: 800_000},
		}
	default:
		return nil
	}
}

type modelResolution struct {
	Model     Model
	Exact     bool
	Ambiguous []Model
}

func resolveModel(input string, models []Model) (modelResolution, bool) {
	needle := normalizeModelText(input)
	if needle == "" {
		return modelResolution{}, false
	}

	for _, m := range models {
		if normalizeModelText(m.ID) == needle {
			return modelResolution{Model: m, Exact: true}, true
		}
	}

	var matches []Model
	for _, m := range models {
		if modelMatches(needle, m) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return modelResolution{Model: matches[0]}, true
	}
	if len(matches) > 1 {
		return modelResolution{Ambiguous: matches}, true
	}
	return modelResolution{}, false
}

func modelMatches(needle string, m Model) bool {
	id := normalizeModelText(m.ID)
	name := normalizeModelText(m.Name)
	if id == needle || name == needle {
		return true
	}
	parts := strings.Split(id, "/")
	if len(parts) > 1 && parts[len(parts)-1] == needle {
		return true
	}
	return strings.Contains(name, needle)
}

func normalizeModelText(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func modelDescription(m Model) string {
	var parts []string
	if m.Name != "" && !strings.EqualFold(m.Name, m.ID) {
		parts = append(parts, m.Name)
	}
	if m.ContextLength > 0 {
		parts = append(parts, fmt.Sprintf("%d ctx", m.ContextLength))
	}
	if m.PromptPrice != "" || m.OutputPrice != "" {
		parts = append(parts, fmt.Sprintf("$%s/$%s", emptyDash(m.PromptPrice), emptyDash(m.OutputPrice)))
	}
	return strings.Join(parts, ", ")
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// orderModelsForPicker pins the curated recommended models to the top (in their
// curated order) and leaves everyone else in the incoming order — which is
// OpenRouter's most-popular ranking. The stable sort preserves that ranking
// for all equal-rank (non-recommended) models.
func orderModelsForPicker(models []Model) []Model {
	out := append([]Model(nil), models...)
	sort.SliceStable(out, func(i, j int) bool {
		return recommendedRank(out[i].ID) < recommendedRank(out[j].ID)
	})
	return out
}

func recommendedRank(modelID string) int {
	needle := normalizeModelText(modelID)
	for i, id := range recommendedModelIDs {
		if normalizeModelText(id) == needle {
			return i
		}
	}
	for i, id := range recommendedGeminiModelIDs {
		if normalizeModelText(id) == needle {
			return len(recommendedModelIDs) + i
		}
	}
	offset := len(recommendedModelIDs) + len(recommendedGeminiModelIDs)
	for i, id := range recommendedFirstClassModelIDs {
		if normalizeModelText(id) == needle {
			return offset + i
		}
	}
	return offset + len(recommendedFirstClassModelIDs)
}

func isRecommendedModel(modelID string) bool {
	return recommendedRank(modelID) < len(recommendedModelIDs)+len(recommendedGeminiModelIDs)+len(recommendedFirstClassModelIDs)
}

func isTestedModel(modelID string) bool {
	return testedModelIDs[normalizeModelText(modelID)]
}

func modelTags(m Model) []string {
	var tags []string
	if isRecommendedModel(m.ID) {
		tags = append(tags, "recommended")
	} else if isTestedModel(m.ID) {
		tags = append(tags, "tested")
	}
	if m.ContextLength >= 1_000_000 {
		tags = append(tags, "1m")
	}
	if supportsParameter(m, "tools") {
		tags = append(tags, "tools")
	}
	if supportsParameter(m, "reasoning") || supportsParameter(m, "reasoning_effort") || supportsParameter(m, "include_reasoning") {
		tags = append(tags, "reasoning")
	}
	if len(tags) == 0 {
		return []string{"standard"}
	}
	return tags
}

func supportsParameter(m Model, param string) bool {
	for _, got := range m.SupportedParameters {
		if strings.EqualFold(got, param) {
			return true
		}
	}
	return false
}

func formatContextLength(n int) string {
	if n <= 0 {
		return "-"
	}
	return commaInt(n)
}

func formatPricePerMillion(prompt, output string) string {
	return fmt.Sprintf("%s/%s", formatOneMillionPrice(prompt), formatOneMillionPrice(output))
}

func formatThroughput(tokensPerSecond float64) string {
	if tokensPerSecond <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d tps", int(math.Round(tokensPerSecond)))
}

func speedColor(tokensPerSecond float64) string {
	if tokensPerSecond <= 0 {
		return clrDim
	}
	return clrModelHi
}

func formatOneMillionPrice(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "-"
	}
	return "$" + trimPrice(value*1_000_000)
}

func trimPrice(value float64) string {
	var out string
	switch {
	case value >= 100:
		out = fmt.Sprintf("%.0f", value)
	case value >= 1:
		out = fmt.Sprintf("%.2f", value)
	default:
		out = fmt.Sprintf("%.3f", value)
	}
	out = strings.TrimRight(out, "0")
	out = strings.TrimRight(out, ".")
	if out == "" {
		return "0"
	}
	return out
}

func commaInt(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
