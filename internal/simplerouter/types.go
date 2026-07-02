package simplerouter

type Config struct {
	Provider         string `json:"provider,omitempty"` // "openrouter" | "gemini"; empty = openrouter
	OpenRouterAPIKey string `json:"openrouter_api_key,omitempty"`
	GeminiAPIKey     string `json:"gemini_api_key,omitempty"`
	LastModel        string `json:"last_model,omitempty"` // OpenRouter last model (legacy key name)
	LastGeminiModel  string `json:"last_gemini_model,omitempty"`
}

type Model struct {
	ID                  string
	Name                string
	ContextLength       int
	PromptPrice         string
	OutputPrice         string
	SupportedParameters []string
}

// Endpoint is one provider serving a model (from /models/:id/endpoints).
type Endpoint struct {
	ProviderName  string
	Tag           string // OpenRouter routing slug, e.g. "deepinfra/fp4"
	Quantization  string
	ContextLength int
	PromptPrice   string
	OutputPrice   string
}

type openRouterEndpointsResponse struct {
	Data struct {
		Endpoints []struct {
			ProviderName  string `json:"provider_name"`
			Tag           string `json:"tag"`
			Quantization  string `json:"quantization"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"endpoints"`
	} `json:"data"`
}

type openRouterModelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		Pricing       struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

type openRouterKeyResponse struct {
	Data map[string]any `json:"data"`
}

type geminiModelsResponse struct {
	Models []struct {
		Name                       string   `json:"name"` // "models/gemini-2.5-flash"
		DisplayName                string   `json:"displayName"`
		InputTokenLimit            int      `json:"inputTokenLimit"`
		OutputTokenLimit           int      `json:"outputTokenLimit"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

type launchSpec struct {
	Path string
	Dir  string
	Args []string
	Env  []string
}
