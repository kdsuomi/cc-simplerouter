package simplerouter

import "encoding/json"

// Gemini generateContent wire format (Google AI Studio, v1beta).
//
// A part carries exactly one data field (text, functionCall, functionResponse,
// or inlineData) plus optional metadata fields thought and thoughtSignature on
// the same part object — they are not standalone parts.

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenConfig  `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"` // "user" | "model"
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
	InlineData       *geminiBlob     `json:"inlineData,omitempty"`
}

type geminiFuncCall struct {
	ID   string          `json:"id,omitempty"` // Gemini 3+ AI Studio; echoed when present
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"` // opaque — never re-shaped
}

type geminiFuncResp struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"` // must be a JSON object
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // scrubbed OpenAPI-subset schema
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFuncCallingConfig `json:"functionCallingConfig"`
}

type geminiFuncCallingConfig struct {
	Mode                 string   `json:"mode"` // AUTO | ANY | NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiGenConfig struct {
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	MaxOutputTokens int                   `json:"maxOutputTokens,omitempty"`
	StopSequences   []string              `json:"stopSequences,omitempty"`
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"` // 2.5-generation models
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`  // gemini-3+; never set both
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
	Error         *geminiError      `json:"error,omitempty"` // mid-stream error chunks
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type geminiCountTokensResponse struct {
	TotalTokens int `json:"totalTokens"`
}

// Anthropic Messages API subset — only the fields the proxies translate.
// Fields absent from these structs (metadata, betas, ...) are intentionally
// dropped by encoding/json.

type anthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	System        json.RawMessage      `json:"system,omitempty"` // string OR []block
	Messages      []anthropicMessage   `json:"messages"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Thinking      *anthropicThinking   `json:"thinking,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OR []anthropicBlock
}

// anthropicBlock covers every incoming content block type, discriminated by Type.
type anthropicBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Thinking  string                `json:"thinking,omitempty"`
	Signature string                `json:"signature,omitempty"`
	Data      string                `json:"data,omitempty"`        // redacted_thinking
	ID        string                `json:"id,omitempty"`          // tool_use
	Name      string                `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage       `json:"input,omitempty"`       // tool_use args, opaque
	ToolUseID string                `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage       `json:"content,omitempty"`     // tool_result: string OR []block
	IsError   bool                  `json:"is_error,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"` // image
	// CacheControl preserves prompt-caching breakpoints for upstreams that
	// support them (OpenRouter); other proxies ignore it.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Type        string          `json:"type,omitempty"` // non-empty/non-"custom" = server tool stub, skipped
}

type anthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // enabled | disabled
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicMessageResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"` // "message"
	Role         string           `json:"role"` // "assistant"
	Model        string           `json:"model"`
	Content      []map[string]any `json:"content"`
	StopReason   string           `json:"stop_reason,omitempty"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}
