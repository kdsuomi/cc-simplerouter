package simplerouter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
)

const geminiInteractionReplayPrefix = "simplerouter-gemini-v1:"

type geminiInteractionReplayState struct {
	Version int               `json:"version"`
	Steps   []json.RawMessage `json:"steps"`
}

func encodeGeminiInteractionReplay(state geminiInteractionReplayState) string {
	if len(state.Steps) == 0 {
		return ""
	}
	state.Version = 1
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return geminiInteractionReplayPrefix + base64.RawStdEncoding.EncodeToString(raw)
}

func decodeGeminiInteractionReplay(encoded string) (geminiInteractionReplayState, bool) {
	if !strings.HasPrefix(encoded, geminiInteractionReplayPrefix) {
		return geminiInteractionReplayState{}, false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encoded, geminiInteractionReplayPrefix))
	if err != nil {
		return geminiInteractionReplayState{}, false
	}
	var state geminiInteractionReplayState
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 || len(state.Steps) == 0 {
		return geminiInteractionReplayState{}, false
	}
	return state, true
}

type geminiInteractionsProxy struct {
	upstreamBase     string
	model            string
	httpClient       *http.Client
	disableReasoning bool
}

func startGeminiInteractionsProxy(upstreamBase, model string, httpClient *http.Client, disableReasoning bool) (baseURL string, stop func(), err error) {
	proxy := newGeminiInteractionsProxy(upstreamBase, model, httpClient, disableReasoning)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: proxy}
	go func() { _ = server.Serve(listener) }()
	return fmt.Sprintf("http://%s/v1", listener.Addr().String()), func() { _ = server.Close() }, nil
}

func newGeminiInteractionsProxy(upstreamBase, model string, httpClient *http.Client, disableReasoning bool) *geminiInteractionsProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &geminiInteractionsProxy{
		upstreamBase:     strings.TrimRight(upstreamBase, "/"),
		model:            model,
		httpClient:       httpClient,
		disableReasoning: disableReasoning,
	}
}

func (p *geminiInteractionsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/responses"):
		p.handleResponses(w, r)
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{
			"id": p.model, "object": "model",
		}}})
	default:
		writeResponsesError(w, http.StatusNotFound, "not_found", "unknown route "+r.URL.Path)
	}
}

func (p *geminiInteractionsProxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "parse body: "+err.Error())
		return
	}
	if !req.Stream {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "Codex Responses requests must enable streaming")
		return
	}

	tools, registry, err := responsesToolsToGeminiInteractions(req.Tools, p.model)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	input, err := responsesInputToGeminiInteractions(&req, registry)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	upstream := map[string]any{
		"model":  p.model,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		upstream["system_instruction"] = instructions
	}
	if len(tools) > 0 {
		upstream["tools"] = tools
	}
	generationConfig := geminiInteractionGenerationConfig(p.model, &req, p.disableReasoning)
	if len(generationConfig) > 0 {
		upstream["generation_config"] = generationConfig
	}
	payload, err := json.Marshal(upstream)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "encode Gemini request: "+err.Error())
		return
	}

	endpoint := p.upstreamBase + "/interactions?alt=sse"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("x-goog-api-key", apiKeyFromRequest(r))

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "api_error", "Gemini request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayResponsesUpstreamError(w, resp, "Gemini")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	translator := newGeminiInteractionsResponsesTranslator(w, flusher, p.model, registry)
	if err := readCompatSSE(resp.Body, translator.onEvent); err != nil && !errors.Is(err, errStreamAborted) {
		translator.fail("stream_error", err.Error())
	}
	translator.finish()
}

func geminiInteractionGenerationConfig(model string, req *responsesRequest, disableReasoning bool) map[string]any {
	config := map[string]any{}
	if req.MaxOutputTokens > 0 {
		config["max_output_tokens"] = req.MaxOutputTokens
	}
	if choice := interactionToolChoice(req.ToolChoice); choice != "" {
		config["tool_choice"] = choice
	}
	if disableReasoning {
		config["thinking_level"] = geminiLowestThinkingLevel(model)
		config["thinking_summaries"] = "none"
		return config
	}
	if req.Reasoning != nil {
		if level := geminiThinkingLevelForModel(model, req.Reasoning.Effort); level != "" {
			config["thinking_level"] = level
		}
		if req.Reasoning.Summary == "none" {
			config["thinking_summaries"] = "none"
		} else {
			config["thinking_summaries"] = "auto"
		}
	}
	return config
}

