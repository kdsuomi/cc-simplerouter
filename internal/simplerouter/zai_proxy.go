package simplerouter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const zaiThinkingSignature = "zai#preserved"

func startZAIProxy(upstreamBase, model string, httpClient *http.Client, disableThinking bool) (baseURL string, stop func(), err error) {
	p := newZAIProxy(upstreamBase, model, httpClient, disableThinking)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: p}
	go server.Serve(listener)
	return fmt.Sprintf("http://%s", listener.Addr().String()), func() { _ = server.Close() }, nil
}

type zaiProxy struct {
	upstreamBase    string
	model           string
	httpClient      *http.Client
	disableThinking bool
}

func newZAIProxy(upstreamBase, model string, httpClient *http.Client, disableThinking bool) *zaiProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &zaiProxy{
		upstreamBase:    strings.TrimRight(upstreamBase, "/"),
		model:           model,
		httpClient:      httpClient,
		disableThinking: disableThinking,
	}
}

func (p *zaiProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		p.handleMessages(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		handleEstimatedCountTokens(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": p.model, "type": "model"}}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		writeJSON(w, http.StatusOK, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1/models/"), "type": "model"})
	default:
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown route "+r.URL.Path)
	}
}

func (p *zaiProxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "parse body: "+err.Error())
		return
	}
	payload, err := anthropicToZAIChat(&req, p.disableThinking)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstreamBase+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+apiKeyFromRequest(r))

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "Z.AI request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayCompatUpstreamError(w, resp, "Z.AI")
		return
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		tr := newZAIStreamTranslator(w, flusher, req.Model)
		if err := readCompatSSE(resp.Body, tr.onEvent); err != nil && !errors.Is(err, errStreamAborted) {
			tr.emitError("api_error", err.Error())
		}
		tr.finish()
		return
	}
	var out zaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode Z.AI response: "+err.Error())
		return
	}
	if zaiIsNetworkError(&out) {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", zaiNetworkErrorMessage)
		return
	}
	writeJSON(w, http.StatusOK, zaiToAnthropic(&out, req.Model))
}

type zaiChatResponse struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []zaiChoice `json:"choices"`
	Usage   zaiUsage    `json:"usage"`
}

type zaiChoice struct {
	Index        int        `json:"index"`
	Message      zaiMessage `json:"message"`
	Delta        zaiMessage `json:"delta"`
	FinishReason string     `json:"finish_reason"`
}

type zaiMessage struct {
	Role             string        `json:"role,omitempty"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []zaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

type zaiToolCall struct {
	Index    int             `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function zaiToolFunction `json:"function"`
}

type zaiToolFunction struct {
	Name      string           `json:"name,omitempty"`
	Arguments zaiToolArguments `json:"arguments,omitempty"`
}

type zaiToolArguments string

func (a *zaiToolArguments) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*a = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		*a = zaiToolArguments(s)
		return nil
	}
	if !json.Valid(trimmed) {
		return fmt.Errorf("invalid tool arguments JSON")
	}
	*a = zaiToolArguments(string(trimmed))
	return nil
}

type zaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func anthropicToZAIChat(req *anthropicRequest, disableThinking bool) ([]byte, error) {
	messages, err := zaiMessagesFromAnthropic(req)
	if err != nil {
		return nil, err
	}
	model := zaiModelID(req.Model)
	out := map[string]any{
		"model":      model,
		"messages":   messages,
		"stream":     req.Stream,
		"max_tokens": req.MaxTokens,
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences[:1]
	}
	tools, err := zaiToolsForChoice(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		out["tools"] = tools
		out["tool_stream"] = req.Stream
		if tc := zaiToolChoice(req.ToolChoice); tc != "" {
			out["tool_choice"] = tc
		}
	}
	thinkingDisabled := disableThinking || (req.Thinking != nil && req.Thinking.Type == "disabled")
	if zaiSupportsThinking(model) {
		if thinkingDisabled {
			out["thinking"] = map[string]any{"type": "disabled"}
		} else {
			out["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
			if req.Thinking != nil && req.Thinking.Type == "enabled" && zaiSupportsReasoningEffort(model) {
				out["reasoning_effort"] = zaiReasoningEffort(req.Thinking.BudgetTokens)
			}
		}
	}
	return json.Marshal(out)
}

func zaiMessagesFromAnthropic(req *anthropicRequest) ([]map[string]any, error) {
	var out []map[string]any
	if sys := systemTextFromRaw(req.System); sys != "" {
		out = append(out, map[string]any{"role": "system", "content": sys})
	}
	for i, msg := range req.Messages {
		blocks, err := blocksFromContent(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: unparseable content: %w", i, err)
		}
		switch msg.Role {
		case "assistant":
			assistant := map[string]any{"role": "assistant"}
			var texts, thoughts []string
			var calls []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case "thinking":
					if b.Thinking != "" {
						thoughts = append(thoughts, b.Thinking)
					}
				case "tool_use":
					id := b.ID
					if id == "" {
						id = newToolUseID()
					}
					calls = append(calls, map[string]any{
						"id":   id,
						"type": "function",
						"function": map[string]any{
							"name":      b.Name,
							"arguments": rawJSONAsString(b.Input),
						},
					})
				}
			}
			assistant["content"] = strings.Join(texts, "\n\n")
			if len(thoughts) > 0 {
				assistant["reasoning_content"] = strings.Join(thoughts, "\n\n")
			}
			if len(calls) > 0 {
				assistant["tool_calls"] = calls
			}
			out = append(out, assistant)
		default:
			var texts []string
			var parts []map[string]any
			hasMultimodal := false
			flushTextPart := func() {
				if len(texts) == 0 {
					return
				}
				parts = append(parts, map[string]any{"type": "text", "text": strings.Join(texts, "\n\n")})
				texts = nil
			}
			flushUser := func() {
				if len(texts) == 0 && len(parts) == 0 {
					return
				}
				if hasMultimodal {
					flushTextPart()
					out = append(out, map[string]any{"role": "user", "content": parts})
				} else {
					out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n\n")})
				}
				texts = nil
				parts = nil
				hasMultimodal = false
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case "image":
					if imageURL := zaiImageURL(b); imageURL != "" {
						hasMultimodal = true
						flushTextPart()
						parts = append(parts, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": imageURL},
						})
					}
				case "tool_result":
					flushUser()
					out = append(out, map[string]any{
						"role":         "tool",
						"tool_call_id": b.ToolUseID,
						"content":      toolResultText(b),
					})
				}
			}
			flushUser()
		}
	}
	return out, nil
}

func zaiImageURL(b anthropicBlock) string {
	if b.Source == nil || b.Source.Type != "base64" || b.Source.Data == "" {
		return ""
	}
	mediaType := b.Source.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + b.Source.Data
}

func zaiToolsForChoice(tools []anthropicTool, tc *anthropicToolChoice) ([]map[string]any, error) {
	if tc == nil || tc.Type == "auto" {
		return zaiTools(tools), nil
	}
	switch tc.Type {
	case "none":
		return nil, nil
	case "any":
		return zaiTools(tools), nil
	case "tool":
		return zaiToolsNamed(tools, tc.Name), nil
	default:
		return nil, fmt.Errorf("unsupported tool_choice %q", tc.Type)
	}
}

func zaiTools(tools []anthropicTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  jsonRawOrObject(scrubJSONSchema(t.InputSchema)),
			},
		})
	}
	return out
}

func zaiToolsNamed(tools []anthropicTool, name string) []map[string]any {
	var filtered []anthropicTool
	for _, t := range tools {
		if t.Name == name {
			filtered = append(filtered, t)
		}
	}
	return zaiTools(filtered)
}

