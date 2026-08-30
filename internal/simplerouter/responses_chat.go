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

const chatReplayStatePrefix = "simplerouter-chat-v1:"

type responsesRequest struct {
	Model             string              `json:"model"`
	Instructions      string              `json:"instructions"`
	Input             []json.RawMessage   `json:"input"`
	Tools             []json.RawMessage   `json:"tools"`
	ToolChoice        json.RawMessage     `json:"tool_choice"`
	ParallelToolCalls bool                `json:"parallel_tool_calls"`
	Reasoning         *responsesReasoning `json:"reasoning"`
	Stream            bool                `json:"stream"`
	MaxOutputTokens   int                 `json:"max_output_tokens"`
}

type responsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type responsesChatProxyOptions struct {
	Label                   string
	ChatPath                string
	DisableReasoning        bool
	SendReasoningEffort     bool
	SendNoneReasoningEffort bool
	ReasoningReplayField    string
	ReasoningEffortMap      map[string]string
	IncludeStreamUsage      bool
	ToolStream              bool
	ExtraBody               map[string]any
	ScrubToolSchemas        bool
}

type responsesChatProxy struct {
	upstreamBase string
	model        string
	httpClient   *http.Client
	options      responsesChatProxyOptions
}

func startResponsesChatProxy(upstreamBase, model string, httpClient *http.Client, options responsesChatProxyOptions) (baseURL string, stop func(), err error) {
	proxy := newResponsesChatProxy(upstreamBase, model, httpClient, options)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: proxy}
	go func() { _ = server.Serve(listener) }()
	return fmt.Sprintf("http://%s/v1", listener.Addr().String()), func() { _ = server.Close() }, nil
}

func newResponsesChatProxy(upstreamBase, model string, httpClient *http.Client, options responsesChatProxyOptions) *responsesChatProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(options.Label) == "" {
		options.Label = "upstream"
	}
	if strings.TrimSpace(options.ChatPath) == "" {
		options.ChatPath = "/chat/completions"
	}
	if strings.TrimSpace(options.ReasoningReplayField) == "" {
		options.ReasoningReplayField = "reasoning_content"
	}
	return &responsesChatProxy{
		upstreamBase: strings.TrimRight(upstreamBase, "/"),
		model:        model,
		httpClient:   httpClient,
		options:      options,
	}
}

func (p *responsesChatProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (p *responsesChatProxy) handleResponses(w http.ResponseWriter, r *http.Request) {
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
	chatTools, registry, err := responsesToolsToChat(req.Tools, p.options.ScrubToolSchemas)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	messages, err := responsesInputToChat(&req, registry, p.options.ReasoningReplayField)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	upstream := map[string]any{
		"model":    p.model,
		"messages": messages,
		"stream":   true,
	}
	if len(chatTools) > 0 {
		upstream["tools"] = chatTools
		upstream["tool_choice"] = chatToolChoice(req.ToolChoice)
		upstream["parallel_tool_calls"] = req.ParallelToolCalls
	}
	if p.options.SendReasoningEffort && !p.options.DisableReasoning && req.Reasoning != nil {
		if effort := mappedReasoningEffort(req.Reasoning.Effort, p.options.ReasoningEffortMap); effort != "" &&
			(effort != "none" || p.options.SendNoneReasoningEffort) {
			upstream["reasoning_effort"] = effort
		}
	}
	if req.MaxOutputTokens > 0 {
		upstream["max_completion_tokens"] = req.MaxOutputTokens
	}
	if p.options.IncludeStreamUsage {
		upstream["stream_options"] = map[string]any{"include_usage": true}
	}
	if p.options.ToolStream {
		upstream["tool_stream"] = true
	}
	for key, value := range p.options.ExtraBody {
		upstream[key] = value
	}
	payload, err := json.Marshal(upstream)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "encode upstream request: "+err.Error())
		return
	}

	endpoint := p.upstreamBase + "/" + strings.TrimLeft(p.options.ChatPath, "/")
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("Authorization", "Bearer "+apiKeyFromRequest(r))

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "api_error", p.options.Label+" request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayResponsesUpstreamError(w, resp, p.options.Label)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	translator := newChatResponsesStreamTranslator(w, flusher, p.model, registry)
	if err := readCompatSSE(resp.Body, translator.onEvent); err != nil && !errors.Is(err, errStreamAborted) {
		translator.fail("stream_error", err.Error())
	}
	translator.finish()
}

