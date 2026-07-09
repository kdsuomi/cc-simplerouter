package simplerouter

import (
	"fmt"
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
	"gemini-3.1-pro-preview",
	"gemini-3.5-flash",
	"gemini-2.5-pro",
	"gemini-2.5-flash",
}

var recommendedFirstClassModelIDs = []string{
	"muse-spark-1.1",
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
	"muse-spark-1.1":               true,
}

func curatedProviderModels(provider string) []Model {
	var models []Model
	switch provider {
	case providerOpenAI:
		models = []Model{
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
		models = []Model{
			{ID: "muse-spark-1.1", Name: "Muse Spark 1.1", ContextLength: 1_048_576, SupportedParameters: []string{"tools", "reasoning"}},
		}
	}
	return append([]Model(nil), models...)
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

func modelWarning(m Model) string {
	id := normalizeModelText(m.ID)
	if strings.HasPrefix(id, "openai/gpt-5") {
		return "known GPT-5 Claude Code issue"
	}
	return ""
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