func geminiThinkingLevel(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal":
		return "minimal"
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(effort))
	case "xhigh", "max", "ultra":
		return "high"
	default:
		return ""
	}
}

func geminiThinkingLevelForModel(model, effort string) string {
	level := geminiThinkingLevel(effort)
	if level == "minimal" && geminiLowestThinkingLevel(model) == "low" {
		return "low"
	}
	return level
}

func geminiLowestThinkingLevel(model string) string {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "2.5") ||
		strings.Contains(lower, "3.1-pro") ||
		strings.Contains(lower, "3-pro") {
		return "low"
	}
	return "minimal"
}

func interactionToolChoice(raw json.RawMessage) string {
	var choice string
	if len(raw) > 0 && json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "auto", "any", "none", "validated":
			return choice
		case "required":
			return "any"
		}
	}
	return "auto"
}

func responsesToolsToGeminiInteractions(rawTools []json.RawMessage, model string) ([]any, *responseToolRegistry, error) {
	registry := newResponseToolRegistry()
	var out []any
	for _, raw := range rawTools {
		var tool rawResponseTool
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, nil, fmt.Errorf("parse Responses tool: %w", err)
		}
		switch tool.Type {
		case "function":
			name := registry.register(responseToolIdentity{Name: tool.Name})
			out = append(out, geminiInteractionFunction(name, tool.Description, tool.Parameters))
		case "custom":
			name := registry.register(responseToolIdentity{Name: tool.Name, Custom: true})
			description := strings.TrimSpace(tool.Description)
			if len(tool.Format) > 0 && string(tool.Format) != "null" {
				description += "\n\nThe input must follow this format exactly:\n" + string(tool.Format)
			}
			parameters := json.RawMessage(`{
			  "type":"object",
			  "properties":{"input":{"type":"string","description":"Raw freeform tool input."}},
			  "required":["input"]
			}`)
			out = append(out, geminiInteractionFunction(name, description, parameters))
		case "namespace":
			for _, childRaw := range tool.Tools {
				var child rawResponseTool
				if err := json.Unmarshal(childRaw, &child); err != nil {
					return nil, nil, fmt.Errorf("parse Responses namespace tool: %w", err)
				}
				if child.Type != "function" {
					continue
				}
				name := registry.register(responseToolIdentity{Name: child.Name, Namespace: tool.Name})
				description := strings.TrimSpace(child.Description)
				if tool.Description != "" {
					description = strings.TrimSpace(tool.Description) + "\n\n" + description
				}
				out = append(out, geminiInteractionFunction(name, description, child.Parameters))
			}
		case "web_search", "web_search_preview":
			registry.webSearch = true
		}
	}
	// Gemini 3 supports combining built-in tools with function calling. Older
	// model families can use Google Search only when no client functions are
	// present, so avoid sending an invalid mixed-tool request.
	if registry.webSearch && (len(out) == 0 || geminiSupportsMixedTools(model)) {
		out = append(out, map[string]any{"type": "google_search"})
	}
	return out, registry, nil
}

func geminiSupportsMixedTools(model string) bool {
	return strings.Contains(strings.ToLower(model), "gemini-3")
}

// emptyGeminiFunctionParameters is the minimal JSON Schema Gemini's Interactions
// API accepts for parameterless tools. Omitting parameters, or sending null,
// fails with: "schema at top-level must be a boolean or an object".
var emptyGeminiFunctionParameters = json.RawMessage(`{"type":"object","properties":{}}`)

