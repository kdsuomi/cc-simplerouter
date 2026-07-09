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
	"strings"
	"time"
)

// openRouterThinkingSignature marks thinking blocks minted by this proxy so
// request translation can recognize them when Claude Code sends them back.
const openRouterThinkingSignature = "openrouter#proxy"

// openRouterReasoningDataPrefix marks redacted_thinking payloads that carry
// an OpenRouter reasoning_details array for verbatim round-tripping.
const openRouterReasoningDataPrefix = "orrd1:"

// openRouterProxyOptions carries per-launch settings into the proxy.
type openRouterProxyOptions struct {
	// ProviderTag pins every request to one OpenRouter provider endpoint via
	// provider.only (empty = let OpenRouter route).
	ProviderTag string
	// DisableThinking suppresses the reasoning parameter entirely.
	DisableThinking bool
	// SupportsReasoning reports whether the selected model advertises the
	// "reasoning" parameter; when false the parameter is never sent, since
	// OpenRouter rejects reasoning requests for models with no such endpoint.
	SupportsReasoning bool
}

// startOpenRouterProxy launches a localhost proxy that translates the
// Anthropic Messages API to OpenRouter's chat completions API. Unlike
// OpenRouter's native Anthropic-compatible endpoint, chat completions
// exposes reasoning as it streams (delta.reasoning / reasoning_details),
// which the proxy forwards as live thinking_delta events.
func startOpenRouterProxy(upstreamBase, model string, httpClient *http.Client, opts openRouterProxyOptions) (baseURL string, stop func(), err error) {
	p := newOpenRouterProxy(upstreamBase, model, httpClient, opts)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: p}
	go server.Serve(listener)
	return fmt.Sprintf("http://%s", listener.Addr().String()), func() { _ = server.Close() }, nil
}

type openRouterProxy struct {
	upstreamBase string
	model        string
	httpClient   *http.Client
	opts         openRouterProxyOptions
}

func newOpenRouterProxy(upstreamBase, model string, httpClient *http.Client, opts openRouterProxyOptions) *openRouterProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(upstreamBase) == "" {
		upstreamBase = defaultOpenRouterAPIBase
	}
	return &openRouterProxy{
		upstreamBase: strings.TrimRight(upstreamBase, "/"),
		model:        model,
		httpClient:   httpClient,
		opts:         opts,
	}
}

func (p *openRouterProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (p *openRouterProxy) handleMessages(w http.ResponseWriter, r *http.Request) {
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
	payload, err := anthropicToOpenRouterChat(&req, p.opts)
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
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "OpenRouter request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayCompatUpstreamError(w, resp, "OpenRouter")
		return
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		tr := newOpenRouterStreamTranslator(w, flusher, req.Model)
		if err := readCompatSSE(resp.Body, tr.onEvent); err != nil && !errors.Is(err, errStreamAborted) {
			tr.emitError("api_error", err.Error())
		}
		tr.finish()
		return
	}
	var out openRouterChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode OpenRouter response: "+err.Error())
		return
	}
	if out.Error != nil {
		status, errType := anthropicErrorForStatus(out.Error.status())
		writeAnthropicError(w, status, errType, out.Error.Message)
		return
	}
	writeJSON(w, http.StatusOK, openRouterToAnthropic(&out, req.Model))
}

// OpenRouter chat completions wire format (the subset the proxy translates).

type openRouterChatResponse struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []openRouterChoice   `json:"choices"`
	Usage   *openRouterUsage     `json:"usage"`
	Error   *openRouterErrorBody `json:"error"`
}

type openRouterErrorBody struct {
	Code    json.RawMessage `json:"code"` // number or string depending on provider
	Message string          `json:"message"`
}

// status maps the error's code onto an HTTP status when it looks like one.
func (e *openRouterErrorBody) status() int {
	var code int
	if e != nil && json.Unmarshal(e.Code, &code) == nil && code >= 400 && code < 600 {
		return code
	}
	return http.StatusBadGateway
}

type openRouterChoice struct {
	Message      openRouterChatMessage `json:"message"`
	Delta        openRouterChatMessage `json:"delta"`
	FinishReason string                `json:"finish_reason"`
}

type openRouterChatMessage struct {
	Role             string                      `json:"role,omitempty"`
	Content          string                      `json:"content,omitempty"`
	Reasoning        string                      `json:"reasoning,omitempty"`
	ReasoningDetails []openRouterReasoningDetail `json:"reasoning_details,omitempty"`
	ToolCalls        []openRouterToolCall        `json:"tool_calls,omitempty"`
}