func zaiToolChoice(tc *anthropicToolChoice) string {
	if tc == nil {
		return ""
	}
	switch tc.Type {
	case "auto", "any", "tool":
		return "auto"
	default:
		return ""
	}
}

func zaiModelID(model string) string {
	model = strings.TrimSuffix(strings.TrimSpace(model), "[1m]")
	if strings.HasPrefix(strings.ToLower(model), "z-ai/") {
		return model[len("z-ai/"):]
	}
	return model
}

func zaiSupportsThinking(model string) bool {
	major, minor, ok := zaiModelVersion(model)
	if !ok {
		return false
	}
	return major > 4 || (major == 4 && minor >= 5)
}

func zaiSupportsReasoningEffort(model string) bool {
	return normalizeModelText(zaiModelID(model)) == "glm-5.2"
}

func zaiModelVersion(model string) (major, minor int, ok bool) {
	m := normalizeModelText(zaiModelID(model))
	if !strings.HasPrefix(m, "glm-") {
		return 0, 0, false
	}
	version := strings.TrimPrefix(m, "glm-")
	i := 0
	for i < len(version) && version[i] >= '0' && version[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, 0, false
	}
	parsedMajor, err := strconv.Atoi(version[:i])
	if err != nil {
		return 0, 0, false
	}
	parsedMinor := 0
	if i < len(version) && version[i] == '.' {
		j := i + 1
		for j < len(version) && version[j] >= '0' && version[j] <= '9' {
			j++
		}
		if j > i+1 {
			parsedMinor, err = strconv.Atoi(version[i+1 : j])
			if err != nil {
				return 0, 0, false
			}
		}
	}
	return parsedMajor, parsedMinor, true
}

func zaiToAnthropic(resp *zaiChatResponse, fallbackModel string) *anthropicMessageResponse {
	model := resp.Model
	if model == "" {
		model = fallbackModel
	}
	out := &anthropicMessageResponse{
		ID:      newMessageID(),
		Type:    "message",
		Role:    "assistant",
		Model:   model,
		Content: []map[string]any{},
		Usage:   anthropicUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens},
	}
	if len(resp.Choices) == 0 {
		out.StopReason = "end_turn"
		return out
	}
	ch := resp.Choices[0]
	msg := ch.Message
	if msg.ReasoningContent != "" {
		out.Content = append(out.Content, map[string]any{"type": "thinking", "thinking": msg.ReasoningContent, "signature": zaiThinkingSignature})
	}
	if msg.Content != "" {
		out.Content = append(out.Content, map[string]any{"type": "text", "text": msg.Content})
	}
	for _, call := range msg.ToolCalls {
		id := call.ID
		if id == "" {
			id = newToolUseID()
		}
		out.Content = append(out.Content, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  call.Function.Name,
			"input": parseToolArguments(string(call.Function.Arguments)),
		})
	}
	out.StopReason = zaiStopReason(ch.FinishReason, len(msg.ToolCalls) > 0)
	return out
}

func zaiStopReason(finish string, sawTool bool) string {
	if sawTool || finish == "tool_calls" {
		return "tool_use"
	}
	if finish == "network_error" {
		return ""
	}
	if finish == "length" || finish == "model_context_window_exceeded" {
		return "max_tokens"
	}
	if finish == "sensitive" {
		return "refusal"
	}
	return "end_turn"
}

const zaiNetworkErrorMessage = "Z.AI response finished with network_error"

func zaiIsNetworkError(resp *zaiChatResponse) bool {
	if resp == nil || len(resp.Choices) == 0 {
		return false
	}
	return resp.Choices[0].FinishReason == "network_error"
}

type zaiToolStreamState struct {
	id         string
	name       string
	blockIndex int
	open       bool
	pending    []string
}

type zaiStreamTranslator struct {
	out        *sseWriter
	model      string
	msgID      string
	started    bool
	blockIndex int
	blockOpen  bool
	blockType  string
	tools      map[int]*zaiToolStreamState
	stopReason string
	usage      anthropicUsage
	errored    bool
}

