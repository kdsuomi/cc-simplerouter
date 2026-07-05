package simplerouter

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

func startOpenAIProxy(upstreamBase, model string, httpClient *http.Client) (baseURL string, stop func(), err error) {
	p := newOpenAIProxy(upstreamBase, model, httpClient)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: p}
	go server.Serve(listener)
	return fmt.Sprintf("http://%s", listener.Addr().String()), func() { _ = server.Close() }, nil
}

type openAIProxy struct {
	upstreamBase string
	model        string
	httpClient   *http.Client
}

func newOpenAIProxy(upstreamBase, model string, httpClient *http.Client) *openAIProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &openAIProxy{
		upstreamBase: strings.TrimRight(upstreamBase, "/"),
		model:        model,
		httpClient:   httpClient,
	}
}

func (p *openAIProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (p *openAIProxy) handleMessages(w http.ResponseWriter, r *http.Request) {
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
	payload, err := anthropicToOpenAIResponses(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstreamBase+"/responses", bytes.NewReader(payload))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+apiKeyFromRequest(r))

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "OpenAI request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayCompatUpstreamError(w, resp, "OpenAI")
		return
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		tr := newOpenAIStreamTranslator(w, flusher, req.Model)
		if err := readCompatSSE(resp.Body, tr.onEvent); err != nil && !errors.Is(err, errStreamAborted) {
			tr.emitError("api_error", err.Error())
		}
		tr.finish()
		return
	}
	var out openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode OpenAI response: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, openAIToAnthropic(&out, req.Model))
}

type openAIResponse struct {
	ID                string              `json:"id"`
	Model             string              `json:"model"`
	Status            string              `json:"status"`
	Output            []json.RawMessage   `json:"output"`
	Usage             openAIUsage         `json:"usage"`
	IncompleteDetails *openAIIncomplete   `json:"incomplete_details,omitempty"`
	Error             *openAIErrorPayload `json:"error,omitempty"`
}

type openAIIncomplete struct {
	Reason string `json:"reason"`
}

type openAIUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type openAIErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type openAIOutputItem struct {
	Type             string            `json:"type"`
	ID               string            `json:"id,omitempty"`
	Status           string            `json:"status,omitempty"`
	CallID           string            `json:"call_id,omitempty"`
	Name             string            `json:"name,omitempty"`
	Arguments        string            `json:"arguments,omitempty"`
	Phase            string            `json:"phase,omitempty"`
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	Content          []openAITextBlock `json:"content,omitempty"`
	Summary          []openAITextBlock `json:"summary,omitempty"`
}

type openAITextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func anthropicToOpenAIResponses(req *anthropicRequest) ([]byte, error) {
	input, err := openAIInputFromAnthropic(req.Messages)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":             strings.TrimSuffix(req.Model, "[1m]"),
		"input":             input,
		"stream":            req.Stream,
		"store":             false,
		"include":           []string{"reasoning.encrypted_content"},
		"max_output_tokens": req.MaxTokens,
	}
	if sys := systemTextFromRaw(req.System); sys != "" {
		out["instructions"] = sys
	}
	if len(req.Tools) > 0 {
		out["tools"] = openAITools(req.Tools)
	}
	if tc := openAIToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		out["reasoning"] = map[string]any{"effort": reasoningEffort(req.Thinking.BudgetTokens)}
	}
	return json.Marshal(out)
}