func writeResponsesError(w http.ResponseWriter, status int, code, message string) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if code == "" {
		code = "api_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"type":    code,
		"code":    code,
		"message": message,
	}})
}

func relayResponsesUpstreamError(w http.ResponseWriter, resp *http.Response, label string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(body) > 0 && json.Valid(body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = label + " request failed"
	}
	writeResponsesError(w, resp.StatusCode, "upstream_error", message)
}

func mappedReasoningEffort(effort string, mapping map[string]string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if mapped := strings.TrimSpace(mapping[effort]); mapped != "" {
		return mapped
	}
	return effort
}

func chatToolChoice(raw json.RawMessage) any {
	var choice any
	if len(raw) != 0 && json.Unmarshal(raw, &choice) == nil && choice != nil {
		switch value := choice.(type) {
		case string:
			return value
		case map[string]any:
			if value["type"] == "function" {
				if name, _ := value["name"].(string); name != "" {
					return map[string]any{"type": "function", "function": map[string]any{"name": name}}
				}
			}
		}
	}
	return "auto"
}

type responseToolIdentity struct {
	Name      string
	Namespace string
	Custom    bool
}

type responseToolRegistry struct {
	byChatName map[string]responseToolIdentity
	byIdentity map[string]string
	webSearch  bool
}

func newResponseToolRegistry() *responseToolRegistry {
	return &responseToolRegistry{
		byChatName: map[string]responseToolIdentity{},
		byIdentity: map[string]string{},
	}
}

func (r *responseToolRegistry) register(identity responseToolIdentity) string {
	key := identity.Namespace + "\x00" + identity.Name
	if existing := r.byIdentity[key]; existing != "" {
		return existing
	}
	name := identity.Name
	if identity.Namespace != "" {
		name = identity.Namespace + "__" + identity.Name
	}
	name = sanitizeChatToolName(name)
	base := name
	for suffix := 2; r.byChatName[name].Name != ""; suffix++ {
		name = fmt.Sprintf("%s_%d", base, suffix)
	}
	r.byIdentity[key] = name
	r.byChatName[name] = identity
	return name
}

func (r *responseToolRegistry) chatName(namespace, name string) string {
	if got := r.byIdentity[namespace+"\x00"+name]; got != "" {
		return got
	}
	return r.register(responseToolIdentity{Name: name, Namespace: namespace})
}

func sanitizeChatToolName(name string) string {
	var out strings.Builder
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_',
			char == '-':
			out.WriteRune(char)
		default:
			out.WriteByte('_')
		}
	}
	got := strings.Trim(out.String(), "_")
	if got == "" {
		got = "tool"
	}
	if len(got) > 64 {
		got = got[:64]
	}
	return got
}

type rawResponseTool struct {
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Parameters  json.RawMessage   `json:"parameters"`
	Format      json.RawMessage   `json:"format"`
	Strict      bool              `json:"strict"`
	Tools       []json.RawMessage `json:"tools"`
}

func responsesToolsToChat(rawTools []json.RawMessage, scrubSchemas bool) ([]any, *responseToolRegistry, error) {
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
			out = append(out, chatFunctionTool(name, tool.Description, tool.Parameters, scrubSchemas))
		case "custom":
			name := registry.register(responseToolIdentity{Name: tool.Name, Custom: true})
			description := strings.TrimSpace(tool.Description)
			if len(tool.Format) > 0 && string(tool.Format) != "null" {
				description += "\n\nThe input must follow this format exactly:\n" + string(tool.Format)
			}
			parameters := json.RawMessage(`{
			  "type":"object",
			  "properties":{"input":{"type":"string","description":"Raw freeform tool input."}},
			  "required":["input"],
			  "additionalProperties":false
			}`)
			out = append(out, chatFunctionTool(name, description, parameters, scrubSchemas))
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
				out = append(out, chatFunctionTool(name, description, child.Parameters, scrubSchemas))
			}
		case "web_search", "web_search_preview":
			// Responses web search is server-executed. Chat-only providers
			// cannot safely expose it as a client function, so provider-specific
			// adapters may instead enable their native search facility.
			registry.webSearch = true
		}
	}
	return out, registry, nil
}

