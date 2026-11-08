package simplerouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (a *app) abliterationBase() string {
	if strings.TrimSpace(a.abliterationAPIBase) != "" {
		return a.abliterationAPIBase
	}
	return defaultAbliterationAPIBase
}

func (a *app) abliterationKey(ctx context.Context, cfg Config) (string, error) {
	return a.providerKey(ctx, "Abliteration", []string{"ABLITERATION_API_KEY", "ABLIT_KEY"}, cfg.AbliterationAPIKey, func(ctx context.Context, key string) error {
		return validateBearerModelsKey(ctx, a.httpClient, a.abliterationBase(), key, "Abliteration")
	})
}

func (a *app) selectAbliteration(ctx context.Context, cfg Config, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	key, err := a.abliterationKey(ctx, cfg)
	if err != nil {
		return "", pickResult{}, err
	}
	if firstRun {
		fmt.Fprintln(a.stderr, style.paint(clrDim, "Fetching Abliteration models..."))
	}
	models, err := abliterationModels(ctx, a.httpClient, a.abliterationBase(), key)
	if err != nil {
		return "", pickResult{}, err
	}
	modelID := strings.TrimSpace(modelFlag)
	if selectModel || modelID == "" {
		current := cfg.LastAbliterationModel
		if modelID != "" {
			current = modelID
		}
		res, err := a.pickModel("Select an Abliteration model", models, current, nil)
		return key, res, err
	}
	res, err := a.resolveStaticModel("Abliteration", models, modelID)
	return key, res, err
}

// GET /models supplies project access, current pricing, and context windows.
// https://docs.abliteration.ai/api-reference/models/list-models-available-to-the-api-key-project
func abliterationModels(ctx context.Context, httpClient *http.Client, apiBase, key string) ([]Model, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiBase, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Abliteration models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("fetch Abliteration models: %w", errProviderKeyRejected)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch Abliteration models: HTTP %d", resp.StatusCode)
	}
	var catalog struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			DisplayName   string `json:"display_name"`
			ContextLength int    `json:"context_length"`
			IsReady       *bool  `json:"is_ready"`
			Access        *struct {
				Available *bool `json:"available"`
				Locked    bool  `json:"locked"`
			} `json:"access"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode Abliteration models: %w", err)
	}
	defaults := abliterationModelDefaults()
	models := make([]Model, 0, len(catalog.Data))
	seen := make(map[string]bool)
	for _, item := range catalog.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] || (item.IsReady != nil && !*item.IsReady) ||
			(item.Access != nil && (item.Access.Locked || (item.Access.Available != nil && !*item.Access.Available))) {
			continue
		}
		seen[id] = true
		model := abliterationModelDefault(id, id, 262_144)
		for _, known := range defaults {
			if known.ID == id {
				model = known
				break
			}
		}
		if name := strings.TrimSpace(item.DisplayName); name != "" {
			model.Name = name
		} else if name := strings.TrimSpace(item.Name); name != "" {
			model.Name = name
		}
		if item.ContextLength > 0 {
			model.ContextLength = item.ContextLength
			model.AutoCompactTokenLimit = item.ContextLength * 4 / 5
		}
		model.PromptPrice, model.OutputPrice = item.Pricing.Prompt, item.Pricing.Completion
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("Abliteration reported no available models for this API key")
	}
	return models, nil
}

// Codex exposes xhigh as its deepest effort. Large models map it to max;
// the base model rejects literal max on Responses and retains xhigh.
// https://docs.abliteration.ai/models
// https://docs.abliteration.ai/capabilities/thinking
func abliterationModelDefault(id, name string, contextLength int) Model {
	model := Model{
		ID: id, Name: name, ContextLength: contextLength,
		SupportedParameters:       []string{"tools", "reasoning"},
		SupportedReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"},
		DefaultReasoningEffort:    "high",
		DefaultReasoningSummary:   "auto",
		AutoCompactTokenLimit:     contextLength * 4 / 5,
	}
	switch id {
	case "abliterated-model-large-v2":
		model.SupportedReasoningEfforts = []string{"low", "high", "xhigh"}
		model.DefaultReasoningEffort = "xhigh"
	case "abliterated-model-large-flash":
		model.SupportedReasoningEfforts = []string{"none", "low", "high", "xhigh"}
	case "abliterated-model-large":
		model.SupportedReasoningEfforts = []string{"none", "high", "xhigh"}
	}
	return model
}

func abliterationModelDefaults() []Model {
	return []Model{
		abliterationModelDefault("abliterated-model-large-v2", "Abliterated Large v2", 1_000_000),
		abliterationModelDefault("abliterated-model-large-flash", "Abliterated Large Flash", 1_048_576),
		abliterationModelDefault("abliterated-model", "Abliterated Model", 262_144),
		abliterationModelDefault("abliterated-model-large", "Abliterated Large (deprecated)", 1_000_000),
	}
}

func abliterationResponsesOptions(model string) responsesPassthroughOptions {
	effortMap := map[string]string{"ultra": "max", "xhigh": "max"}
	if model == "abliterated-model" {
		effortMap = map[string]string{"ultra": "xhigh", "max": "xhigh"}
	}
	return responsesPassthroughOptions{
		Label:                     "Abliteration",
		ReasoningEffortMap:        effortMap,
		OmitNoneReasoningSummary:  true,
		RecoverCompletedToolCalls: true,
		TranslateCustomTools:      true,
		FlattenNamespaces:         true,
		AllowedToolTypes:          []string{"function", "web_search", "web_search_preview"},
	}
}