func openAIInputFromAnthropic(messages []anthropicMessage) ([]any, error) {
	var input []any
	for i, msg := range messages {
		blocks, err := blocksFromContent(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: unparseable content: %w", i, err)
		}
		hasToolUse := false
		for _, b := range blocks {
			if b.Type == "tool_use" {
				hasToolUse = true
				break
			}
		}
		var textParts []string
		flushMessage := func() {
			if len(textParts) == 0 {
				return
			}
			item := map[string]any{"role": msg.Role, "content": strings.Join(textParts, "\n\n")}
			if msg.Role == "assistant" {
				item["phase"] = openAIAssistantPhase(hasToolUse)
			}
			input = append(input, item)
			textParts = nil
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					textParts = append(textParts, b.Text)
				}
			case "image":
				flushMessage()
				if b.Source != nil && b.Source.Type == "base64" {
					input = append(input, map[string]any{
						"role": msg.Role,
						"content": []map[string]any{{
							"type":      "input_image",
							"image_url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
						}},
					})
				}
			case "redacted_thinking":
				flushMessage()
				if item := decodeOpenAIReasoningItem(b.Data); item != nil {
					input = append(input, item)
				}
			case "tool_use":
				flushMessage()
				state := decodeOpenAIToolState(b.ID)
				item := map[string]any{
					"type":      "function_call",
					"call_id":   state.CallID,
					"name":      b.Name,
					"arguments": rawJSONAsString(b.Input),
				}
				if state.ID != "" {
					item["id"] = state.ID
				}
				if state.Status != "" {
					item["status"] = state.Status
				}
				input = append(input, item)
			case "tool_result":
				flushMessage()
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": decodeOpenAIToolID(b.ToolUseID),
					"output":  toolResultText(b),
				})
			}
		}
		flushMessage()
	}
	return input, nil
}

func openAIAssistantPhase(hasToolUse bool) string {
	if hasToolUse {
		return "commentary"
	}
	return "final_answer"
}

func openAITools(tools []anthropicTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  jsonRawOrObject(scrubJSONSchema(t.InputSchema)),
			"strict":      false,
		})
	}
	return out
}

func openAIToolChoice(tc *anthropicToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "name": tc.Name}
	default:
		return nil
	}
}

func openAIToAnthropic(resp *openAIResponse, fallbackModel string) *anthropicMessageResponse {
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
		Usage:   anthropicUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
	}
	sawTool := false
	for _, raw := range resp.Output {
		var item openAIOutputItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		switch item.Type {
		case "reasoning":
			out.Content = append(out.Content, map[string]any{"type": "redacted_thinking", "data": encodeOpenAIReasoningItem(raw)})
		case "message":
			for _, c := range item.Content {
				if c.Text != "" {
					out.Content = append(out.Content, map[string]any{"type": "text", "text": c.Text})
				}
			}
		case "function_call":
			out.Content = append(out.Content, map[string]any{
				"type":  "tool_use",
				"id":    encodeOpenAIToolUseID(item),
				"name":  item.Name,
				"input": parseToolArguments(item.Arguments),
			})
			sawTool = true
		}
	}
	out.StopReason = "end_turn"
	if sawTool {
		out.StopReason = "tool_use"
	} else if resp.Status == "incomplete" && resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason == "max_output_tokens" {
		out.StopReason = "max_tokens"
	}
	return out
}