func chatFunctionTool(name, description string, parameters json.RawMessage, scrubSchema bool) map[string]any {
	if len(parameters) == 0 || string(parameters) == "null" {
		parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if scrubSchema {
		if scrubbed := scrubJSONSchema(parameters); len(scrubbed) > 0 {
			parameters = scrubbed
		}
	}
	var schema any
	if err := json.Unmarshal(parameters, &schema); err != nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	function := map[string]any{
		"name":       name,
		"parameters": schema,
	}
	if description != "" {
		function["description"] = description
	}
	return map[string]any{"type": "function", "function": function}
}

type chatReplayState struct {
	Version       int                                   `json:"version"`
	MessageFields map[string]json.RawMessage            `json:"message_fields,omitempty"`
	ToolFields    map[string]map[string]json.RawMessage `json:"tool_fields,omitempty"`
}

func (s *chatReplayState) merge(other chatReplayState) {
	if other.Version > s.Version {
		s.Version = other.Version
	}
	if len(other.MessageFields) > 0 {
		if s.MessageFields == nil {
			s.MessageFields = map[string]json.RawMessage{}
		}
		for key, value := range other.MessageFields {
			s.MessageFields[key] = append(json.RawMessage(nil), value...)
		}
	}
	if len(other.ToolFields) > 0 {
		if s.ToolFields == nil {
			s.ToolFields = map[string]map[string]json.RawMessage{}
		}
		for callID, fields := range other.ToolFields {
			if s.ToolFields[callID] == nil {
				s.ToolFields[callID] = map[string]json.RawMessage{}
			}
			for key, value := range fields {
				s.ToolFields[callID][key] = append(json.RawMessage(nil), value...)
			}
		}
	}
}

func (s chatReplayState) empty() bool {
	return len(s.MessageFields) == 0 && len(s.ToolFields) == 0
}

func encodeChatReplayState(state chatReplayState) string {
	if state.empty() {
		return ""
	}
	state.Version = 1
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return chatReplayStatePrefix + base64.RawStdEncoding.EncodeToString(raw)
}

func decodeChatReplayState(encoded string) (chatReplayState, bool) {
	if !strings.HasPrefix(encoded, chatReplayStatePrefix) {
		return chatReplayState{}, false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encoded, chatReplayStatePrefix))
	if err != nil {
		return chatReplayState{}, false
	}
	var state chatReplayState
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 {
		return chatReplayState{}, false
	}
	return state, true
}

type assistantChatAccumulator struct {
	content       []any
	reasoningText strings.Builder
	state         chatReplayState
	toolCalls     []any
}

func (a *assistantChatAccumulator) empty() bool {
	return len(a.content) == 0 && a.reasoningText.Len() == 0 && a.state.empty() && len(a.toolCalls) == 0
}