type openRouterToolCall struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openRouterToolFunction `json:"function"`
}

type openRouterToolFunction struct {
	Name      string            `json:"name,omitempty"`
	Arguments chatToolArguments `json:"arguments,omitempty"`
}

// openRouterReasoningDetail is one reasoning_details entry
// (reasoning.text / reasoning.summary / reasoning.encrypted).
type openRouterReasoningDetail struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        json.RawMessage `json:"id,omitempty"`
	Format    string          `json:"format,omitempty"`
	Index     *int            `json:"index,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type openRouterUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
}

func anthropicUsageFromOpenRouter(u *openRouterUsage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	cached, written := 0, 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
		written = u.PromptTokensDetails.CacheWriteTokens
	}
	// OpenRouter reports prompt_tokens OpenAI-style (cache reads and writes
	// included); Anthropic's input_tokens excludes both.
	input := u.PromptTokens - cached - written
	if input < 0 {
		input = 0
	}
	return anthropicUsage{
		InputTokens:              input,
		OutputTokens:             u.CompletionTokens,
		CacheReadInputTokens:     cached,
		CacheCreationInputTokens: written,
	}
}

// anthropicToOpenRouterChat translates an Anthropic Messages request into an
// OpenRouter chat completions payload.
func anthropicToOpenRouterChat(req *anthropicRequest, opts openRouterProxyOptions) ([]byte, error) {
	messages, sawCacheControl, err := openRouterMessagesFromAnthropic(req)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":    openRouterModelID(req.Model),
		"messages": messages,
		"stream":   req.Stream,
		// Usage accounting: the final stream chunk then carries token counts
		// including cache reads/writes.
		"usage": map[string]any{"include": true},
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	tools, err := openRouterToolsForChoice(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := openRouterToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	if reasoning := openRouterReasoningParam(req.Thinking, opts); reasoning != nil {
		out["reasoning"] = reasoning
	}
	if opts.ProviderTag != "" {
		out["provider"] = map[string]any{"only": []string{opts.ProviderTag}, "allow_fallbacks": false}
	}
	if !sawCacheControl {
		// No explicit breakpoints from the client: ask OpenRouter to place an
		// automatic cache breakpoint (honored by Anthropic/Vertex/Azure).
		out["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return json.Marshal(out)
}

// openRouterReasoningParam maps Anthropic's thinking config onto OpenRouter's
// unified reasoning parameter. OpenRouter converts max_tokens to an effort
// level (and vice versa) for models that only support one of the two.
func openRouterReasoningParam(th *anthropicThinking, opts openRouterProxyOptions) map[string]any {
	if opts.DisableThinking || !opts.SupportsReasoning || th == nil {
		return nil
	}
	switch th.Type {
	case "enabled", "adaptive":
		if th.BudgetTokens > 0 {
			return map[string]any{"max_tokens": th.BudgetTokens}
		}
		return map[string]any{"enabled": true}
	default: // "disabled" or unknown future types: leave the model default
		return nil
	}
}

func openRouterModelID(model string) string {
	return strings.TrimSuffix(strings.TrimSpace(model), "[1m]")
}

func openRouterMessagesFromAnthropic(req *anthropicRequest) (messages []map[string]any, sawCacheControl bool, err error) {
	appendMsg := func(m map[string]any) { messages = append(messages, m) }

	if sysBlocks, err := blocksFromContent(req.System); err == nil && len(sysBlocks) > 0 {
		if content, sawCache := openRouterTextContent(sysBlocks); content != nil {
			sawCacheControl = sawCacheControl || sawCache
			appendMsg(map[string]any{"role": "system", "content": content})
		}
	}

	for i, msg := range req.Messages {
		blocks, err := blocksFromContent(msg.Content)
		if err != nil {
			return nil, false, fmt.Errorf("messages[%d]: unparseable content: %w", i, err)
		}
		switch msg.Role {
		case "assistant":
			appendMsg(openRouterAssistantMessage(blocks))
		default:
			sawCache := openRouterUserMessages(blocks, appendMsg)
			sawCacheControl = sawCacheControl || sawCache
		}
	}
	return messages, sawCacheControl, nil
}

// openRouterTextContent renders text blocks as a plain string, or as content
// parts when any block carries a cache_control breakpoint that must survive.
func openRouterTextContent(blocks []anthropicBlock) (content any, sawCacheControl bool) {
	var parts []map[string]any
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		part := map[string]any{"type": "text", "text": b.Text}
		if len(b.CacheControl) > 0 {
			part["cache_control"] = b.CacheControl
			sawCacheControl = true
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, false
	}
	if !sawCacheControl {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			texts = append(texts, p["text"].(string))
		}
		return strings.Join(texts, "\n\n"), false
	}
	return parts, true
}

func openRouterAssistantMessage(blocks []anthropicBlock) map[string]any {
	assistant := map[string]any{"role": "assistant"}
	var texts, thoughts []string
	var calls []map[string]any
	var details []json.RawMessage
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
		case "redacted_thinking":
			details = append(details, decodeOpenRouterReasoningDetails(b.Data)...)
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
	switch {
	case len(details) > 0:
		// The canonical reasoning (with signatures / encrypted payloads) came
		// back via reasoning_details; pass it through verbatim and drop the
		// display-only thinking text so it isn't duplicated.
		assistant["reasoning_details"] = details
	case len(thoughts) > 0:
		assistant["reasoning"] = joinThinkingChunks(thoughts)
	}
	if len(calls) > 0 {
		assistant["tool_calls"] = calls
	}
	return assistant
}

// openRouterUserMessages emits user-role content, splitting tool results into
// their own tool-role messages as the chat completions API requires.
func openRouterUserMessages(blocks []anthropicBlock, appendMsg func(map[string]any)) (sawCacheControl bool) {
	var parts []map[string]any
	hasMultimodal := false
	flushUser := func() {
		if len(parts) == 0 {
			return
		}
		if !hasMultimodal && !partsHaveCacheControl(parts) {
			texts := make([]string, 0, len(parts))
			for _, p := range parts {
				texts = append(texts, p["text"].(string))
			}
			appendMsg(map[string]any{"role": "user", "content": strings.Join(texts, "\n\n")})
		} else {
			appendMsg(map[string]any{"role": "user", "content": parts})
		}
		parts = nil
		hasMultimodal = false
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			part := map[string]any{"type": "text", "text": b.Text}
			if len(b.CacheControl) > 0 {
				part["cache_control"] = b.CacheControl
				sawCacheControl = true
			}
			parts = append(parts, part)
		case "image":
			if url := openRouterImageURL(b); url != "" {
				hasMultimodal = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "tool_result":
			flushUser()
			tool := map[string]any{"role": "tool", "tool_call_id": b.ToolUseID}
			if len(b.CacheControl) > 0 {
				sawCacheControl = true
				tool["content"] = []map[string]any{{
					"type":          "text",
					"text":          toolResultText(b),
					"cache_control": b.CacheControl,
				}}
			} else {
				tool["content"] = toolResultText(b)
			}
			appendMsg(tool)
		}
	}
	flushUser()
	return sawCacheControl
}

func partsHaveCacheControl(parts []map[string]any) bool {
	for _, p := range parts {
		if _, ok := p["cache_control"]; ok {
			return true
		}
	}
	return false
}

func openRouterImageURL(b anthropicBlock) string {
	if b.Source == nil || b.Source.Type != "base64" || b.Source.Data == "" {
		return ""
	}
	mediaType := b.Source.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + b.Source.Data
}

func openRouterToolsForChoice(tools []anthropicTool, tc *anthropicToolChoice) ([]map[string]any, error) {
	if tc != nil {
		switch tc.Type {
		case "auto", "any", "none", "tool":
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", tc.Type)
		}
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "custom" {
			continue // server tool stubs the upstream can't execute
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  jsonRawOrObject(t.InputSchema),
			},
		})
	}
	return out, nil
}

func openRouterToolChoice(tc *anthropicToolChoice) any {
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
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return nil
	}
}

// mergeReasoningDetails folds a chunk's reasoning_details deltas into the
// accumulated array: consecutive entries of the same type and index are one
// logical entry whose text/summary/data streams in pieces.
func mergeReasoningDetails(acc, incoming []openRouterReasoningDetail) []openRouterReasoningDetail {
	for _, d := range incoming {
		if n := len(acc); n > 0 {
			last := &acc[n-1]
			if last.Type == d.Type && sameDetailIndex(last.Index, d.Index) {
				last.Text += d.Text
				last.Summary += d.Summary
				last.Data += d.Data
				if d.Signature != "" {
					last.Signature = d.Signature
				}
				if len(d.ID) > 0 && !bytes.Equal(bytes.TrimSpace(d.ID), []byte("null")) {
					last.ID = d.ID
				}
				if d.Format != "" {
					last.Format = d.Format
				}
				continue
			}
		}
		acc = append(acc, d)
	}
	return acc
}

func sameDetailIndex(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func textFromReasoningDetails(details []openRouterReasoningDetail) string {
	var texts []string
	for _, d := range details {
		switch {
		case d.Text != "":
			texts = append(texts, d.Text)
		case d.Summary != "":
			texts = append(texts, d.Summary)
		}
	}
	return joinThinkingChunks(texts)
}

// encodeOpenRouterReasoningDetails packs the accumulated reasoning_details
// array into a redacted_thinking data payload. Claude Code preserves
// redacted_thinking blocks verbatim, so the details survive the round trip
// back into the next request (signatures and encrypted reasoning included).
func encodeOpenRouterReasoningDetails(details []openRouterReasoningDetail) string {
	data, err := json.Marshal(details)
	if err != nil {
		return ""
	}
	return openRouterReasoningDataPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func decodeOpenRouterReasoningDetails(data string) []json.RawMessage {
	enc, ok := strings.CutPrefix(strings.TrimSpace(data), openRouterReasoningDataPrefix)
	if !ok {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	for _, item := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &probe) != nil || !strings.HasPrefix(probe.Type, "reasoning.") {
			return nil
		}
	}
	return items
}

// openRouterToAnthropic converts a non-streaming chat completion into an
// Anthropic Messages response.
func openRouterToAnthropic(resp *openRouterChatResponse, fallbackModel string) *anthropicMessageResponse {
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
		Usage:   anthropicUsageFromOpenRouter(resp.Usage),
	}
	if len(resp.Choices) == 0 {
		out.StopReason = "end_turn"
		return out
	}
	msg := resp.Choices[0].Message
	thinking := msg.Reasoning
	if thinking == "" {
		thinking = textFromReasoningDetails(msg.ReasoningDetails)
	}
	if thinking != "" {
		out.Content = append(out.Content, map[string]any{"type": "thinking", "thinking": thinking, "signature": openRouterThinkingSignature})
	}
	if len(msg.ReasoningDetails) > 0 {
		if data := encodeOpenRouterReasoningDetails(msg.ReasoningDetails); data != "" {
			out.Content = append(out.Content, map[string]any{"type": "redacted_thinking", "data": data})
		}
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
	out.StopReason = openRouterStopReason(resp.Choices[0].FinishReason, len(msg.ToolCalls) > 0)
	return out
}

func openRouterStopReason(finish string, sawTool bool) string {
	if sawTool || finish == "tool_calls" {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// openRouterStreamTranslator converts chat completion chunks into Anthropic
// SSE events, streaming reasoning live through a thinkingStreamer.
type openRouterStreamTranslator struct {
	out        *sseWriter
	model      string
	msgID      string
	started    bool
	blockIndex int
	blockOpen  bool
	blockType  string
	thinking   *thinkingStreamer
	details    []openRouterReasoningDetail
	// detailsEmitted counts entries of details already written out as a
	// redacted_thinking block.
	detailsEmitted int
	// visibleSource latches which channel renders thinking text — the
	// reasoning string or reasoning_details — since providers may carry the
	// same text on both and displaying both would duplicate it.
	visibleSource string
	tools         map[int]*openRouterToolStreamState
	stopReason    string
	usage         anthropicUsage
	errored       bool
}

type openRouterToolStreamState struct {
	id         string
	name       string
	blockIndex int
	open       bool
	pending    []string
}

func newOpenRouterStreamTranslator(w io.Writer, flush http.Flusher, model string) *openRouterStreamTranslator {
	t := &openRouterStreamTranslator{
		out:        &sseWriter{w: w, flush: flush},
		model:      model,
		msgID:      newMessageID(),
		tools:      map[int]*openRouterToolStreamState{},
		stopReason: "end_turn",
	}
	t.thinking = &thinkingStreamer{
		open:        func() { t.openBlock("thinking") },
		closeCurr:   func() { t.closeBlock() },
		emit:        func(s string) { t.emitDelta(map[string]any{"type": "thinking_delta", "thinking": s}) },
		isOpen:      func() bool { return t.blockOpen && t.blockType == "thinking" },
		rotateEvery: thinkingRotateEvery,
		minChars:    thinkingRotateMinChars,
		now:         time.Now,
	}
	return t
}

func (t *openRouterStreamTranslator) onEvent(raw json.RawMessage) error {
	var chunk openRouterChatResponse
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return err
	}
	if chunk.Error != nil {
		t.emitError("api_error", chunk.Error.Message)
		return errStreamAborted
	}
	t.start()
	if chunk.Usage != nil {
		t.usage = anthropicUsageFromOpenRouter(chunk.Usage)
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason == "error" {
			t.emitError("api_error", "OpenRouter stream finished with an error")
			return errStreamAborted
		}
		if ch.FinishReason != "" {
			t.stopReason = openRouterStopReason(ch.FinishReason, t.stopReason == "tool_use")
		}
		delta := ch.Delta
		if len(delta.ReasoningDetails) > 0 {
			t.details = mergeReasoningDetails(t.details, delta.ReasoningDetails)
		}
		if delta.Reasoning != "" {
			if t.visibleSource == "" {
				t.visibleSource = "reasoning"
			}
			if t.visibleSource == "reasoning" {
				t.thinking.add(delta.Reasoning)
			}
		} else if text := textFromReasoningDetails(delta.ReasoningDetails); text != "" {
			if t.visibleSource == "" {
				t.visibleSource = "details"
			}
			if t.visibleSource == "details" {
				t.thinking.add(text)
			}
		}
		if delta.Content != "" {
			t.thinking.flush()
			t.emitReasoningDetails()
			t.openBlock("text")
			t.emitDelta(map[string]any{"type": "text_delta", "text": delta.Content})
		}
		for _, call := range delta.ToolCalls {
			t.onToolDelta(call)
		}
	}
	return nil
}

func (t *openRouterStreamTranslator) start() {
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

func (t *openRouterStreamTranslator) emitDelta(delta map[string]any) {
	t.out.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.blockIndex, "delta": delta})
}

func (t *openRouterStreamTranslator) openBlock(blockType string) {
	if t.blockOpen && t.blockType == blockType {
		return
	}
	t.closeBlock()
	t.blockType = blockType
	t.blockOpen = true
	block := map[string]any{"type": blockType}
	if blockType == "thinking" {
		block["thinking"] = ""
		// Provide the placeholder signature up front so clients can render
		// thinking deltas as they arrive instead of buffering to block close.
		block["signature"] = openRouterThinkingSignature
	} else {
		block["text"] = ""
	}
	t.out.event("content_block_start", map[string]any{"type": "content_block_start", "index": t.blockIndex, "content_block": block})
}

func (t *openRouterStreamTranslator) closeBlock() {
	if !t.blockOpen {
		return
	}
	if t.blockType == "thinking" {
		t.emitDelta(map[string]any{"type": "signature_delta", "signature": openRouterThinkingSignature})
	}
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blockIndex})
	t.blockOpen = false
	t.blockIndex++
}

func (t *openRouterStreamTranslator) onToolDelta(call openRouterToolCall) {
	st := t.tools[call.Index]
	if st == nil {
		st = &openRouterToolStreamState{}
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
		t.thinking.flush()
		t.emitReasoningDetails()
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

func (t *openRouterStreamTranslator) emitError(errType, message string) {
	if message == "" {
		message = "OpenRouter stream failed"
	}
	t.errored = true
	t.out.event("error", map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
}

// emitReasoningDetails writes accumulated reasoning_details out as a
// redacted_thinking block. It runs at the reasoning->content transition so
// the block precedes text and tool blocks (matching native Anthropic block
// order — clients may take the last block as the answer), with a finish()
// backstop for details that trail the content.
func (t *openRouterStreamTranslator) emitReasoningDetails() {
	if len(t.details) <= t.detailsEmitted {
		return
	}
	data := encodeOpenRouterReasoningDetails(t.details[t.detailsEmitted:])
	t.detailsEmitted = len(t.details)
	if data == "" {
		return
	}
	t.closeBlock()
	idx := t.blockIndex
	t.blockIndex++
	t.out.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": map[string]any{"type": "redacted_thinking", "data": data},
	})
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (t *openRouterStreamTranslator) finish() {
	if t.errored {
		return
	}
	t.start()
	t.thinking.flush()
	t.closeBlock()
	for _, st := range t.tools {
		if st.open {
			t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.blockIndex})
		}
	}
	t.emitReasoningDetails()
	t.out.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": t.stopReason, "stop_sequence": nil},
		"usage": map[string]int{
			"input_tokens":                t.usage.InputTokens,
			"output_tokens":               t.usage.OutputTokens,
			"cache_read_input_tokens":     t.usage.CacheReadInputTokens,
			"cache_creation_input_tokens": t.usage.CacheCreationInputTokens,
		},
	})
	t.out.event("message_stop", map[string]any{"type": "message_stop"})
}