func newZAIStreamTranslator(w io.Writer, flush http.Flusher, model string) *zaiStreamTranslator {
	return &zaiStreamTranslator{
		out:        &sseWriter{w: w, flush: flush},
		model:      model,
		msgID:      newMessageID(),
		tools:      map[int]*zaiToolStreamState{},
		stopReason: "end_turn",
	}
}

func (t *zaiStreamTranslator) onEvent(raw json.RawMessage) error {
	var chunk zaiChatResponse
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return err
	}
	t.start()
	if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
		t.usage = anthropicUsage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != "" {
			if ch.FinishReason == "network_error" {
				t.emitError("api_error", zaiNetworkErrorMessage)
				return errStreamAborted
			}
			t.stopReason = zaiStopReason(ch.FinishReason, t.stopReason == "tool_use")
		}
		delta := ch.Delta
		if delta.ReasoningContent != "" {
			t.openBlock("thinking")
			t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": delta.ReasoningContent}})
		}
		if delta.Content != "" {
			t.openBlock("text")
			t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.blockIndex, "delta": map[string]any{"type": "text_delta", "text": delta.Content}})
		}
		for _, call := range delta.ToolCalls {
			t.onToolDelta(call)
		}
	}
	return nil
}

func (t *zaiStreamTranslator) start() {
	if t.started {
		return
	}
	t.started = true
	t.out.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            t.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (t *zaiStreamTranslator) openBlock(blockType string) {
	if t.blockOpen && t.blockType == blockType {
		return
	}
	t.closeBlock()
	t.blockType = blockType
	t.blockOpen = true
	block := map[string]any{"type": blockType}
	if blockType == "thinking" {
		block["thinking"] = ""
		block["signature"] = ""
	} else {
		block["text"] = ""
	}
	t.out.event("content_block_start", map[string]any{"type": "content_block_start", "index": t.blockIndex, "content_block": block})
}

func (t *zaiStreamTranslator) closeBlock() {
	if !t.blockOpen {
		return
	}
	if t.blockType == "thinking" {
		t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.blockIndex, "delta": map[string]any{"type": "signature_delta", "signature": zaiThinkingSignature}})
	}
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blockIndex})
	t.blockOpen = false
	t.blockIndex++
}

func (t *zaiStreamTranslator) onToolDelta(call zaiToolCall) {
	st := t.tools[call.Index]
	if st == nil {
		st = &zaiToolStreamState{}
		t.tools[call.Index] = st
	}
	if call.ID != "" {
		st.id = call.ID
	}
	if call.Function.Name != "" {
		st.name = call.Function.Name
	}
	arguments := string(call.Function.Arguments)
	if !st.open && st.name == "" {
		if arguments != "" {
			st.pending = append(st.pending, arguments)
		}
		return
	}
	if !st.open {
		t.closeBlock()
		st.open = true
		st.blockIndex = t.blockIndex
		t.blockIndex++
		if st.id == "" {
			st.id = newToolUseID()
		}
		t.stopReason = "tool_use"
		t.out.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    st.id,
				"name":  st.name,
				"input": map[string]any{},
			},
		})
		for _, pending := range st.pending {
			t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.blockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": pending}})
		}
		st.pending = nil
	}
	if arguments != "" {
		t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.blockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}})
	}
}

func (t *zaiStreamTranslator) emitError(errType, message string) {
	if message == "" {
		message = "Z.AI stream failed"
	}
	t.errored = true
	t.out.event("error", map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
}

func (t *zaiStreamTranslator) finish() {
	if t.errored {
		return
	}
	t.start()
	t.closeBlock()
	for _, st := range t.tools {
		if st.open {
			t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.blockIndex})
		}
	}
	t.out.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": t.stopReason, "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": t.usage.InputTokens, "output_tokens": t.usage.OutputTokens},
	})
	t.out.event("message_stop", map[string]any{"type": "message_stop"})
}