func geminiInteractionFunction(name, description string, parameters json.RawMessage) map[string]any {
	tool := map[string]any{
		"type": "function",
		"name": name,
	}
	if description != "" {
		tool["description"] = description
	}
	// Interactions requires parameters to be a boolean or object schema.
	// scrubJSONSchema returns nil for empty/unusable Codex schemas (e.g.
	// get_context_remaining / new_context with properties:{}), so fall back
	// to an empty object schema instead of omitting the field.
	schema := scrubJSONSchema(parameters)
	if len(schema) == 0 {
		schema = emptyGeminiFunctionParameters
	}
	if value, ok := rawJSONValue(schema); ok {
		tool["parameters"] = value
	} else {
		tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return tool
}

type interactionAssistantAccumulator struct {
	reconstructed []any
	replay        []json.RawMessage
	hasReplay     bool
}

func (a *interactionAssistantAccumulator) empty() bool {
	return len(a.reconstructed) == 0 && !a.hasReplay
}

func responsesInputToGeminiInteractions(req *responsesRequest, registry *responseToolRegistry) ([]any, error) {
	var steps []any
	pending := interactionAssistantAccumulator{}
	callNames := map[string]string{}
	flushAssistant := func() {
		if pending.empty() {
			return
		}
		if pending.hasReplay {
			for _, raw := range pending.replay {
				steps = append(steps, append(json.RawMessage(nil), raw...))
			}
		} else {
			steps = append(steps, pending.reconstructed...)
		}
		pending = interactionAssistantAccumulator{}
	}

	for _, raw := range req.Input {
		var header struct {
			Type             string          `json:"type"`
			Role             string          `json:"role"`
			Name             string          `json:"name"`
			Namespace        string          `json:"namespace"`
			CallID           string          `json:"call_id"`
			Arguments        string          `json:"arguments"`
			Input            string          `json:"input"`
			Output           json.RawMessage `json:"output"`
			Content          json.RawMessage `json:"content"`
			EncryptedContent string          `json:"encrypted_content"`
			Author           string          `json:"author"`
			Recipient        string          `json:"recipient"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, fmt.Errorf("parse Responses input item: %w", err)
		}
		switch header.Type {
		case "message":
			content := responseContentToGeminiInteraction(header.Content)
			if header.Role == "assistant" {
				pending.reconstructed = append(pending.reconstructed, map[string]any{
					"type":    "model_output",
					"content": content,
				})
				continue
			}
			flushAssistant()
			if header.Role == "developer" {
				content = append([]any{map[string]any{"type": "text", "text": "[Developer message]"}}, content...)
			}
			steps = append(steps, map[string]any{
				"type":    "user_input",
				"content": content,
			})
		case "agent_message":
			var content []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(header.Content, &content)
			var text strings.Builder
			for _, item := range content {
				text.WriteString(item.Text)
			}
			if text.Len() > 0 {
				pending.reconstructed = append(pending.reconstructed, map[string]any{
					"type": "model_output",
					"content": []any{map[string]any{
						"type": "text",
						"text": fmt.Sprintf("[%s -> %s]\n%s", header.Author, header.Recipient, text.String()),
					}},
				})
			}
		case "reasoning":
			if state, ok := decodeGeminiInteractionReplay(header.EncryptedContent); ok {
				pending.replay = append([]json.RawMessage(nil), state.Steps...)
				pending.hasReplay = true
				continue
			}
			var item struct {
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.Unmarshal(raw, &item)
			var text strings.Builder
			for _, part := range item.Content {
				text.WriteString(part.Text)
			}
			if text.Len() == 0 {
				for _, part := range item.Summary {
					text.WriteString(part.Text)
				}
			}
			if text.Len() > 0 {
				// Unsigned reasoning from another backend cannot be replayed
				// as a Gemini thought step. Preserve it as assistant context.
				pending.reconstructed = append(pending.reconstructed, map[string]any{
					"type": "model_output",
					"content": []any{map[string]any{
						"type": "text",
						"text": "[Prior reasoning summary]\n" + text.String(),
					}},
				})
			}
		case "function_call", "custom_tool_call":
			identity := responseToolIdentity{
				Name:      header.Name,
				Namespace: header.Namespace,
				Custom:    header.Type == "custom_tool_call",
			}
			name := registry.chatName(identity.Namespace, identity.Name)
			callNames[header.CallID] = name
			var arguments any
			if identity.Custom {
				arguments = map[string]any{"input": header.Input}
			} else if json.Unmarshal([]byte(header.Arguments), &arguments) != nil || arguments == nil {
				arguments = map[string]any{}
			}
			pending.reconstructed = append(pending.reconstructed, map[string]any{
				"type":      "function_call",
				"id":        header.CallID,
				"name":      name,
				"arguments": arguments,
			})
		case "function_call_output", "custom_tool_call_output":
			flushAssistant()
			result := map[string]any{
				"type":    "function_result",
				"call_id": header.CallID,
				"result": []any{map[string]any{
					"type": "text",
					"text": responseToolOutputText(header.Output),
				}},
			}
			if name := callNames[header.CallID]; name != "" {
				result["name"] = name
			}
			steps = append(steps, result)
		case "local_shell_call", "web_search_call", "image_generation_call":
			// Exact Gemini server steps are carried by the replay envelope.
			// For foreign history, retain a compact assistant marker.
			pending.reconstructed = append(pending.reconstructed, map[string]any{
				"type": "model_output",
				"content": []any{map[string]any{
					"type": "text",
					"text": "[" + header.Type + " completed]",
				}},
			})
		}
	}
	flushAssistant()
	return steps, nil
}

func responseContentToGeminiInteraction(raw json.RawMessage) []any {
	var content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		AudioURL string `json:"audio_url"`
	}
	if json.Unmarshal(raw, &content) != nil {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return []any{}
	}
	out := make([]any, 0, len(content))
	for _, item := range content {
		switch item.Type {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": item.Text})
		case "input_image":
			out = append(out, geminiInteractionMedia("image", item.ImageURL))
		case "input_audio":
			out = append(out, geminiInteractionMedia("audio", item.AudioURL))
		}
	}
	return out
}

func geminiInteractionMedia(kind, url string) map[string]any {
	content := map[string]any{"type": kind}
	if strings.HasPrefix(url, "data:") {
		metadata, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
		if ok {
			content["data"] = data
			content["mime_type"] = strings.TrimSuffix(metadata, ";base64")
			return content
		}
	}
	content["uri"] = url
	return content
}

type geminiInteractionEvent struct {
	EventType     string                 `json:"event_type"`
	InteractionID string                 `json:"interaction_id"`
	Status        string                 `json:"status"`
	Index         int                    `json:"index"`
	Step          json.RawMessage        `json:"step"`
	Delta         json.RawMessage        `json:"delta"`
	Usage         geminiInteractionUsage `json:"usage"`
	Metadata      *struct {
		TotalUsage geminiInteractionUsage `json:"total_usage"`
	} `json:"metadata"`
	Interaction *struct {
		ID     string                  `json:"id"`
		Model  string                  `json:"model"`
		Status string                  `json:"status"`
		Usage  geminiInteractionUsage  `json:"usage"`
		Error  *geminiInteractionError `json:"error"`
	} `json:"interaction"`
	Error *geminiInteractionError `json:"error"`
}

type geminiInteractionError struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

type geminiInteractionUsage struct {
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens"`
	TotalTokens        int `json:"total_tokens"`
}

type geminiInteractionStepState struct {
	index       int
	kind        string
	raw         map[string]any
	stopped     bool
	arguments   strings.Builder
	itemID      string
	reasoningID string
	messageID   string
	outputIndex int
	text        strings.Builder
}

type geminiInteractionsResponsesTranslator struct {
	out        *sseWriter
	model      string
	registry   *responseToolRegistry
	responseID string

	started       bool
	errored       bool
	terminal      bool
	status        string
	nextOutput    int
	functionCalls int
	steps         map[int]*geminiInteractionStepState
	completed     map[int]json.RawMessage
	usage         geminiInteractionUsage
}

func newGeminiInteractionsResponsesTranslator(w io.Writer, flush http.Flusher, model string, registry *responseToolRegistry) *geminiInteractionsResponsesTranslator {
	return &geminiInteractionsResponsesTranslator{
		out:        &sseWriter{w: w, flush: flush},
		model:      model,
		registry:   registry,
		responseID: "resp_gemini_" + newMessageID(),
		status:     "in_progress",
		steps:      map[int]*geminiInteractionStepState{},
		completed:  map[int]json.RawMessage{},
	}
}

func (t *geminiInteractionsResponsesTranslator) onEvent(raw json.RawMessage) error {
	var event geminiInteractionEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if event.Error != nil {
		t.fail(firstNonEmpty(event.Error.Code, event.Error.Type, "upstream_error"), event.Error.Message)
		return errStreamAborted
	}
	if event.Interaction != nil {
		if event.Interaction.Model != "" {
			t.model = event.Interaction.Model
		}
		if event.Interaction.Status != "" {
			t.status = event.Interaction.Status
		}
		if event.Interaction.Usage.TotalTokens != 0 ||
			event.Interaction.Usage.TotalInputTokens != 0 ||
			event.Interaction.Usage.TotalOutputTokens != 0 {
			t.usage = event.Interaction.Usage
		}
		if event.Interaction.Error != nil {
			t.fail(
				firstNonEmpty(event.Interaction.Error.Code, event.Interaction.Error.Type, "upstream_error"),
				event.Interaction.Error.Message,
			)
			return errStreamAborted
		}
	}
	t.captureUsage(event.Usage)
	if event.Metadata != nil {
		t.captureUsage(event.Metadata.TotalUsage)
	}
	if event.Status != "" {
		t.status = event.Status
	}

	switch event.EventType {
	case "interaction.created":
		t.start()
	case "interaction.in_progress":
		t.status = "in_progress"
		t.start()
	case "step.start":
		t.start()
		return t.startStep(event.Index, event.Step)
	case "step.delta":
		return t.deltaStep(event.Index, event.Delta)
	case "step.stop":
		return t.stopStep(event.Index)
	case "interaction.completed":
		if t.status == "" || t.status == "in_progress" {
			t.status = "completed"
		}
		t.terminal = true
		t.start()
	case "interaction.requires_action":
		t.status = "requires_action"
		t.terminal = true
		t.start()
	case "interaction.incomplete":
		t.status = "incomplete"
		t.terminal = true
		t.start()
	case "interaction.cancelled":
		t.status = "cancelled"
		t.terminal = true
		t.start()
	case "interaction.status_update":
		switch t.status {
		case "requires_action", "completed", "failed", "cancelled", "incomplete":
			t.terminal = true
		}
		t.start()
	case "interaction.failed", "error":
		t.fail("upstream_error", "Gemini interaction failed")
		return errStreamAborted
	}
	return nil
}

func (t *geminiInteractionsResponsesTranslator) captureUsage(usage geminiInteractionUsage) {
	if usage.TotalTokens != 0 ||
		usage.TotalInputTokens != 0 ||
		usage.TotalOutputTokens != 0 ||
		usage.TotalThoughtTokens != 0 ||
		usage.TotalCachedTokens != 0 {
		t.usage = usage
	}
}

func (t *geminiInteractionsResponsesTranslator) start() {
	if t.started {
		return
	}
	t.started = true
	t.out.event("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     t.responseID,
			"object": "response",
			"status": "in_progress",
			"model":  t.model,
		},
	})
}

func (t *geminiInteractionsResponsesTranslator) startStep(index int, raw json.RawMessage) error {
	var step map[string]any
	if err := json.Unmarshal(raw, &step); err != nil {
		return fmt.Errorf("parse Gemini step.start: %w", err)
	}
	kind, _ := step["type"].(string)
	state := &geminiInteractionStepState{
		index:       index,
		kind:        kind,
		raw:         step,
		outputIndex: -1,
	}
	t.steps[index] = state

	switch kind {
	case "thought":
		for _, text := range interactionContentTexts(step["summary"]) {
			t.emitThoughtDelta(state, text)
		}
	case "model_output":
		t.startModelOutput(state)
		for _, text := range interactionContentTexts(step["content"]) {
			t.emitModelText(state, text)
		}
	case "function_call":
		state.arguments.WriteString(interactionArgumentsString(step["arguments"]))
	}
	return nil
}

func (t *geminiInteractionsResponsesTranslator) deltaStep(index int, raw json.RawMessage) error {
	state := t.steps[index]
	if state == nil {
		return fmt.Errorf("Gemini step.delta for unknown index %d", index)
	}
	var delta map[string]any
	if err := json.Unmarshal(raw, &delta); err != nil {
		return fmt.Errorf("parse Gemini step.delta: %w", err)
	}
	argumentDelta, _ := delta["arguments_delta"].(string)
	if argumentDelta == "" {
		argumentDelta, _ = delta["arguments"].(string)
	}
	if argumentDelta != "" {
		value := argumentDelta
		state.arguments.WriteString(value)
		if state.kind == "function_call" {
			if state.itemID == "" {
				state.itemID = newToolUseID()
			}
			t.out.event("response.custom_tool_call_input.delta", map[string]any{
				"type":    "response.custom_tool_call_input.delta",
				"item_id": state.itemID,
				"call_id": state.itemID,
				"delta":   value,
			})
		}
	}
	kind, _ := delta["type"].(string)
	switch kind {
	case "thought_summary":
		if content, ok := delta["content"].(map[string]any); ok {
			appendInteractionContent(state.raw, "summary", content)
			if text, _ := content["text"].(string); text != "" {
				t.emitThoughtDelta(state, text)
			}
		}
	case "thought_signature":
		if signature, _ := delta["signature"].(string); signature != "" {
			state.raw["signature"] = signature
		}
	case "text":
		appendInteractionContent(state.raw, "content", delta)
		if text, _ := delta["text"].(string); text != "" {
			if state.kind == "thought" {
				t.emitThoughtDelta(state, text)
			} else {
				if state.kind == "model_output" && state.messageID == "" {
					t.startModelOutput(state)
				}
				t.emitModelText(state, text)
			}
		}
	case "text_annotation_delta":
		appendInteractionAnnotations(state.raw, delta["annotations"])
	case "arguments_delta":
		// Already accumulated from the field above.
	case "function_call":
		if argumentDelta == "" {
			mergeInteractionDelta(state.raw, delta)
		}
	default:
		mergeInteractionDelta(state.raw, delta)
	}
	return nil
}

func (t *geminiInteractionsResponsesTranslator) stopStep(index int) error {
	state := t.steps[index]
	if state == nil || state.stopped {
		return nil
	}
	state.stopped = true
	if state.arguments.Len() > 0 {
		var arguments any
		if json.Unmarshal([]byte(state.arguments.String()), &arguments) == nil {
			state.raw["arguments"] = arguments
		}
	}

	switch state.kind {
	case "thought":
		t.finishThought(state)
	case "model_output":
		t.finishModelOutput(state)
	case "function_call":
		t.emitFunctionCall(state)
	case "google_search_call":
		t.emitGoogleSearchCall(state)
	}
	encoded, err := json.Marshal(state.raw)
	if err != nil {
		return err
	}
	t.completed[index] = encoded
	return nil
}

func (t *geminiInteractionsResponsesTranslator) emitThoughtDelta(state *geminiInteractionStepState, delta string) {
	if delta == "" {
		return
	}
	if state.reasoningID == "" {
		state.reasoningID = "rs_" + newMessageID()
		state.outputIndex = t.nextOutput
		t.nextOutput++
		t.out.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": state.outputIndex,
			"item": map[string]any{
				"type":              "reasoning",
				"id":                state.reasoningID,
				"summary":           []any{},
				"encrypted_content": nil,
			},
		})
		t.out.event("response.reasoning_summary_part.added", map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       state.reasoningID,
			"summary_index": 0,
		})
	}
	state.text.WriteString(delta)
	t.out.event("response.reasoning_summary_text.delta", map[string]any{
		"type":          "response.reasoning_summary_text.delta",
		"item_id":       state.reasoningID,
		"summary_index": 0,
		"delta":         delta,
	})
	t.out.event("response.reasoning_summary_text.done", map[string]any{
		"type":          "response.reasoning_summary_text.done",
		"item_id":       state.reasoningID,
		"summary_index": 0,
		"text":          delta,
	})
}

