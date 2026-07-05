package simplerouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var errProviderKeyRejected = errors.New("provider rejected the API key")

const (
	defaultOpenAIAPIBase   = "https://api.openai.com/v1"
	defaultDeepSeekAPIBase = "https://api.deepseek.com"
	defaultZAIAPIBase      = "https://api.z.ai/api/paas/v4"
)

func validateBearerModels(ctx context.Context, httpClient *http.Client, apiBase, key, label string) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiBase, "/")+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("validate %s key: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errProviderKeyRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s key validation failed: HTTP %d", label, resp.StatusCode)
	}
	return nil
}
