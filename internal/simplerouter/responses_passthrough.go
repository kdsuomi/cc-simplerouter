package simplerouter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

type responsesPassthroughOptions struct {
	Label                string
	ProviderTag          string
	ReasoningEffortMap   map[string]string
	TranslateCustomTools bool
}

type responsesPassthroughProxy struct {
	upstreamBase   string
	model          string
	httpClient     *http.Client
	options        responsesPassthroughOptions
	jsonObjectOnly atomic.Bool
}

func startResponsesPassthroughProxy(upstreamBase, model string, httpClient *http.Client, options responsesPassthroughOptions) (baseURL string, stop func(), err error) {
	proxy := newResponsesPassthroughProxy(upstreamBase, model, httpClient, options)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: proxy}
	go func() { _ = server.Serve(listener) }()
	return fmt.Sprintf("http://%s/v1", listener.Addr().String()), func() { _ = server.Close() }, nil
}

func newResponsesPassthroughProxy(upstreamBase, model string, httpClient *http.Client, options responsesPassthroughOptions) *responsesPassthroughProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(options.Label) == "" {
		options.Label = "upstream"
	}
	return &responsesPassthroughProxy{
		upstreamBase: strings.TrimRight(upstreamBase, "/"),
		model:        model,
		httpClient:   httpClient,
		options:      options,
	}
}

func (p *responsesPassthroughProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/responses"):
		p.forwardResponses(w, r)
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{
			"id": p.model, "object": "model",
		}}})
	default:
		writeResponsesError(w, http.StatusNotFound, "not_found", "unknown route "+r.URL.Path)
	}
}

func (p *responsesPassthroughProxy) forwardResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "parse body: "+err.Error())
		return
	}
	model, _ := json.Marshal(p.model)
	request["model"] = model
	if err := rewriteResponsesReasoningEffort(request, p.options.ReasoningEffortMap); err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "rewrite reasoning effort: "+err.Error())
		return
	}
	var customTools map[string]struct{}
	if p.options.TranslateCustomTools {
		customTools, err = translateResponsesCustomTools(request)
		if err != nil {
			writeResponsesError(w, http.StatusInternalServerError, "api_error", "translate custom tools: "+err.Error())
			return
		}
	}
	if tag := strings.TrimSpace(p.options.ProviderTag); tag != "" {
		var provider map[string]any
		if raw := request["provider"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &provider)
		}
		if provider == nil {
			provider = map[string]any{}
		}
		provider["only"] = []string{tag}
		provider["allow_fallbacks"] = false
		encoded, _ := json.Marshal(provider)
		request["provider"] = encoded
	}
	if p.jsonObjectOnly.Load() {
		if fallbackPayload, changed, fallbackErr := jsonObjectFallbackPayload(request); fallbackErr != nil {
			writeResponsesError(w, http.StatusInternalServerError, "api_error", "encode JSON-object fallback: "+fallbackErr.Error())
			return
		} else if changed {
			p.forwardResponsesPayload(w, r, request, fallbackPayload, customTools)
			return
		}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "encode request: "+err.Error())
		return
	}

	p.forwardResponsesPayload(w, r, request, payload, customTools)
}

func rewriteResponsesReasoningEffort(request map[string]json.RawMessage, mapping map[string]string) error {
	if len(mapping) == 0 {
		return nil
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(request["reasoning"], &reasoning); err != nil || reasoning == nil {
		return nil
	}
	var effort string
	if err := json.Unmarshal(reasoning["effort"], &effort); err != nil {
		return nil
	}
	mapped := mappedReasoningEffort(effort, mapping)
	if mapped == "" || mapped == effort {
		return nil
	}
	encodedEffort, err := json.Marshal(mapped)
	if err != nil {
		return err
	}
	reasoning["effort"] = encodedEffort
	request["reasoning"], err = json.Marshal(reasoning)
	return err
}

func (p *responsesPassthroughProxy) forwardResponsesPayload(w http.ResponseWriter, r *http.Request, request map[string]json.RawMessage, payload []byte, customTools map[string]struct{}) {
	resp, err := p.sendResponsesRequest(r, payload)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "api_error", p.options.Label+" request failed: "+err.Error())
		return
	}
	if resp.StatusCode == http.StatusBadRequest {
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr == nil && rejectsJSONSchemaButSupportsJSONObject(errorBody) {
			p.jsonObjectOnly.Store(true)
			fallbackPayload, changed, fallbackErr := jsonObjectFallbackPayload(request)
			if fallbackErr != nil {
				writeResponsesError(w, http.StatusInternalServerError, "api_error", "encode JSON-object fallback: "+fallbackErr.Error())
				return
			}
			if changed {
				resp, err = p.sendResponsesRequest(r, fallbackPayload)
				if err != nil {
					writeResponsesError(w, http.StatusBadGateway, "api_error", p.options.Label+" request failed: "+err.Error())
					return
				}
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(errorBody))
			}
		} else {
			resp.Body = io.NopCloser(bytes.NewReader(errorBody))
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayResponsesUpstreamError(w, resp, p.options.Label)
		return
	}
	p.relayResponsesStream(w, resp, customTools)
}