func (t *geminiInteractionsResponsesTranslator) finishThought(state *geminiInteractionStepState) {
	if state.reasoningID == "" {
		return
	}
	t.out.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": state.outputIndex,
		"item":         reasoningResponseItem(state.reasoningID, state.text.String(), ""),
	})
}

func (t *geminiInteractionsResponsesTranslator) startModelOutput(state *geminiInteractionStepState) {
	if state.messageID != "" {
		return
	}
	state.messageID = "msg_" + newMessageID()
	state.outputIndex = t.nextOutput
	t.nextOutput++
	t.out.event("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": state.outputIndex,
		"item": map[string]any{
			"type":    "message",
			"id":      state.messageID,
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": ""}},
		},
	})
}

func (t *geminiInteractionsResponsesTranslator) emitModelText(state *geminiInteractionStepState, delta string) {
	if delta == "" {
		return
	}
	if state.messageID == "" {
		t.startModelOutput(state)
	}
	state.text.WriteString(delta)
	t.out.event("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       state.messageID,
		"output_index":  state.outputIndex,
		"content_index": 0,
		"delta":         delta,
	})
}

func (t *geminiInteractionsResponsesTranslator) finishModelOutput(state *geminiInteractionStepState) {
	if state.messageID == "" {
		t.startModelOutput(state)
	}
	t.out.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": state.outputIndex,
		"item": map[string]any{
			"type":  "message",
			"id":    state.messageID,
			"role":  "assistant",
			"phase": "final_answer",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": state.text.String(),
			}},
		},
	})
}