func responsesInputToChat(req *responsesRequest, registry *responseToolRegistry, reasoningReplayField string) ([]any, error) {
	var messages []any
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	var pending assistantChatAccumulator
	flushAssistant := func() {
		if pending.empty() {
			return
		}
		message := map[string]any{"role": "assistant"}
		if len(pending.content) > 0 {
			message["content"] = compactChatContent(pending.content)
		} else {
			message["content"] = nil
		}
		if len(pending.toolCalls) > 0 {
			message["tool_calls"] = pending.toolCalls
		}
		for key, raw := range pending.state.MessageFields {
			if value, ok := rawJSONValue(raw); ok {
				message[key] = value
			}
		}
		if pending.reasoningText.Len() > 0 {
			if _, exists := message[reasoningReplayField]; !exists && reasoningReplayField != "" {
				message[reasoningReplayField] = pending.reasoningText.String()
			}
		}
		messages = append(messages, message)
		pending = assistantChatAccumulator{}
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
			content := responseContentToChat(header.Content)
			if header.Role == "assistant" {
				pending.content = append(pending.content, content...)
				continue
			}
			flushAssistant()
			role := header.Role
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			messages = append(messages, map[string]any{
				"role":    role,
				"content": compactChatContent(content),
			})
		case "agent_message":
			var content []struct {
				Type             string `json:"type"`
				Text             string `json:"text"`
				EncryptedContent string `json:"encrypted_content"`
			}
			_ = json.Unmarshal(header.Content, &content)
			for _, item := range content {
				if item.Text != "" {
					pending.content = append(pending.content, map[string]any{
						"type": "text",
						"text": fmt.Sprintf("[%s -> %s]\n%s", header.Author, header.Recipient, item.Text),
					})
				}
			}
		case "reasoning":
			var item struct {
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.Unmarshal(raw, &item)
			for _, part := range item.Content {
				pending.reasoningText.WriteString(part.Text)
			}
			if pending.reasoningText.Len() == 0 {
				for _, part := range item.Summary {
					pending.reasoningText.WriteString(part.Text)
				}
			}
			if state, ok := decodeChatReplayState(header.EncryptedContent); ok {
				pending.state.merge(state)
			}
		case "function_call", "custom_tool_call":
			identity := responseToolIdentity{
				Name:      header.Name,
				Namespace: header.Namespace,
				Custom:    header.Type == "custom_tool_call",
			}
			chatName := registry.chatName(identity.Namespace, identity.Name)
			arguments := header.Arguments
			if identity.Custom {
				encoded, _ := json.Marshal(map[string]string{"input": header.Input})
				arguments = string(encoded)
			}
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			call := map[string]any{
				"id":   header.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      chatName,
					"arguments": arguments,
				},
			}
			if fields := pending.state.ToolFields[header.CallID]; len(fields) > 0 {
				for key, value := range fields {
					if decoded, ok := rawJSONValue(value); ok {
						call[key] = decoded
					}
				}
			}
			pending.toolCalls = append(pending.toolCalls, call)
		case "function_call_output", "custom_tool_call_output":
			flushAssistant()
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": header.CallID,
				"content":      responseToolOutputText(header.Output),
			})
		case "local_shell_call", "web_search_call", "image_generation_call":
			// These are server-side Responses items. Preserve a compact marker
			// in history rather than inventing a client tool execution.
			pending.content = append(pending.content, map[string]any{
				"type": "text",
				"text": "[" + header.Type + " completed]",
			})
		}
	}
	flushAssistant()
	return messages, nil
}

func responseContentToChat(raw json.RawMessage) []any {
	var content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		AudioURL string `json:"audio_url"`
		Detail   string `json:"detail"`
	}
	if json.Unmarshal(raw, &content) != nil {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return nil
	}
	out := make([]any, 0, len(content))
	for _, item := range content {
		switch item.Type {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": item.Text})
		case "input_image":
			image := map[string]any{"url": item.ImageURL}
			if item.Detail != "" {
				image["detail"] = item.Detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
		case "input_audio":
			out = append(out, map[string]any{"type": "text", "text": "[audio input: " + item.AudioURL + "]"})
		}
	}
	return out
}

func compactChatContent(content []any) any {
	if len(content) == 0 {
		return ""
	}
	if len(content) == 1 {
		if item, ok := content[0].(map[string]any); ok && item["type"] == "text" {
			if text, ok := item["text"].(string); ok {
				return text
			}
		}
	}
	return content
}

func responseToolOutputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		AudioURL string `json:"audio_url"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, part := range parts {
			switch part.Type {
			case "input_text", "output_text", "text":
				out = append(out, part.Text)
			case "input_image":
				out = append(out, "[image: "+part.ImageURL+"]")
			case "input_audio":
				out = append(out, "[audio: "+part.AudioURL+"]")
			}
		}
		return strings.Join(out, "\n")
	}
	return strings.TrimSpace(string(raw))
}

func rawJSONValue(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	return value, true
}

type chatCompletionChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []chatStreamChoice   `json:"choices"`
	Usage   chatCompletionUsage  `json:"usage"`
	Error   *chatCompletionError `json:"error"`
}

type chatCompletionError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type chatStreamChoice struct {
	Index        int             `json:"index"`
	Delta        chatStreamDelta `json:"delta"`
	FinishReason string          `json:"finish_reason"`
}

type chatStreamDelta struct {
	Role             string            `json:"role"`
	Content          json.RawMessage   `json:"content"`
	ReasoningContent string            `json:"reasoning_content"`
	Reasoning        string            `json:"reasoning"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details"`
	ExtraContent     json.RawMessage   `json:"extra_content"`
	ToolCalls        []chatStreamCall  `json:"tool_calls"`
}

type chatStreamCall struct {
	Index        int                `json:"index"`
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Function     chatStreamFunction `json:"function"`
	ExtraContent json.RawMessage    `json:"extra_content"`
}

type chatStreamFunction struct {
	Name      string            `json:"name"`
	Arguments chatToolArguments `json:"arguments"`
}

type chatCompletionUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chatResponsesToolState struct {
	index        int
	id           string
	itemID       string
	chatName     string
	arguments    strings.Builder
	extraContent json.RawMessage
}

type chatResponsesStreamTranslator struct {
	out        *sseWriter
	model      string
	registry   *responseToolRegistry
	responseID string

	started           bool
	errored           bool
	reasoningStarted  bool
	reasoningDone     bool
	reasoningID       string
	reasoning         strings.Builder
	reasoningSnapshot string
	messageStarted    bool
	messageDone       bool
	messageID         string
	text              strings.Builder

	messageFieldText map[string]*strings.Builder
	messageFieldJSON map[string]json.RawMessage
	tools            map[int]*chatResponsesToolState
	usage            chatCompletionUsage
	finishReason     string
}

func newChatResponsesStreamTranslator(w io.Writer, flush http.Flusher, model string, registry *responseToolRegistry) *chatResponsesStreamTranslator {
	return &chatResponsesStreamTranslator{
		out:              &sseWriter{w: w, flush: flush},
		model:            model,
		registry:         registry,
		responseID:       "resp_" + newMessageID(),
		reasoningID:      "rs_" + newMessageID(),
		messageID:        "msg_" + newMessageID(),
		messageFieldText: map[string]*strings.Builder{},
		messageFieldJSON: map[string]json.RawMessage{},
		tools:            map[int]*chatResponsesToolState{},
	}
}

func (t *chatResponsesStreamTranslator) onEvent(raw json.RawMessage) error {
	var chunk chatCompletionChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return err
	}
	if chunk.Error != nil {
		t.fail(firstNonEmpty(chunk.Error.Code, chunk.Error.Type, "upstream_error"), chunk.Error.Message)
		return errStreamAborted
	}
	if !t.started && chunk.ID != "" && strings.HasPrefix(chunk.ID, "resp_") {
		t.responseID = chunk.ID
	}
	t.start()
	if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 || chunk.Usage.TotalTokens != 0 {
		t.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			t.finishReason = choice.FinishReason
			if code, message := chatFinishFailure(choice.FinishReason); code != "" {
				t.fail(code, message)
				return errStreamAborted
			}
		}
		delta := choice.Delta
		visibleReasoning := ""
		if delta.ReasoningContent != "" {
			visibleReasoning = delta.ReasoningContent
			t.appendMessageTextField("reasoning_content", delta.ReasoningContent)
		}
		if delta.Reasoning != "" {
			if visibleReasoning == "" {
				visibleReasoning = delta.Reasoning
			}
			t.appendMessageTextField("reasoning", delta.Reasoning)
		}
		if len(delta.ReasoningDetails) > 0 {
			if visibleReasoning == "" {
				visibleReasoning = textFromReasoningDetailsRaw(delta.ReasoningDetails)
			}
			t.mergeMessageJSONArrayField("reasoning_details", delta.ReasoningDetails)
		}
		if len(delta.ExtraContent) > 0 && string(delta.ExtraContent) != "null" {
			t.messageFieldJSON["extra_content"] = mergeRawJSON(t.messageFieldJSON["extra_content"], delta.ExtraContent)
		}
		if visibleReasoning != "" {
			t.onReasoning(visibleReasoning)
		}
		if content := chatDeltaText(delta.Content); content != "" {
			t.onText(content)
		}
		for _, call := range delta.ToolCalls {
			t.onToolCall(call)
		}
	}
	return nil
}