type openAIToolState struct {
	CallID string `json:"call_id"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
}

func encodeOpenAIToolUseID(item openAIOutputItem) string {
	if item.ID == "" && item.Status == "" {
		return encodeOpenAIToolID(item.CallID)
	}
	if strings.TrimSpace(item.CallID) == "" {
		return newToolUseID()
	}
	state := openAIToolState{CallID: item.CallID, ID: item.ID, Status: item.Status}
	data, err := json.Marshal(state)
	if err != nil {
		return encodeOpenAIToolID(item.CallID)
	}
	return "toolu_oai2_" + base64.RawURLEncoding.EncodeToString(data)
}

func encodeOpenAIToolID(callID string) string {
	if strings.TrimSpace(callID) == "" {
		return newToolUseID()
	}
	return "toolu_oai_" + base64.RawURLEncoding.EncodeToString([]byte(callID))
}

func decodeOpenAIToolID(id string) string {
	return decodeOpenAIToolState(id).CallID
}

func decodeOpenAIToolState(id string) openAIToolState {
	id = strings.TrimSpace(id)
	if enc, ok := strings.CutPrefix(id, "toolu_oai2_"); ok {
		if data, err := base64.RawURLEncoding.DecodeString(enc); err == nil && len(data) > 0 {
			var state openAIToolState
			if json.Unmarshal(data, &state) == nil && state.CallID != "" {
				return state
			}
		}
	}
	if enc, ok := strings.CutPrefix(id, "toolu_oai_"); ok {
		if data, err := base64.RawURLEncoding.DecodeString(enc); err == nil && len(data) > 0 {
			return openAIToolState{CallID: string(data)}
		}
	}
	return openAIToolState{CallID: id}
}

func encodeOpenAIReasoningItem(raw json.RawMessage) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeOpenAIReasoningItem(data string) any {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil || !json.Valid(raw) {
		return nil
	}
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil
	}
	if item["type"] != "reasoning" {
		return nil
	}
	if _, ok := item["summary"]; !ok {
		item["summary"] = []any{}
	}
	return item
}

type openAIStreamTranslator struct {
	out        *sseWriter
	model      string
	msgID      string
	started    bool
	blockIndex int
	textOpen   bool
	tools      map[int]int
	toolArgs   map[int]bool
	toolItems  map[int]openAIOutputItem
	pendingArg map[int][]string
	stopReason string
	usage      anthropicUsage
	errored    bool
}

func newOpenAIStreamTranslator(w io.Writer, flush http.Flusher, model string) *openAIStreamTranslator {
	return &openAIStreamTranslator{
		out:        &sseWriter{w: w, flush: flush},
		model:      model,
		msgID:      newMessageID(),
		tools:      map[int]int{},
		toolArgs:   map[int]bool{},
		toolItems:  map[int]openAIOutputItem{},
		pendingArg: map[int][]string{},
		stopReason: "end_turn",
	}
}

func (t *openAIStreamTranslator) onEvent(raw json.RawMessage) error {
	var ev struct {
		Type        string             `json:"type"`
		OutputIndex int                `json:"output_index"`
		Delta       string             `json:"delta"`
		Item        json.RawMessage    `json:"item"`
		Response    openAIResponse     `json:"response"`
		Error       openAIErrorPayload `json:"error"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	t.start()
	switch ev.Type {
	case "response.output_text.delta":
		t.openText()
		t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.blockIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Delta}})
	case "response.output_item.added":
		if item, ok := openAIOutputItemFromRaw(ev.Item); ok && item.Type == "function_call" {
			t.toolItems[ev.OutputIndex] = item
			t.startTool(ev.OutputIndex, item)
		}
	case "response.function_call_arguments.delta":
		item, ok := openAIOutputItemFromRaw(ev.Item)
		if !ok {
			item = t.toolItems[ev.OutputIndex]
		}
		t.startTool(ev.OutputIndex, item)
		t.emitToolArgDelta(ev.OutputIndex, ev.Delta)
	case "response.output_item.done":
		item, ok := openAIOutputItemFromRaw(ev.Item)
		if !ok {
			break
		}
		switch item.Type {
		case "function_call":
			t.toolItems[ev.OutputIndex] = item
			if t.startTool(ev.OutputIndex, item) && item.Arguments != "" && !t.toolArgs[ev.OutputIndex] {
				t.emitToolArgDelta(ev.OutputIndex, item.Arguments)
			}
			t.stopTool(ev.OutputIndex)
		case "reasoning":
			t.closeText()
			t.emitRedactedThinking(ev.Item)
		}
	case "response.completed":
		t.usage = anthropicUsage{InputTokens: ev.Response.Usage.InputTokens, OutputTokens: ev.Response.Usage.OutputTokens}
	case "response.incomplete":
		t.usage = anthropicUsage{InputTokens: ev.Response.Usage.InputTokens, OutputTokens: ev.Response.Usage.OutputTokens}
		t.stopReason = "max_tokens"
	case "response.failed", "error":
		message := ev.Error.Message
		if message == "" && ev.Response.Error != nil {
			message = ev.Response.Error.Message
		}
		t.emitError("api_error", message)
		return errStreamAborted
	}
	return nil
}