func (p *responsesPassthroughProxy) sendResponsesRequest(r *http.Request, payload []byte) (*http.Response, error) {
	endpoint := p.upstreamBase
	if !strings.HasSuffix(endpoint, "/responses") {
		endpoint += "/responses"
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("Authorization", "Bearer "+apiKeyFromRequest(r))
	upReq.Header.Set("X-Title", "simplerouter")

	return p.httpClient.Do(upReq)
}

func rejectsJSONSchemaButSupportsJSONObject(body []byte) bool {
	message := strings.ToLower(string(body))
	return strings.Contains(message, "json_schema") &&
		strings.Contains(message, "json_object") &&
		(strings.Contains(message, "does not support") || strings.Contains(message, "unsupported"))
}

func jsonObjectFallbackPayload(request map[string]json.RawMessage) ([]byte, bool, error) {
	var textControls map[string]json.RawMessage
	if err := json.Unmarshal(request["text"], &textControls); err != nil {
		return nil, false, nil
	}
	var format map[string]json.RawMessage
	if err := json.Unmarshal(textControls["format"], &format); err != nil {
		return nil, false, nil
	}
	var formatType string
	if err := json.Unmarshal(format["type"], &formatType); err != nil || formatType != "json_schema" {
		return nil, false, nil
	}

	schema := append(json.RawMessage(nil), format["schema"]...)
	textControls["format"] = json.RawMessage(`{"type":"json_object"}`)
	encodedText, err := json.Marshal(textControls)
	if err != nil {
		return nil, false, err
	}
	request["text"] = encodedText

	if len(schema) > 0 && json.Valid(schema) {
		var instructions string
		if raw := request["instructions"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &instructions)
		}
		instructions += "\n\nThe final response must be a JSON object that matches this JSON Schema exactly:\n" + string(schema)
		request["instructions"], err = json.Marshal(instructions)
		if err != nil {
			return nil, false, err
		}
	}

	payload, err := json.Marshal(request)
	return payload, true, err
}

func (p *responsesPassthroughProxy) relayResponsesStream(w http.ResponseWriter, resp *http.Response, customTools map[string]struct{}) {
	for _, name := range []string{
		"Content-Type",
		"Cache-Control",
		"OpenAI-Request-ID",
		"X-Request-ID",
		"X-Generation-ID",
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
	} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	writeBlocks := func(blocks [][]byte) bool {
		for _, block := range blocks {
			if len(block) == 0 {
				continue
			}
			if _, err := w.Write(block); err != nil {
				return false
			}
		}
		if len(blocks) > 0 && flusher != nil {
			flusher.Flush()
		}
		return true
	}
	filter := newReasoningReplayFilter()
	customToolTranslator := newResponsesCustomToolStreamTranslator(customTools)
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	var block []byte
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			block = append(block, line...)
			if isBlankSSELine(line) {
				for _, translated := range customToolTranslator.processBlock(block) {
					if !writeBlocks(filter.processBlock(translated)) {
						return
					}
				}
				block = nil
			}
		}
		if readErr != nil {
			if len(block) > 0 {
				for _, translated := range customToolTranslator.processBlock(block) {
					if !writeBlocks(filter.processBlock(translated)) {
						return
					}
				}
			}
			writeBlocks(filter.finish())
			return
		}
	}
}

func isBlankSSELine(line []byte) bool {
	trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	return len(trimmed) == 0
}