func (t *geminiInteractionsResponsesTranslator) emitFunctionCall(state *geminiInteractionStepState) {
	name, _ := state.raw["name"].(string)
	callID, _ := state.raw["id"].(string)
	if callID == "" {
		callID = newToolUseID()
	}
	identity, ok := t.registry.byChatName[name]
	if !ok {
		identity = responseToolIdentity{Name: name}
	}
	arguments := state.arguments.String()
	if arguments == "" {
		arguments = interactionArgumentsString(state.raw["arguments"])
	}
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	if state.itemID == "" {
		state.itemID = newToolUseID()
	}
	item := map[string]any{
		"type":      "function_call",
		"id":        state.itemID,
		"call_id":   callID,
		"name":      identity.Name,
		"arguments": arguments,
	}
	if identity.Namespace != "" {
		item["namespace"] = identity.Namespace
	}
	if identity.Custom {
		item["type"] = "custom_tool_call"
		delete(item, "id")
		delete(item, "arguments")
		item["input"] = customToolInput(arguments)
	}
	t.out.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": t.nextOutput,
		"item":         item,
	})
	t.nextOutput++
	t.functionCalls++
}

func (t *geminiInteractionsResponsesTranslator) emitGoogleSearchCall(state *geminiInteractionStepState) {
	id, _ := state.raw["id"].(string)
	if id == "" {
		id = "ws_" + newMessageID()
	}
	t.out.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": t.nextOutput,
		"item": map[string]any{
			"type":   "web_search_call",
			"id":     id,
			"status": "completed",
			"action": map[string]any{
				"type":  "search",
				"query": interactionSearchQuery(state.raw),
			},
		},
	})
	t.nextOutput++
}