func openAIOutputItemFromRaw(raw json.RawMessage) (openAIOutputItem, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return openAIOutputItem{}, false
	}
	var item openAIOutputItem
	if err := json.Unmarshal(raw, &item); err != nil || item.Type == "" {
		return openAIOutputItem{}, false
	}
	return item, true
}

func (t *openAIStreamTranslator) start() {
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

func (t *openAIStreamTranslator) openText() {
	if t.textOpen {
		return
	}
	t.out.event("content_block_start", map[string]any{"type": "content_block_start", "index": t.blockIndex, "content_block": map[string]any{"type": "text", "text": ""}})
	t.textOpen = true
}

func (t *openAIStreamTranslator) closeText() {
	if !t.textOpen {
		return
	}
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blockIndex})
	t.textOpen = false
	t.blockIndex++
}

func (t *openAIStreamTranslator) startTool(outputIndex int, item openAIOutputItem) bool {
	if _, ok := t.tools[outputIndex]; ok {
		return true
	}
	if item.CallID == "" || item.Name == "" {
		return false
	}
	t.closeText()
	idx := t.blockIndex
	t.tools[outputIndex] = idx
	t.blockIndex++
	t.stopReason = "tool_use"
	t.out.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    encodeOpenAIToolUseID(item),
			"name":  item.Name,
			"input": map[string]any{},
		},
	})
	if pending := t.pendingArg[outputIndex]; len(pending) > 0 {
		delete(t.pendingArg, outputIndex)
		for _, delta := range pending {
			t.writeToolArgDelta(outputIndex, delta)
		}
	}
	return true
}

func (t *openAIStreamTranslator) emitToolArgDelta(outputIndex int, delta string) {
	if delta == "" {
		return
	}
	if _, ok := t.tools[outputIndex]; !ok {
		t.pendingArg[outputIndex] = append(t.pendingArg[outputIndex], delta)
		return
	}
	t.writeToolArgDelta(outputIndex, delta)
}

func (t *openAIStreamTranslator) writeToolArgDelta(outputIndex int, delta string) {
	t.toolArgs[outputIndex] = true
	t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.tools[outputIndex], "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
}

func (t *openAIStreamTranslator) stopTool(outputIndex int) {
	idx, ok := t.tools[outputIndex]
	if !ok {
		return
	}
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	delete(t.tools, outputIndex)
	delete(t.toolArgs, outputIndex)
	delete(t.toolItems, outputIndex)
	delete(t.pendingArg, outputIndex)
}

func (t *openAIStreamTranslator) emitRedactedThinking(raw json.RawMessage) {
	idx := t.blockIndex
	t.blockIndex++
	t.out.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": map[string]any{"type": "redacted_thinking", "data": encodeOpenAIReasoningItem(raw)},
	})
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (t *openAIStreamTranslator) emitError(errType, message string) {
	if message == "" {
		message = "OpenAI stream failed"
	}
	t.errored = true
	t.out.event("error", map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
}

func (t *openAIStreamTranslator) finish() {
	if t.errored {
		return
	}
	t.start()
	t.closeText()
	for outputIndex := range t.tools {
		t.stopTool(outputIndex)
	}
	t.out.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": t.stopReason, "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": t.usage.InputTokens, "output_tokens": t.usage.OutputTokens},
	})
	t.out.event("message_stop", map[string]any{"type": "message_stop"})
}

func readCompatSSE(body io.Reader, emit func(json.RawMessage) error) error {
	reader := bufio.NewReader(body)
	var data strings.Builder
	flush := func() error {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		return emit(json.RawMessage(payload))
	}
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ferr := flush(); ferr != nil {
				return ferr
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if err != nil {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
