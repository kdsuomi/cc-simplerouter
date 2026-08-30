package simplerouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// errOpenRouterKeyRejected signals that OpenRouter explicitly rejected the
// API key (HTTP 401/403). Every other failure — network error, timeout, 429
// rate limit, 5xx — is transient and must NOT be treated as an invalid key,
// so callers can proceed optimistically with the key instead of forcing a
// re-paste on a flaky connection.
var errOpenRouterKeyRejected = errors.New("OpenRouter rejected the API key")

const (
	defaultOpenRouterAPIBase = "https://openrouter.ai/api/v1"
)

type openRouterClient struct {
	httpClient  *http.Client
	apiBase     string
	zdrOnce     sync.Once
	zdrPolicies map[string]bool
	zdrErr      error
}

func newOpenRouterClient(httpClient *http.Client, apiBase string) *openRouterClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(apiBase) == "" {
		apiBase = defaultOpenRouterAPIBase
	}
	return &openRouterClient{httpClient: httpClient, apiBase: strings.TrimRight(apiBase, "/")}
}

func (c *openRouterClient) validateKey(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/key", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("validate OpenRouter key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errOpenRouterKeyRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OpenRouter key validation failed: HTTP %d", resp.StatusCode)
	}
	var out openRouterKeyResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return nil
}

func (c *openRouterClient) models(ctx context.Context, key string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	// Trim the catalog to models useful in Codex and order by popularity:
	// text output, tool-calling support, most-popular first. This drops ~90
	// junk/unusable entries (image-only, no-tools, obscure) before display.
	q := req.URL.Query()
	q.Set("output_modalities", "text")
	q.Set("supported_parameters", "tools")
	q.Set("sort", "most-popular")
	req.URL.RawQuery = q.Encode()
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenRouter models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch OpenRouter models: HTTP %d", resp.StatusCode)
	}

	var raw openRouterModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode OpenRouter models: %w", err)
	}
	models := make([]Model, 0, len(raw.Data))
	for _, m := range raw.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		models = append(models, Model{
			ID:                  id,
			Name:                strings.TrimSpace(m.Name),
			ContextLength:       m.ContextLength,
			PromptPrice:         strings.TrimSpace(m.Pricing.Prompt),
			OutputPrice:         strings.TrimSpace(m.Pricing.Completion),
			Privacy:             "non-zdr",
			SupportedParameters: cleanSupportedParameters(m.SupportedParameters),
		})
	}
	// Preserve OpenRouter's most-popular ordering from the query above.
	return models, nil
}

// endpoints lists the provider endpoints currently serving a model, fastest
// measured throughput first.
func (c *openRouterClient) endpoints(ctx context.Context, key, modelID string) ([]Endpoint, error) {
	author, slug, ok := strings.Cut(strings.TrimSpace(modelID), "/")
	if !ok || author == "" || slug == "" {
		return nil, fmt.Errorf("invalid model id %q", modelID)
	}
	url := c.apiBase + "/models/" + author + "/" + slug + "/endpoints"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch endpoints: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch endpoints: HTTP %d", resp.StatusCode)
	}
	var raw openRouterEndpointsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode endpoints: %w", err)
	}
	zdr, err := c.zdrPolicy(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]Endpoint, 0, len(raw.Data.Endpoints))
	for _, e := range raw.Data.Endpoints {
		tag := strings.TrimSpace(e.Tag)
		out = append(out, Endpoint{
			ProviderName:  strings.TrimSpace(e.ProviderName),
			Tag:           tag,
			Quantization:  strings.TrimSpace(e.Quantization),
			ContextLength: e.ContextLength,
			PromptPrice:   strings.TrimSpace(e.Pricing.Prompt),
			OutputPrice:   strings.TrimSpace(e.Pricing.Completion),
			ThroughputP50: e.ThroughputLast30m.P50,
			Privacy:       endpointPrivacy(modelID, tag, zdr),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ThroughputP50 > out[j].ThroughputP50
	})
	return out, nil
}

func (c *openRouterClient) zdrPolicy(ctx context.Context, key string) (map[string]bool, error) {
	c.zdrOnce.Do(func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/endpoints/zdr", nil)
		if err != nil {
			c.zdrErr = err
			return
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.zdrErr = fmt.Errorf("fetch zero-data-retention endpoints: %w", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.zdrErr = fmt.Errorf("fetch zero-data-retention endpoints: HTTP %d", resp.StatusCode)
			return
		}
		var raw openRouterZDRResponse
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			c.zdrErr = fmt.Errorf("decode zero-data-retention endpoints: %w", err)
			return
		}
		policies := make(map[string]bool, len(raw.Data))
		for _, endpoint := range raw.Data {
			modelID := strings.TrimSpace(endpoint.ModelID)
			tag := strings.TrimSpace(endpoint.Tag)
			if modelID == "" || tag == "" {
				continue
			}
			policies[modelID+"\x00"+tag] = true
		}
		c.zdrPolicies = policies
	})
	return c.zdrPolicies, c.zdrErr
}

func endpointPrivacy(modelID, tag string, zdr map[string]bool) string {
	if zdr[modelID+"\x00"+tag] {
		return "zdr"
	}
	return "non-zdr"
}

func cleanSupportedParameters(params []string) []string {
	out := make([]string, 0, len(params))
	seen := make(map[string]bool, len(params))
	for _, param := range params {
		param = strings.TrimSpace(param)
		if param == "" || seen[param] {
			continue
		}
		seen[param] = true
		out = append(out, param)
	}
	return out
}