func chatFinishFailure(reason string) (code, message string) {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "context_length_exceeded", "model_context_window_exceeded":
		return "context_length_exceeded", "upstream model context window exceeded"
	case "network_error", "insufficient_system_resource", "server_error", "error":
		return "upstream_error", "upstream response finished with " + reason
	default:
		return "", ""
	}
}

func chatIncompleteReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens":
		return "max_output_tokens"
	case "content_filter", "sensitive", "safety":
		return "content_filter"
	default:
		return ""
	}
}

func (t *chatResponsesStreamTranslator) start() {
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

func (t *chatResponsesStreamTranslator) onReasoning(delta string) {
	t.reasoning.WriteString(delta)
	if t.messageStarted || t.reasoningDone {
		return
	}
	if !t.reasoningStarted {
		t.reasoningStarted = true
		t.out.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]any{
				"type":              "reasoning",
				"id":                t.reasoningID,
				"summary":           []any{},
				"encrypted_content": nil,
			},
		})
		t.out.event("response.reasoning_summary_part.added", map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       t.reasoningID,
			"summary_index": 0,
		})
	}
	t.out.event("response.reasoning_summary_text.delta", map[string]any{
		"type":          "response.reasoning_summary_text.delta",
		"item_id":       t.reasoningID,
		"summary_index": 0,
		"delta":         delta,
	})
	// Current Codex requests sequential-cutoff reasoning summaries. It ignores
	// summary deltas in that mode and consumes "done" text as the display
	// delta, while older/non-cutoff clients do the inverse. Emitting both makes
	// reasoning progressive in either mode.
	t.out.event("response.reasoning_summary_text.done", map[string]any{
		"type":          "response.reasoning_summary_text.done",
		"item_id":       t.reasoningID,
		"summary_index": 0,
		"text":          delta,
	})
}

func (t *chatResponsesStreamTranslator) onText(delta string) {
	if !t.messageStarted {
		t.finishReasoning()
		t.messageStarted = true
		t.out.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 1,
			"item": map[string]any{
				"type":    "message",
				"id":      t.messageID,
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": ""}},
			},
		})
	}
	t.text.WriteString(delta)
	t.out.event("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       t.messageID,
		"output_index":  1,
		"content_index": 0,
		"delta":         delta,
	})
}

func (t *chatResponsesStreamTranslator) onToolCall(call chatStreamCall) {
	state := t.tools[call.Index]
	if state == nil {
		state = &chatResponsesToolState{index: call.Index}
		t.tools[call.Index] = state
	}
	if state.itemID == "" {
		state.itemID = newToolUseID()
	}
	if call.ID != "" {
		state.id = call.ID
	}
	if call.Function.Name != "" {
		state.chatName = call.Function.Name
	}
	if call.Function.Arguments != "" {
		state.arguments.WriteString(string(call.Function.Arguments))
		t.out.event("response.custom_tool_call_input.delta", map[string]any{
			"type":    "response.custom_tool_call_input.delta",
			"item_id": state.itemID,
			"call_id": state.itemID,
			"delta":   call.Function.Arguments,
		})
	}
	if len(call.ExtraContent) > 0 && string(call.ExtraContent) != "null" {
		state.extraContent = mergeRawJSON(state.extraContent, call.ExtraContent)
	}
}