func (t *geminiInteractionsResponsesTranslator) replayState() geminiInteractionReplayState {
	indexes := make([]int, 0, len(t.completed))
	for index := range t.completed {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	state := geminiInteractionReplayState{Version: 1}
	for _, index := range indexes {
		state.Steps = append(state.Steps, append(json.RawMessage(nil), t.completed[index]...))
	}
	return state
}

func (t *geminiInteractionsResponsesTranslator) fail(code, message string) {
	if t.errored {
		return
	}
	t.errored = true
	t.start()
	if message == "" {
		message = "Gemini interaction failed"
	}
	t.out.event("response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":     t.responseID,
			"status": "failed",
			"error": map[string]any{
				"type":    code,
				"code":    code,
				"message": message,
			},
		},
	})
}

func (t *geminiInteractionsResponsesTranslator) finish() {
	if t.errored {
		return
	}
	if !t.terminal {
		t.fail("stream_error", "Gemini interaction stream ended before a terminal event")
		return
	}
	t.start()
	indexes := make([]int, 0, len(t.steps))
	for index := range t.steps {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		_ = t.stopStep(index)
	}

	encodedState := encodeGeminiInteractionReplay(t.replayState())
	if encodedState != "" {
		t.out.event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": t.nextOutput,
			"item":         reasoningResponseItem("rs_state_"+newMessageID(), "", encodedState),
		})
		t.nextOutput++
	}

	outputTokens := t.usage.TotalOutputTokens + t.usage.TotalThoughtTokens
	totalTokens := t.usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = t.usage.TotalInputTokens + outputTokens
	}
	response := map[string]any{
		"id":       t.responseID,
		"object":   "response",
		"status":   "completed",
		"model":    t.model,
		"end_turn": t.functionCalls == 0,
		"usage": map[string]any{
			"input_tokens": t.usage.TotalInputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": t.usage.TotalCachedTokens,
			},
			"output_tokens": outputTokens,
			"output_tokens_details": map[string]any{
				"reasoning_tokens": t.usage.TotalThoughtTokens,
			},
			"total_tokens": totalTokens,
		},
	}
	switch t.status {
	case "completed", "requires_action":
		t.out.event("response.completed", map[string]any{
			"type":     "response.completed",
			"response": response,
		})
	case "incomplete", "budget_exceeded":
		response["status"] = "incomplete"
		response["error"] = nil
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		t.out.event("response.incomplete", map[string]any{
			"type":     "response.incomplete",
			"response": response,
		})
	case "failed":
		t.fail("upstream_error", "Gemini interaction failed")
	case "cancelled":
		t.fail("cancelled", "Gemini interaction was cancelled")
	default:
		t.fail("stream_error", "Gemini interaction ended with unexpected status "+t.status)
	}
}

