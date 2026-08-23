package simplerouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const lmStudioRequestTimeout = 10 * time.Second

type lmStudioReasoningCapability struct {
	AllowedOptions []string `json:"allowed_options"`
	Default        string   `json:"default"`
}

type lmStudioModelsResponse struct {
	Models []struct {
		Type             string `json:"type"`
		Key              string `json:"key"`
		DisplayName      string `json:"display_name"`
		MaxContextLength int    `json:"max_context_length"`
		Capabilities     struct {
			TrainedForToolUse bool            `json:"trained_for_tool_use"`
			Reasoning         json.RawMessage `json:"reasoning"`
		} `json:"capabilities"`
		LoadedInstances []struct {
			Config struct {
				ContextLength int `json:"context_length"`
			} `json:"config"`
		} `json:"loaded_instances"`
	} `json:"models"`
}

func lmStudioModels(ctx context.Context, httpClient *http.Client, apiBase string) ([]Model, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(apiBase) == "" {
		apiBase = defaultLMStudioAPIBase
	}
	ctx, cancel := context.WithTimeout(ctx, lmStudioRequestTimeout)
	defer cancel()

	endpoint := lmStudioRoot(apiBase) + "/api/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to LM Studio at %s: %w (start its Local Server first)", strings.TrimRight(apiBase, "/"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		detail := strings.TrimSpace(string(body))
		if detail != "" {
			return nil, fmt.Errorf("fetch LM Studio models: HTTP %d: %s", resp.StatusCode, detail)
		}
		return nil, fmt.Errorf("fetch LM Studio models: HTTP %d", resp.StatusCode)
	}

	var raw lmStudioModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode LM Studio models: %w", err)
	}
	models := make([]Model, 0, len(raw.Models))
	for _, item := range raw.Models {
		id := strings.TrimSpace(item.Key)
		if !strings.EqualFold(strings.TrimSpace(item.Type), "llm") || id == "" {
			continue
		}
		contextLength := item.MaxContextLength
		for _, instance := range item.LoadedInstances {
			if instance.Config.ContextLength > 0 {
				contextLength = instance.Config.ContextLength
				break
			}
		}
		params := []string(nil)
		if item.Capabilities.TrainedForToolUse {
			params = append(params, "tools")
		}
		reasoning := parseLMStudioReasoningCapability(item.Capabilities.Reasoning)
		efforts, defaultEffort := lmStudioReasoningMetadata(reasoning)
		defaultSummary := ""
		if reasoning != nil {
			params = append(params, "reasoning")
			defaultSummary = "auto"
		}
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = id
		}
		models = append(models, Model{
			ID:                        id,
			Name:                      name,
			ContextLength:             contextLength,
			SupportedParameters:       params,
			SupportedReasoningEfforts: efforts,
			DefaultReasoningEffort:    defaultEffort,
			DefaultReasoningSummary:   defaultSummary,
			AutoCompactTokenLimit:     contextLength * 4 / 5,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("LM Studio reported no installed LLMs at %s", endpoint)
	}
	return models, nil
}

func lmStudioRoot(apiBase string) string {
	root := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if strings.HasSuffix(strings.ToLower(root), "/v1") {
		root = strings.TrimRight(root[:len(root)-len("/v1")], "/")
	}
	return root
}

func parseLMStudioReasoningCapability(raw json.RawMessage) *lmStudioReasoningCapability {
	var reasoning lmStudioReasoningCapability
	if json.Unmarshal(raw, &reasoning) != nil || (len(reasoning.AllowedOptions) == 0 && strings.TrimSpace(reasoning.Default) == "") {
		return nil
	}
	return &reasoning
}

func lmStudioReasoningMetadata(reasoning *lmStudioReasoningCapability) ([]string, string) {
	if reasoning == nil {
		return nil, ""
	}
	normalize := func(value string) string {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "off", "none":
			return "none"
		case "minimal", "low", "medium", "high", "xhigh":
			return strings.ToLower(strings.TrimSpace(value))
		default:
			return ""
		}
	}
	seen := make(map[string]bool)
	efforts := make([]string, 0, len(reasoning.AllowedOptions))
	for _, option := range reasoning.AllowedOptions {
		if effort := normalize(option); effort != "" && !seen[effort] {
			seen[effort] = true
			efforts = append(efforts, effort)
		}
	}
	return efforts, normalize(reasoning.Default)
}