func (t *chatResponsesStreamTranslator) appendMessageTextField(name, delta string) {
	builder := t.messageFieldText[name]
	if builder == nil {
		builder = &strings.Builder{}
		t.messageFieldText[name] = builder
	}
	builder.WriteString(delta)
}

func (t *chatResponsesStreamTranslator) mergeMessageJSONArrayField(name string, values []json.RawMessage) {
	var current []json.RawMessage
	_ = json.Unmarshal(t.messageFieldJSON[name], &current)
	current = append(current, values...)
	encoded, _ := json.Marshal(current)
	t.messageFieldJSON[name] = encoded
}

func (t *chatResponsesStreamTranslator) replayState() chatReplayState {
	state := chatReplayState{
		Version:       1,
		MessageFields: map[string]json.RawMessage{},
		ToolFields:    map[string]map[string]json.RawMessage{},
	}
	for key, builder := range t.messageFieldText {
		encoded, _ := json.Marshal(builder.String())
		state.MessageFields[key] = encoded
	}
	for key, raw := range t.messageFieldJSON {
		state.MessageFields[key] = append(json.RawMessage(nil), raw...)
	}
	for _, tool := range t.tools {
		if len(tool.extraContent) == 0 {
			continue
		}
		callID := tool.id
		if callID == "" {
			callID = fmt.Sprintf("call_%d", tool.index)
		}
		state.ToolFields[callID] = map[string]json.RawMessage{
			"extra_content": append(json.RawMessage(nil), tool.extraContent...),
		}
	}
	if len(state.MessageFields) == 0 {
		state.MessageFields = nil
	}
	if len(state.ToolFields) == 0 {
		state.ToolFields = nil
	}
	return state
}

func (t *chatResponsesStreamTranslator) finishReasoning() {
	if t.reasoningDone || !t.reasoningStarted {
		return
	}
	t.reasoningDone = true
	text := t.reasoning.String()
	encodedState := encodeChatReplayState(t.replayState())
	t.reasoningSnapshot = encodedState
	t.out.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item":         reasoningResponseItem(t.reasoningID, text, encodedState),
	})
}

func reasoningResponseItem(id, text, encryptedContent string) map[string]any {
	item := map[string]any{
		"type":    "reasoning",
		"id":      id,
		"summary": []any{},
	}
	if text != "" {
		item["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
		item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
	}
	if encryptedContent != "" {
		item["encrypted_content"] = encryptedContent
	} else {
		item["encrypted_content"] = nil
	}
	return item
}

func (t *chatResponsesStreamTranslator) finishMessage(hasTools bool) {
	if t.messageDone || !t.messageStarted {
		return
	}
	t.messageDone = true
	phase := "final_answer"
	if hasTools {
		phase = "commentary"
	}
	t.out.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": 1,
		"item": map[string]any{
			"type":  "message",
			"id":    t.messageID,
			"role":  "assistant",
			"phase": phase,
			"content": []any{map[string]any{
				"type": "output_text",
				"text": t.text.String(),
			}},
		},
	})
}

func (t *chatResponsesStreamTranslator) emitTools(firstOutputIndex int) int {
	indexes := make([]int, 0, len(t.tools))
	for index := range t.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	outputIndex := firstOutputIndex
	emitted := 0
	for _, index := range indexes {
		tool := t.tools[index]
		if tool.id == "" {
			tool.id = newToolUseID()
		}
		if tool.itemID == "" {
			tool.itemID = newToolUseID()
		}
		identity, ok := t.registry.byChatName[tool.chatName]
		if !ok {
			identity = responseToolIdentity{Name: tool.chatName}
		}
		arguments := tool.arguments.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		item := map[string]any{
			"type":      "function_call",
			"id":        tool.itemID,
			"call_id":   tool.id,
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
			"output_index": outputIndex,
			"item":         item,
		})
		outputIndex++
		emitted++
	}
	return emitted
}