func interactionContentTexts(value any) []string {
	items, _ := value.([]any)
	var out []string
	for _, item := range items {
		content, _ := item.(map[string]any)
		if text, _ := content["text"].(string); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func appendInteractionContent(step map[string]any, field string, incoming map[string]any) {
	items, _ := step[field].([]any)
	if len(items) > 0 {
		last, _ := items[len(items)-1].(map[string]any)
		lastType, _ := last["type"].(string)
		incomingType, _ := incoming["type"].(string)
		if lastType == "text" && incomingType == "text" {
			lastText, _ := last["text"].(string)
			incomingText, _ := incoming["text"].(string)
			last["text"] = lastText + incomingText
			for key, value := range incoming {
				if key == "type" || key == "text" {
					continue
				}
				mergeInteractionValue(last, key, value)
			}
			items[len(items)-1] = last
			step[field] = items
			return
		}
	}
	step[field] = append(items, incoming)
}

func appendInteractionAnnotations(step map[string]any, value any) {
	annotations, _ := value.([]any)
	if len(annotations) == 0 {
		return
	}
	content, _ := step["content"].([]any)
	for index := len(content) - 1; index >= 0; index-- {
		item, _ := content[index].(map[string]any)
		if item["type"] != "text" {
			continue
		}
		current, _ := item["annotations"].([]any)
		item["annotations"] = append(current, annotations...)
		content[index] = item
		step["content"] = content
		return
	}
}

func mergeInteractionValue(target map[string]any, key string, value any) {
	switch incoming := value.(type) {
	case []any:
		current, _ := target[key].([]any)
		target[key] = append(current, incoming...)
	case map[string]any:
		current, _ := target[key].(map[string]any)
		if current == nil {
			current = map[string]any{}
		}
		mergeJSONObjects(current, incoming)
		target[key] = current
	default:
		target[key] = value
	}
}

func mergeInteractionDelta(step, delta map[string]any) {
	for key, value := range delta {
		if key == "type" {
			continue
		}
		mergeInteractionValue(step, key, value)
	}
}

func interactionArgumentsString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func interactionSearchQuery(step map[string]any) string {
	arguments, _ := step["arguments"].(map[string]any)
	if query, _ := arguments["query"].(string); query != "" {
		return query
	}
	if values, ok := arguments["queries"].([]any); ok {
		var queries []string
		for _, value := range values {
			if query, ok := value.(string); ok {
				queries = append(queries, query)
			}
		}
		return strings.Join(queries, "; ")
	}
	if query, _ := step["query"].(string); query != "" {
		return query
	}
	return "Google Search"
}