func customToolInput(arguments string) string {
	var wrapped struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(arguments), &wrapped) == nil && wrapped.Input != "" {
		return wrapped.Input
	}
	var plain string
	if json.Unmarshal([]byte(arguments), &plain) == nil {
		return plain
	}
	return arguments
}

func (t *chatResponsesStreamTranslator) fail(code, message string) {
	if t.errored {
		return
	}
	t.errored = true
	t.start()
	if message == "" {
		message = "upstream stream failed"
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

func (t *chatResponsesStreamTranslator) finish() {
	if t.errored {
		return
	}
	t.start()
	t.finishReasoning()
	hasTools := len(t.tools) > 0
	if !t.messageStarted && !hasTools && !t.reasoningStarted {
		t.onText("")
	}
	t.finishMessage(hasTools)

	finalState := encodeChatReplayState(t.replayState())
	nextOutputIndex := 2
	if finalState != "" && finalState != t.reasoningSnapshot {
		t.out.event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": nextOutputIndex,
			"item":         reasoningResponseItem("rs_state_"+newMessageID(), "", finalState),
		})
		nextOutputIndex++
	}
	toolCount := t.emitTools(nextOutputIndex)
	inputDetails := map[string]any{"cached_tokens": 0}
	if t.usage.PromptTokensDetails != nil {
		inputDetails["cached_tokens"] = t.usage.PromptTokensDetails.CachedTokens
	}
	reasoningTokens := 0
	if t.usage.CompletionTokensDetails != nil {
		reasoningTokens = t.usage.CompletionTokensDetails.ReasoningTokens
	}
	total := t.usage.TotalTokens
	if total == 0 {
		total = t.usage.PromptTokens + t.usage.CompletionTokens
	}
	response := map[string]any{
		"id":       t.responseID,
		"object":   "response",
		"status":   "completed",
		"model":    t.model,
		"end_turn": toolCount == 0,
		"usage": map[string]any{
			"input_tokens":          t.usage.PromptTokens,
			"input_tokens_details":  inputDetails,
			"output_tokens":         t.usage.CompletionTokens,
			"output_tokens_details": map[string]any{"reasoning_tokens": reasoningTokens},
			"total_tokens":          total,
		},
	}
	if reason := chatIncompleteReason(t.finishReason); reason != "" {
		response["status"] = "incomplete"
		response["error"] = nil
		response["incomplete_details"] = map[string]any{"reason": reason}
		t.out.event("response.incomplete", map[string]any{
			"type":     "response.incomplete",
			"response": response,
		})
		return
	}
	t.out.event("response.completed", map[string]any{
		"type":     "response.completed",
		"response": response,
	})
}

func chatDeltaText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out strings.Builder
		for _, part := range parts {
			out.WriteString(part.Text)
		}
		return out.String()
	}
	return ""
}

func textFromReasoningDetailsRaw(details []json.RawMessage) string {
	var out strings.Builder
	for _, raw := range details {
		var detail struct {
			Text    string `json:"text"`
			Summary string `json:"summary"`
			Delta   string `json:"delta"`
		}
		if json.Unmarshal(raw, &detail) == nil {
			out.WriteString(firstNonEmpty(detail.Text, detail.Summary, detail.Delta))
		}
	}
	return out.String()
}

func mergeRawJSON(current, incoming json.RawMessage) json.RawMessage {
	if len(current) == 0 || string(current) == "null" {
		return append(json.RawMessage(nil), incoming...)
	}
	var left, right map[string]any
	if json.Unmarshal(current, &left) == nil && json.Unmarshal(incoming, &right) == nil {
		mergeJSONObjects(left, right)
		encoded, _ := json.Marshal(left)
		return encoded
	}
	return append(json.RawMessage(nil), incoming...)
}

func mergeJSONObjects(target, incoming map[string]any) {
	for key, value := range incoming {
		if right, ok := value.(map[string]any); ok {
			if left, ok := target[key].(map[string]any); ok {
				mergeJSONObjects(left, right)
				continue
			}
		}
		target[key] = value
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
